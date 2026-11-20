package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"tokenusage/internal/types"
)

// Edits must travel in their own request (no records field), so a
// pre-edits server that rejects unknown JSON fields still accepts the
// usage batches from an upgraded watcher.
func TestSendEditsPostsStandaloneBatch(t *testing.T) {
	var got types.IngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(types.IngestResponse{EditsAccepted: 1, EditsDuplicates: 1})
	}))
	defer srv.Close()

	u := &Uploader{
		Endpoint:  srv.URL,
		MachineID: "m1",
		APIKey:    "tuk_test",
		Client:    srv.Client(),
	}
	edits := []types.EditRecord{
		{EventID: "e1", SessionID: "s1", Tool: "claude-code", Lang: "golang",
			LinesAdded: 3, LinesRemoved: 1, Timestamp: time.Now()},
		{EventID: "e2", SessionID: "s1", Tool: "codex", Lang: "java",
			LinesAdded: 2, Timestamp: time.Now()},
	}
	res, err := u.SendEdits(context.Background(), edits)
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != "m1" || len(got.Edits) != 2 || len(got.Records) != 0 {
		t.Errorf("request = %+v, want machine m1, 2 edits, 0 records", got)
	}
	if res.Accepted != 1 || res.Duplicates != 1 {
		t.Errorf("result = %+v, want accepted=1 duplicates=1", res)
	}
}

func TestSendEditsEmptyIsNoop(t *testing.T) {
	u := &Uploader{Endpoint: "http://127.0.0.1:1", Client: &http.Client{}}
	if _, err := u.SendEdits(context.Background(), nil); err != nil {
		t.Fatalf("empty SendEdits must not touch the network: %v", err)
	}
}

func TestSpoolCreatesUniqueFilesAndFlushCountsUsageAndEdits(t *testing.T) {
	bufDir := t.TempDir()
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer failSrv.Close()

	u := &Uploader{
		Endpoint:  failSrv.URL,
		MachineID: "m1",
		APIKey:    "tuk_test",
		Client:    failSrv.Client(),
		BufferDir: bufDir,
	}
	res1, err := u.Send(context.Background(), []types.UsageRecord{
		{MessageID: "m1", SessionID: "s1", Model: "claude-sonnet-4", Timestamp: time.Now()},
	})
	if err != nil || !res1.Spooled {
		t.Fatalf("Send = (%+v, %v), want spooled", res1, err)
	}
	res2, err := u.SendEdits(context.Background(), []types.EditRecord{
		{EventID: "e1", SessionID: "s1", Tool: "claude-code", Lang: "golang", LinesAdded: 5, Timestamp: time.Now()},
	})
	if err != nil || !res2.Spooled {
		t.Fatalf("SendEdits = (%+v, %v), want spooled", res2, err)
	}

	entries, err := os.ReadDir(bufDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("spooled files = %d, want 2 (no filename collision)", len(entries))
	}

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req types.IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if len(req.Edits) > 0 {
			_ = json.NewEncoder(w).Encode(types.IngestResponse{EditsAccepted: 3, EditsDuplicates: 4})
			return
		}
		_ = json.NewEncoder(w).Encode(types.IngestResponse{Accepted: 2, Duplicates: 1})
	}))
	defer okSrv.Close()

	u.Endpoint = okSrv.URL
	u.Client = okSrv.Client()

	var logs bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(origOut)

	u.Flush(context.Background())

	entries, err = os.ReadDir(bufDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remaining spooled files = %d, want 0", len(entries))
	}
	if !strings.Contains(logs.String(), "flushed 2 spooled batch(es): accepted=5 duplicates=5") {
		t.Fatalf("flush log = %q, want combined usage+edits totals", logs.String())
	}
}
