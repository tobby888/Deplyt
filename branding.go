package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxBrandIconBytes = 6 << 20

//go:embed web/brand/*.png
var brandFiles embed.FS

type brandImage struct {
	contentType string
	body        []byte
}

type branding struct {
	origin string
	client *http.Client
	mu     sync.Mutex
	until  time.Time
	image  brandImage
}

func (b *branding) serve(w http.ResponseWriter, r *http.Request) {
	image := b.current(r.Context())
	w.Header().Set("Content-Type", image.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(image.body)
}

func (b *branding) current(ctx context.Context) brandImage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Now().Before(b.until) {
		return b.image
	}
	if b.origin != "" {
		if image, next, err := b.fetch(ctx); err == nil {
			b.image = image
			b.until = time.Now().Add(30 * time.Second)
			if now := time.Now(); next.After(now) && next.Before(b.until) {
				b.until = next
			}
			return b.image
		}
	}
	if b.image.body == nil {
		b.image = builtinBrandImage("camera")
	}
	b.until = time.Now().Add(10 * time.Second)
	return b.image
}

func (b *branding) fetch(ctx context.Context) (brandImage, time.Time, error) {
	var settings struct {
		Code string
		Data struct {
			IconType, BuiltinIcon, CustomIconURL, DisplayIconType, DisplayIconURL string
			NextIconRefreshAt                                                     time.Time
		}
	}
	body, _, err := b.read(ctx, "/api/v1/branding", 64<<10)
	if err != nil {
		return brandImage{}, time.Time{}, err
	}
	if err := json.Unmarshal(body, &settings); err != nil || settings.Code != "OK" {
		return brandImage{}, time.Time{}, errors.New("invalid PhotoLib branding")
	}
	data := settings.Data
	iconType, iconURL := data.DisplayIconType, data.DisplayIconURL
	if iconType == "" {
		iconType, iconURL = data.IconType, data.CustomIconURL
	}
	if iconType != "custom" {
		return builtinBrandImage(data.BuiltinIcon), data.NextIconRefreshAt, nil
	}

	icon, err := url.Parse(iconURL)
	if err != nil || icon.IsAbs() || icon.Host != "" || icon.User != nil || icon.Fragment != "" ||
		(icon.Path != "/api/v1/branding/icon" &&
			!(strings.HasPrefix(icon.Path, "/api/v1/branding/scheduled-icons/") &&
				strings.HasSuffix(icon.Path, "/icon") &&
				strings.Count(icon.Path, "/") == 6)) {
		return brandImage{}, time.Time{}, errors.New("invalid PhotoLib brand icon URL")
	}
	body, contentType, err := b.read(ctx, icon.RequestURI(), maxBrandIconBytes)
	if err != nil {
		return brandImage{}, time.Time{}, err
	}
	if (contentType != "image/png" && contentType != "image/jpeg") ||
		contentType != http.DetectContentType(body) {
		return brandImage{}, time.Time{}, errors.New("invalid PhotoLib brand icon content")
	}
	return brandImage{contentType, body}, data.NextIconRefreshAt, nil
}

func (b *branding) read(ctx context.Context, path string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.origin+path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New("PhotoLib branding unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, "", errors.New("cannot read PhotoLib branding")
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func builtinBrandImage(name string) brandImage {
	switch name {
	case "camera", "aperture", "picture", "bulb", "star":
	default:
		name = "camera"
	}
	body, err := brandFiles.ReadFile("web/brand/" + name + ".png")
	if err != nil {
		panic(err)
	}
	return brandImage{"image/png", body}
}
