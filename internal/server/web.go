package server

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html web/static/*
var webFS embed.FS

// webHandler serves the embedded dashboard:
//
//	GET /              -> index.html
//	GET /index.html    -> index.html (canonical, no redirect)
//	GET /static/...    -> CSS / JS
//	anything else      -> 404
func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embedded web FS missing: " + err.Error())
	}
	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("embedded index.html missing: " + err.Error())
	}
	staticFS, err := fs.Sub(sub, "static")
	if err != nil {
		panic("embedded static dir missing: " + err.Error())
	}
	staticSrv := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))

	// Use a fixed modtime so HTTP If-Modified-Since works deterministically.
	indexTime := time.Now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", indexTime, bytes.NewReader(indexBytes))
		case strings.HasPrefix(r.URL.Path, "/static/"):
			w.Header().Set("Cache-Control", "public, max-age=3600")
			staticSrv.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
