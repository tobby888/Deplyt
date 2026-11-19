package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
)

//go:embed web/*
var webFiles embed.FS

var pageTemplate = template.Must(template.ParseFS(webFiles, "web/index.html"))

type page struct {
	Title   string
	Message string
}

func registerWebAssets(mux *http.ServeMux, brand *branding) {
	mux.HandleFunc("GET /brand-icon", brand.serve)
	mux.HandleFunc("GET /brand.js", serveWebAsset("brand.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", serveWebAsset("style.css", "text/css; charset=utf-8"))
}

func serveWebAsset(name, contentType string) http.HandlerFunc {
	data, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
	}
}

func renderPage(w http.ResponseWriter, status int, data page) {
	var output bytes.Buffer
	if err := pageTemplate.Execute(&output, data); err != nil {
		http.Error(w, "cannot render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; script-src 'self'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = output.WriteTo(w)
}
