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
	"time"
)

var (
	jarNamePattern = regexp.MustCompile(`^photolib-backend-[A-Za-z0-9][A-Za-z0-9._+-]{0,159}\.jar$`)
	runIDPattern   = regexp.MustCompile(`^[0-9]{1,20}$`)
)

type artifact struct {
	name   string
	sha256 string
	key    string
}

type relay struct {
	token       string
	maxSize     int64
	logger      *slog.Logger
	archive     func(context.Context, string, string, *os.File) error
	downloadURL func(context.Context, string) (string, error)
	deploy      func(context.Context, artifact, string, string, *slog.Logger) error
	mu          sync.Mutex
	history     history
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
	commit := r.Header.Get("X-Deploy-Commit")
	if commit != "" && !commitPattern.MatchString(commit) {
		http.Error(w, "invalid commit SHA", http.StatusBadRequest)
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

	file, err := os.CreateTemp("", "deplyt-*.jar")
	if err != nil {
		logger.Error("create temporary file", "error", err)
		http.Error(w, "cannot stage JAR on relay", http.StatusInternalServerError)
		return
	}
	defer func() {
		_ = file.Close()
		if err := os.Remove(file.Name()); err != nil {
			logger.Warn("remove temporary JAR", "error", err)
		}
	}()

	hash := sha256.New()
	// Hash the JAR as it streams to disk.
	size, err := io.Copy(io.MultiWriter(file, hash), http.MaxBytesReader(w, r.Body, s.maxSize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var fileError *os.PathError
		switch {
		case errors.As(err, &tooLarge):
			http.Error(w, "JAR exceeds upload limit", http.StatusRequestEntityTooLarge)
		case errors.As(err, &fileError):
			logger.Error("write temporary JAR", "error", err)
			http.Error(w, "cannot stage JAR on relay", http.StatusInternalServerError)
		default:
			logger.Warn("read JAR upload", "error", err)
			http.Error(w, "incomplete JAR upload", http.StatusBadRequest)
		}
		return
	}
	actualSum := hash.Sum(nil)
	if subtle.ConstantTimeCompare(actualSum, expectedSum) != 1 {
		http.Error(w, "JAR SHA-256 mismatch", http.StatusUnprocessableEntity)
		return
	}
	if err := validateJAR(file, size); err != nil {
		logger.Warn("invalid PhotoLib JAR", "error", err)
		http.Error(w, "invalid PhotoLib JAR", http.StatusUnprocessableEntity)
		return
	}
	if r.Context().Err() != nil {
		return
	}

	// Once accepted, deployment must finish even if the runner disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deployTimeout)
	defer cancel()
	id := strings.ToLower(rand.Text())
	sum := hex.EncodeToString(actualSum)
	archiveName := fmt.Sprintf("photolib-backend-%s-%s-%s.jar", runID, id, sum)
	record := deployment{
		ID: id, Kind: "deploy", Status: "running", Run: runID, Commit: strings.ToLower(commit),
		Name: archiveName, OriginalName: name, Object: archivePrefix + archiveName,
		SHA256: sum, Size: size, Started: time.Now().UTC(),
	}
	logger = logger.With("deployment_id", id, "object", record.Object)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		logger.Error("rewind staged JAR", "error", err)
		http.Error(w, "cannot stage JAR on relay", http.StatusInternalServerError)
		return
	}
	if err := s.history.save(record); err != nil {
		logger.Error("create deployment record", "error", err)
		http.Error(w, "cannot save deployment record", http.StatusInternalServerError)
		return
	}
	if err := s.archive(ctx, record.Object, sum, file); err != nil {
		logger.Error("archive JAR in OSS", "error", err)
		if err := s.history.finish(record, "failed", "OSS 归档未获确认"); err != nil {
			logger.Error("save archive failure", "error", err)
		}
		http.Error(w, "cannot archive JAR in OSS", http.StatusBadGateway)
		return
	}
	record.Archived = true
	if err := s.history.save(record); err != nil {
		logger.Error("record OSS archive", "error", err)
		http.Error(w, "cannot save archive status", http.StatusInternalServerError)
		return
	}
	if !s.apply(w, ctx, record) {
		return
	}
	_, _ = io.WriteString(w, "PhotoLib deployed\n")
}

func (s *relay) activate(ctx context.Context, jar artifact, stageID string, logger *slog.Logger) error {
	downloadURL, err := s.downloadURL(ctx, jar.key)
	if err != nil {
		return errors.New("cannot sign OSS download")
	}
	return s.deploy(ctx, jar, stageID, downloadURL, logger)
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
