package parsers

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// buildOpencodeDB creates a minimal opencode.db with one assistant
// message (usage), one completed edit part, one completed write part
// (new file), and one errored edit part (must be skipped).
func buildOpencodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`INSERT INTO session VALUES ('ses_1', '/w/proj')`,
		`INSERT INTO message VALUES ('msg_1', 'ses_1', 1751364000000, 1751364001000,
		   '{"role":"assistant","modelID":"claude-sonnet-5","providerID":"anthropic",
		     "tokens":{"input":10,"output":20,"reasoning":0,"cache":{"read":2,"write":1}}}')`,
		`INSERT INTO part VALUES ('prt_edit1', 'msg_1', 'ses_1', 1751364002000, 1751364002000,
		   '{"type":"tool","tool":"edit","callID":"c1","state":{"status":"completed",
		     "input":{"filePath":"/w/proj/mapper.xml","oldString":"a","newString":"b"},
		     "metadata":{"diff":"Index: /w/proj/mapper.xml\n===\n--- /w/proj/mapper.xml\n+++ /w/proj/mapper.xml\n@@ -1,2 +1,3 @@\n ctx\n+added one\n+added two\n-removed"}}}')`,
		`INSERT INTO part VALUES ('prt_write1', 'msg_1', 'ses_1', 1751364003000, 1751364003000,
		   '{"type":"tool","tool":"write","callID":"c2","state":{"status":"completed",
		     "input":{"filePath":"/w/proj/New.java","content":"line1\nline2\nline3"},
		     "metadata":{"exists":false}}}')`,
		`INSERT INTO part VALUES ('prt_err1', 'msg_1', 'ses_1', 1751364004000, 1751364004000,
		   '{"type":"tool","tool":"edit","callID":"c3","state":{"status":"error",
		     "input":{"filePath":"/w/proj/x.go"}}}')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	return path
}

func TestOpencodeScanEmitsUsageAndEdits(t *testing.T) {
	path := buildOpencodeDB(t)
	now := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	res, state, err := opencodeParser{}.Scan(path, "opencode", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 1 || res.Usage[0].MessageID != "msg_1" {
		t.Fatalf("usage = %+v, want 1 record msg_1", res.Usage)
	}
	if len(res.Edits) != 2 {
		t.Fatalf("edits = %d, want 2 (errored part skipped): %+v", len(res.Edits), res.Edits)
	}
	e := res.Edits[0]
	if e.EventID != "prt_edit1" || e.Lang != "xml" || e.LinesAdded != 2 || e.LinesRemoved != 1 {
		t.Errorf("edit part = %+v, want prt_edit1 xml +2/-1", e)
	}
	if e.SessionID != "ses_1" || e.ProjectPath != "/w/proj" || e.Tool != "opencode" {
		t.Errorf("edit identity = %+v", e)
	}
	w := res.Edits[1]
	if w.EventID != "prt_write1" || w.Lang != "java" || w.LinesAdded != 3 || w.LinesRemoved != 0 {
		t.Errorf("write part = %+v, want prt_write1 java +3/-0", w)
	}

	// Watermark: re-scan with the returned state yields nothing.
	res2, _, err := opencodeParser{}.Scan(path, "opencode", state, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Usage) != 0 || len(res2.Edits) != 0 {
		t.Errorf("re-scan produced %d usage + %d edits, want 0 + 0", len(res2.Usage), len(res2.Edits))
	}
}

// Older opencode databases have no `part` table — usage must still flow
// and the scan must not error.
func TestOpencodeScanWithoutPartTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`INSERT INTO session VALUES ('ses_1', '/w/proj')`,
		`INSERT INTO message VALUES ('msg_1', 'ses_1', 1751364000000, 1751364001000,
		   '{"role":"assistant","modelID":"claude-sonnet-5","providerID":"anthropic",
		     "tokens":{"input":10,"output":20,"reasoning":0,"cache":{"read":2,"write":1}}}')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	res, _, err := opencodeParser{}.Scan(path, "opencode", FileState{}, 0, time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 1 || len(res.Edits) != 0 {
		t.Errorf("got %d usage + %d edits, want 1 + 0", len(res.Usage), len(res.Edits))
	}
}
