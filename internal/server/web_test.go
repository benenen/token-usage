package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebHandlerEmbedAndRoutes(t *testing.T) {
	srv := httptest.NewServer(webHandler())
	defer srv.Close()

	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantSub    string // substring expected in body (empty = skip)
		wantCache  string // expected Cache-Control value (empty = skip)
	}{
		{"root serves index", "/", 200, "TOKEN USAGE", "no-cache"},
		{"index direct", "/index.html", 200, "<div class=\"grain\"", "no-cache"},
		{"styles.css", "/static/styles.css", 200, "--ink", "public, max-age=3600"},
		{"app.js", "/static/app.js", 200, "TOKEN USAGE // observatory", "public, max-age=3600"},
		{"unknown 404", "/somethingelse", 404, "", ""},
		{"escape attempt", "/static/../web.go", 404, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer r.Body.Close()
			if r.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s status: got %d, want %d", tc.path, r.StatusCode, tc.wantStatus)
			}
			if tc.wantSub != "" {
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte(tc.wantSub)) {
					t.Fatalf("GET %s body missing %q (got %d bytes)", tc.path, tc.wantSub, len(body))
				}
			}
			if tc.wantCache != "" {
				if got := r.Header.Get("Cache-Control"); !strings.Contains(got, tc.wantCache) {
					t.Errorf("GET %s Cache-Control: got %q, want substring %q", tc.path, got, tc.wantCache)
				}
			}
		})
	}
}
