package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
