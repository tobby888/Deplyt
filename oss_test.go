package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
