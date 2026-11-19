package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
)

var (
	jarNamePattern = regexp.MustCompile(`^photolib-backend-[A-Za-z0-9][A-Za-z0-9._+-]{0,159}\.jar$`)
	runIDPattern   = regexp.MustCompile(`^[0-9]{1,20}$`)
)

type artifact struct {
	name   string
	sha256 string
	file   *os.File
	size   int64
}

type relay struct {
	token   string
	maxSize int64
	logger  *slog.Logger
	deploy  func(context.Context, artifact, string, *slog.Logger) error
	mu      sync.Mutex
}

func (s *relay) handleDeploy(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	presented, expected := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.token))
	if !ok || subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "invalid deployment token", http.StatusUnauthorized)
		return
	}
	name := r.Header.Get("X-Artifact-Name")
	runID := r.Header.Get("X-Deploy-Run-ID")
	expectedSum, err := hex.DecodeString(r.Header.Get("X-Artifact-SHA256"))
	if !jarNamePattern.MatchString(name) || !runIDPattern.MatchString(runID) || err != nil || len(expectedSum) != sha256.Size {
		http.Error(w, "invalid artifact name, SHA-256, or workflow run ID", http.StatusBadRequest)
		return
	}
	if r.ContentLength > s.maxSize {
		http.Error(w, "JAR exceeds upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	if !s.mu.TryLock() {
		http.Error(w, "another deployment is in progress", http.StatusConflict)
		return
	}
	defer s.mu.Unlock()
	logger := s.logger.With("run_id", runID, "artifact", name)

	jar, stageErr := stageArtifact(http.MaxBytesReader(w, r.Body, s.maxSize), name, expectedSum, s.maxSize, logger)
	if stageErr != nil {
		http.Error(w, stageErr.Error(), stageErr.status)
		return
	}
	defer jar.remove(logger)
	if r.Context().Err() != nil {
		return
	}
	if err := s.deployStaged(r.Context(), jar, runID, logger); err != nil {
		http.Error(w, "production deployment failed; check relay logs for this run ID", http.StatusBadGateway)
		return
	}
	_, _ = io.WriteString(w, "PhotoLib deployed\n")
}

type stagingError struct {
	message string
	status  int
}

func (e *stagingError) Error() string { return e.message }

func stageArtifact(source io.Reader, name string, expectedSum []byte, maxSize int64, logger *slog.Logger) (artifact, *stagingError) {
	file, err := os.CreateTemp("", "deplyt-*.jar")
	if err != nil {
		logger.Error("create temporary file", "error", err)
		return artifact{}, &stagingError{"cannot stage JAR on relay", http.StatusInternalServerError}
	}
	jar := artifact{name: name, file: file}
	accepted := false
	defer func() {
		if !accepted {
			jar.remove(logger)
		}
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, maxSize+1))
	var tooLarge *http.MaxBytesError
	if size > maxSize || errors.As(err, &tooLarge) {
		return artifact{}, &stagingError{"JAR exceeds upload limit", http.StatusRequestEntityTooLarge}
	}
	if err != nil {
		var fileError *os.PathError
		if errors.As(err, &fileError) {
			logger.Error("write temporary JAR", "error", err)
			return artifact{}, &stagingError{"cannot stage JAR on relay", http.StatusInternalServerError}
		}
		return artifact{}, &stagingError{"incomplete JAR transfer", http.StatusBadRequest}
	}
	actualSum := hash.Sum(nil)
	if subtle.ConstantTimeCompare(actualSum, expectedSum) != 1 {
		return artifact{}, &stagingError{"JAR SHA-256 mismatch", http.StatusUnprocessableEntity}
	}
	if err := validateJAR(file, size); err != nil {
		logger.Warn("invalid PhotoLib JAR", "error", err)
		return artifact{}, &stagingError{"invalid PhotoLib JAR", http.StatusUnprocessableEntity}
	}
	jar.sha256 = hex.EncodeToString(actualSum)
	jar.size = size
	accepted = true
	return jar, nil
}

func (a artifact) remove(logger *slog.Logger) {
	_ = a.file.Close()
	if err := os.Remove(a.file.Name()); err != nil {
		logger.Warn("remove temporary JAR", "error", err)
	}
}

func (s *relay) deployStaged(ctx context.Context, jar artifact, runID string, logger *slog.Logger) error {
	// Once accepted, deployment must finish even if the caller disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployTimeout)
	defer cancel()
	stageID := runID + "-" + rand.Text()
	logger.Info("accepted PhotoLib JAR", "bytes", jar.size, "sha256", jar.sha256, "stage_id", stageID)
	if err := s.deploy(ctx, jar, stageID, logger); err != nil {
		logger.Error("production deployment failed", "error", err)
		return err
	}
	logger.Info("PhotoLib deployment succeeded")
	return nil
}

func validateJAR(file io.ReaderAt, size int64) error {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return err
	}
	// Match PhotoLib CI's artifact checks and require a JAR manifest.
	required := map[string]bool{
		"META-INF/MANIFEST.MF":                                      false,
		"BOOT-INF/classes/static/index.html":                        false,
		"BOOT-INF/classes/native/linux-x86_64/libphotolib-image.so": false,
		"BOOT-INF/classes/native/windows-x86_64/photolib-image.dll": false,
	}
	for _, entry := range reader.File {
		if seen, needed := required[entry.Name]; needed {
			if seen || !entry.Mode().IsRegular() || entry.UncompressedSize64 == 0 {
				return fmt.Errorf("invalid JAR entry %s", entry.Name)
			}
			required[entry.Name] = true
		}
	}
	for name, seen := range required {
		if !seen {
			return fmt.Errorf("missing JAR entry %s", name)
		}
	}
	return nil
}
