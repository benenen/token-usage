package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
//
// The index is rewritten at startup so every reference to a /static/ asset
// gets a ?v=<sha> query string derived from the embedded asset bytes. Asset
// responses set Cache-Control: max-age=3600, but the URL changes whenever
// the binary changes — so an upgrade automatically invalidates the cache
// without operators having to tell users to hard-refresh.
func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embedded web FS missing: " + err.Error())
	}
	rawIndex, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("embedded index.html missing: " + err.Error())
	}
	staticSub, err := fs.Sub(sub, "static")
	if err != nil {
		panic("embedded static dir missing: " + err.Error())
	}

	// Hash whatever's under static/ — gives us a stable version stamp per binary.
	ver := assetsVersion(staticSub)
	indexBytes := bytes.ReplaceAll(rawIndex,
		[]byte(`/static/app.js"`),
		[]byte(`/static/app.js?v=`+ver+`"`))
	indexBytes = bytes.ReplaceAll(indexBytes,
		[]byte(`/static/styles.css"`),
		[]byte(`/static/styles.css?v=`+ver+`"`))

	staticSrv := http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))
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

// assetsVersion returns the first 10 hex chars of sha256 over every file
// in `static`, sorted by name for stability. Different bytes → different
// stamp → fresh URL → cache busted.
func assetsVersion(staticSub fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(staticSub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(staticSub, p)
		if rerr != nil {
			return rerr
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}
