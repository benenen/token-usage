package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"tokenusage/internal/types"
)

type Uploader struct {
	Endpoint  string
	MachineID string
	APIKey    string // tuk_… key; sent as Authorization: Bearer header. Required.
	Client    *http.Client
	BufferDir string // if non-empty, failed batches are written here for later retry
}

// SendResult reports what the server actually did with the batch. On a
// 2xx, Accepted+Duplicates sum to len(recs); on a spooled batch (server
// unreachable, written to BufferDir), Spooled is true and SpoolReason
// carries the underlying HTTP/transport error so callers can log it
// (otherwise "spooled" is opaque — was it DNS? a 5xx? a timeout?).
// On a 2xx the other fields are zero and SpoolReason is nil.
type SendResult struct {
	Accepted    int
	Duplicates  int
	Spooled     bool
	SpoolReason error
}

// Send POSTs recs as one batch. If the POST fails AND BufferDir is set,
// it durably writes the batch to disk and returns Spooled=true, nil —
// callers can safely advance the checkpoint. Returns an error only when
// nothing is durable yet.
func (u *Uploader) Send(ctx context.Context, recs []types.UsageRecord) (SendResult, error) {
	if len(recs) == 0 {
		return SendResult{}, nil
	}
	body, err := json.Marshal(types.IngestRequest{
		MachineID: u.MachineID,
		Records:   recs,
	})
	if err != nil {
		return SendResult{}, err
	}
	res, err := u.post(ctx, body)
	if err == nil {
		return res, nil
	}
	if u.BufferDir == "" {
		return SendResult{}, err
	}
	// Server unreachable or 5xx — spool to disk so the checkpoint can advance.
	if werr := u.spool(body); werr != nil {
		return SendResult{}, fmt.Errorf("post failed (%v) and could not buffer (%v)", err, werr)
	}
	return SendResult{Spooled: true, SpoolReason: err}, nil
}

func (u *Uploader) post(ctx context.Context, body []byte) (SendResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Endpoint, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return SendResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SendResult{}, fmt.Errorf("server %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var ack types.IngestResponse
	// Server may legitimately return an empty body (older builds, or a
	// 204-style no-content path); fall through to zero-valued ack rather
	// than treat decode failure as a transport error.
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	return SendResult{Accepted: ack.Accepted, Duplicates: ack.Duplicates}, nil
}

func (u *Uploader) spool(body []byte) error {
	if err := os.MkdirAll(u.BufferDir, 0o700); err != nil {
		return err
	}
	// Nanosecond timestamp keeps lexicographic order = chronological order.
	name := fmt.Sprintf("batch-%d.json", time.Now().UnixNano())
	tmp := filepath.Join(u.BufferDir, name+".tmp")
	final := filepath.Join(u.BufferDir, name)
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Flush attempts to re-send everything in BufferDir. Stops on the first
// network failure (no point hammering an offline server).
func (u *Uploader) Flush(ctx context.Context) {
	if u.BufferDir == "" {
		return
	}
	entries, err := os.ReadDir(u.BufferDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		p := filepath.Join(u.BufferDir, e.Name())
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if _, err := u.post(ctx, body); err != nil {
			// Stop the loop; we'll retry next tick. Only bail on context-cancel
			// or transport-level failure; an HTTP error means the server saw it
			// but rejected — in that case drop the bad batch so we don't loop.
			if isHTTPRejection(err) {
				_ = os.Remove(p)
				continue
			}
			return
		}
		_ = os.Remove(p)
	}
}

// isHTTPRejection reports whether err is a server-side rejection that
// won't fix itself on retry — those are the only batches Flush deletes
// from the spool. Anything else (transport errors, 5xx, AND 4xx that a
// human is expected to fix, like 401 auth-failed or 429 rate-limit)
// stays queued so that once the cause is resolved, the original records
// flow through to the server.
//
// Concretely, drop only:
//   400 bad request    — payload itself is malformed; won't get better
//   404 not found      — wrong endpoint URL; admin must reconfigure
//   410 gone           — server explicitly retired this endpoint
//   413 payload too large — batch oversized; would still be oversized
//   422 unprocessable  — schema-level rejection (Anthropic+OpenAI use)
//
// Keep (i.e. NOT a rejection — retry forever):
//   401 unauthorized   — wrong/revoked API key; will work after fix
//   403 forbidden      — perms problem; will work after fix
//   408 request timeout — transient
//   429 too many requests — transient, the server is asking us to wait
//   5xx                — transient by definition
//   transport errors   — dial refused, DNS, TLS, deadline exceeded, etc.
func isHTTPRejection(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// post() formats as "server NNN: <body>" for HTTP-status errors.
	const prefix = "server "
	if len(s) < len(prefix)+3 || s[:len(prefix)] != prefix {
		return false // transport/dial/TLS/etc — always retry
	}
	switch s[len(prefix) : len(prefix)+3] {
	case "400", "404", "410", "413", "422":
		return true
	}
	return false
}
