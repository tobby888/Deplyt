package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeplytUsesEffectivePhotoLibBrandIcon(t *testing.T) {
	var upstreamIconPath, requestedIcon string
	scheduled := builtinBrandImage("star").body
	custom := builtinBrandImage("picture").body
	photoLib := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/branding":
			data := map[string]string{"iconType": "custom", "customIconUrl": "/api/v1/branding/icon?v=2"}
			if upstreamIconPath != "" {
				data["displayIconType"], data["displayIconUrl"] = "custom", upstreamIconPath
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": data})
		case "/api/v1/branding/scheduled-icons/device-day/icon":
			requestedIcon = r.URL.RequestURI()
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(scheduled)
		case "/api/v1/branding/icon":
			requestedIcon = r.URL.RequestURI()
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(custom)
		default:
			http.NotFound(w, r)
		}
	}))
	defer photoLib.Close()
	brand := &branding{origin: photoLib.URL, client: photoLib.Client()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /brand-icon", brand.serve)
	for _, icon := range []struct {
		display, expectedPath string
		expectedBytes         []byte
	}{
		{"/api/v1/branding/scheduled-icons/device-day/icon?v=1", "/api/v1/branding/scheduled-icons/device-day/icon?v=1", scheduled},
		{"", "/api/v1/branding/icon?v=2", custom},
		{"https://untrusted.example/icon", "", custom},
	} {
		upstreamIconPath, requestedIcon = icon.display, ""
		brand.until = time.Time{}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/brand-icon", nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" ||
			!bytes.Equal(response.Body.Bytes(), icon.expectedBytes) || requestedIcon != icon.expectedPath {
			t.Fatalf("unexpected effective image for %q, requested %q", icon.display, requestedIcon)
		}
	}
}
