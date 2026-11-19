package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestOSSPresignedDownloadAndRedirectRejection(t *testing.T) {
	const query = "x-oss-signature=test%2Bsignature&x-oss-security-token=test-token"
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/"+testJARName+"?"+query, http.StatusFound)
			return
		}
		if r.URL.Path != "/"+testJARName || r.Header.Get("Authorization") != "" || r.URL.RawQuery != query {
			http.Error(w, "invalid presigned URL", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "artifact")
	}))
	defer source.Close()

	artifact, err := loadOSSArtifact(map[string]string{
		"OSS_ARTIFACT_URL":    source.URL + "/" + testJARName + "?" + query,
		"OSS_ARTIFACT_SHA256": fmt.Sprintf("%064x", 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := outboundClient(time.Second)
	client.Transport = source.Client().Transport
	result, err := artifact.download(t.Context(), client, maxJARSize)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	body, err := io.ReadAll(result)
	if err != nil || string(body) != "artifact" {
		t.Fatalf("download = %q, %v", body, err)
	}
	artifact.url = source.URL + "/redirect?" + query
	if _, err := artifact.download(t.Context(), client, maxJARSize); err == nil ||
		!strings.Contains(err.Error(), "HTTP 302") || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("unexpected redirect response: %v", err)
	}
}

func TestBearerOSSDeploymentStreamsVerifiedJARToProduction(t *testing.T) {
	data := makeTestJAR(t)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oss-token" {
			t.Error("missing OSS bearer token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(data)
	}))
	defer source.Close()
	digest := sha256.Sum256(data)
	artifact, err := loadOSSArtifact(map[string]string{
		"OSS_ARTIFACT_URL": source.URL + "/" + testJARName, "OSS_ARTIFACT_SHA256": fmt.Sprintf("%x", digest),
		"OSS_ACCESS_TOKEN": "oss-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{NoClientAuth: true}
	server.AddHostKey(key)
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	result := make(chan error, 1)
	go func() {
		err := expectSSHCommand(listener, server, func(command string, body []byte) error {
			if !strings.Contains(command, "cat > ") || strings.Contains(command, "oss-token") ||
				!strings.Contains(command, fmt.Sprintf("%x", digest)) || !bytes.Equal(body, data) {
				return fmt.Errorf("unexpected SSH transfer")
			}
			return nil
		})
		if err != nil {
			_ = listener.Close()
			result <- err
			return
		}
		result <- expectSSHCommand(listener, server, func(command string, body []byte) error {
			if !strings.Contains(command, "photolib-backend-oss-") || string(body) != restartScript {
				return fmt.Errorf("unexpected SSH activation")
			}
			return nil
		})
	}()

	p := productionServer{address: listener.Addr().String(), hostKey: key.PublicKey(), user: "root"}
	s := &relay{maxSize: maxJARSize, logger: slog.New(slog.DiscardHandler), history: history(t.TempDir()), deploy: p.deploy}
	w := httptest.NewRecorder()
	user := consoleIdentity{administrator: administrator{ID: "27", Username: "member"}, Allowed: true}
	s.handleOSS(artifact, source.Client())(w, httptest.NewRequest(http.MethodPost, "/api/deployments/oss", nil), user)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	records, err := s.history.list()
	if w.Code != http.StatusNoContent || err != nil || len(records) != 1 || records[0].Status != "succeeded" ||
		records[0].Kind != "oss" || records[0].Archived || records[0].Operator != user.administrator {
		t.Fatalf("unexpected manual deployment result: %d %v %v", w.Code, records, err)
	}
}
