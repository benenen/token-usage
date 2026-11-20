package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"tokenusage/internal/types"
	"tokenusage/internal/watcher"
)

func TestResolveSourcesPrecedence(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit --source wins", func(t *testing.T) {
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, "/default", t.TempDir())
		if err := cmd.ParseFlags([]string{"--source", "codex=/a", "--source", "pi=/b"}); err != nil {
			t.Fatal(err)
		}
		got, err := resolveSources(cmd, &cfg, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != (watcher.Source{Tool: "codex", Root: "/a"}) || got[1] != (watcher.Source{Tool: "pi", Root: "/b"}) {
			t.Fatalf("got = %+v", got)
		}
	})

	t.Run("invalid --source returns error", func(t *testing.T) {
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, "/default", t.TempDir())
		if err := cmd.ParseFlags([]string{"--source", "missing-equals"}); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveSources(cmd, &cfg, home); err == nil {
			t.Fatal("expected error for malformed --source")
		}
	})

	t.Run("explicit --root overrides auto-detect", func(t *testing.T) {
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, "/default", t.TempDir())
		if err := cmd.ParseFlags([]string{"--root", "/custom", "--tool", "codex"}); err != nil {
			t.Fatal(err)
		}
		got, err := resolveSources(cmd, &cfg, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != (watcher.Source{Tool: "codex", Root: "/custom"}) {
			t.Fatalf("got = %+v", got)
		}
	})

	t.Run("auto-detects existing dirs under home", func(t *testing.T) {
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, "/default", t.TempDir())
		got, err := resolveSources(cmd, &cfg, home)
		if err != nil {
			t.Fatal(err)
		}
		want := watcher.Source{Tool: "claude-code", Root: claudeDir}
		found := false
		for _, s := range got {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("got = %+v, want entry %+v", got, want)
		}
	})

	t.Run("falls back to default root when nothing detected", func(t *testing.T) {
		emptyHome := t.TempDir()
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, "/default-root", t.TempDir())
		got, err := resolveSources(cmd, &cfg, emptyHome)
		if err != nil {
			t.Fatal(err)
		}
		if auto := watcher.AutoDetectSources(emptyHome); len(auto) > 0 {
			if len(got) != len(auto) {
				t.Fatalf("got %d sources, want %d from AutoDetectSources", len(got), len(auto))
			}
			return
		}
		if len(got) != 1 || got[0] != (watcher.Source{Tool: "claude-code", Root: "/default-root"}) {
			t.Fatalf("got = %+v", got)
		}
	})
}

func TestSendBatched(t *testing.T) {
	t.Run("empty returns true without calling send", func(t *testing.T) {
		called := false
		ok := sendBatched(0, 2, "", func(start, end int) (watcher.SendResult, error) {
			called = true
			return watcher.SendResult{}, nil
		})
		if !ok || called {
			t.Fatalf("ok=%v called=%v, want true/false", ok, called)
		}
	})

	t.Run("splits into windows", func(t *testing.T) {
		var windows [][2]int
		ok := sendBatched(5, 2, "", func(start, end int) (watcher.SendResult, error) {
			windows = append(windows, [2]int{start, end})
			return watcher.SendResult{Accepted: end - start}, nil
		})
		want := [][2]int{{0, 2}, {2, 4}, {4, 5}}
		if !ok || len(windows) != len(want) {
			t.Fatalf("ok=%v windows=%v, want %v", ok, windows, want)
		}
		for i := range want {
			if windows[i] != want[i] {
				t.Errorf("window %d = %v, want %v", i, windows[i], want[i])
			}
		}
	})

	t.Run("stops on first error", func(t *testing.T) {
		calls := 0
		ok := sendBatched(6, 2, "", func(start, end int) (watcher.SendResult, error) {
			calls++
			if calls == 2 {
				return watcher.SendResult{}, errors.New("boom")
			}
			return watcher.SendResult{Accepted: end - start}, nil
		})
		if ok || calls != 2 {
			t.Fatalf("ok=%v calls=%d, want false/2", ok, calls)
		}
	})

	t.Run("non-positive batchSize does not hang", func(t *testing.T) {
		calls := 0
		ok := sendBatched(3, 0, "", func(start, end int) (watcher.SendResult, error) {
			calls++
			if start != 0 || end != 3 {
				t.Errorf("window = [%d:%d], want [0:3]", start, end)
			}
			return watcher.SendResult{Accepted: end - start}, nil
		})
		if !ok || calls != 1 {
			t.Fatalf("ok=%v calls=%d, want true/1", ok, calls)
		}
	})
}

func TestDoRunRejectsNonPositiveBatch(t *testing.T) {
	for _, batch := range []int{0, -5} {
		var cfg RunConfig
		cmd := &cobra.Command{Use: "test"}
		BindRunFlags(cmd, &cfg, t.TempDir(), t.TempDir())
		cfg.APIKey = "tuk_1234567890"
		cfg.Batch = batch
		if err := DoRun(cmd, &cfg, t.TempDir()); err == nil {
			t.Errorf("DoRun(batch=%d) = nil, want error", batch)
		}
	}
}

func writeClaudeFixture(t *testing.T, dir string) string {
	t.Helper()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(proj, "session.jsonl")
	now := time.Now().UTC().Format(time.RFC3339)
	content := `{"type":"assistant","sessionId":"s1","timestamp":"` + now + `","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"assistant","sessionId":"s1","timestamp":"` + now + `","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-4","usage":{"input_tokens":20,"output_tokens":8}}}
{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"` + now + `","toolUseResult":{"filePath":"/tmp/a.go","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":2,"lines":["-old","+new1","+new2"]}]}}
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunOnceCheckpointAdvancesOnlyWhenAllBatchesSucceed(t *testing.T) {
	t.Run("partial usage batch failure does not advance checkpoint", func(t *testing.T) {
		root := t.TempDir()
		fixturePath := writeClaudeFixture(t, root)
		ckptPath := filepath.Join(t.TempDir(), "checkpoint.json")
		ckpt, err := watcher.LoadCheckpoint(ckptPath)
		if err != nil {
			t.Fatal(err)
		}

		var reqCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reqCount.Add(1) == 2 {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(types.IngestResponse{Accepted: 1})
		}))
		defer srv.Close()

		sc := &watcher.Scanner{
			Sources:    []watcher.Source{{Tool: "claude-code", Root: root}},
			Checkpoint: ckpt,
		}
		up := &watcher.Uploader{
			Endpoint:  srv.URL,
			MachineID: "m1",
			APIKey:    "tuk_test",
			Client:    srv.Client(),
			BufferDir: "", // disabled so 500 fails runOnce
		}

		// batchSize=1 forces 2 usage batches; batch 2 fails.
		runOnce(context.Background(), sc, up, ckpt, 1)

		if _, ok := ckpt.Get(fixturePath); ok {
			t.Fatal("checkpoint advanced despite batch 2 failure")
		}
		if _, err := os.Stat(ckptPath); !os.IsNotExist(err) {
			t.Fatalf("checkpoint file should not exist after failed run, err=%v", err)
		}
	})

	t.Run("edit batch failure does not advance checkpoint", func(t *testing.T) {
		root := t.TempDir()
		fixturePath := writeClaudeFixture(t, root)
		ckptPath := filepath.Join(t.TempDir(), "checkpoint.json")
		ckpt, err := watcher.LoadCheckpoint(ckptPath)
		if err != nil {
			t.Fatal(err)
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req types.IngestRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.Edits) > 0 {
				http.Error(w, "edits failed", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(types.IngestResponse{Accepted: len(req.Records)})
		}))
		defer srv.Close()

		sc := &watcher.Scanner{
			Sources:    []watcher.Source{{Tool: "claude-code", Root: root}},
			Checkpoint: ckpt,
		}
		up := &watcher.Uploader{
			Endpoint:  srv.URL,
			MachineID: "m1",
			APIKey:    "tuk_test",
			Client:    srv.Client(),
		}

		runOnce(context.Background(), sc, up, ckpt, 10)

		if _, ok := ckpt.Get(fixturePath); ok {
			t.Fatal("checkpoint advanced despite edit batch failure")
		}
	})

	t.Run("all batches succeed advances checkpoint and skips on rescan", func(t *testing.T) {
		root := t.TempDir()
		fixturePath := writeClaudeFixture(t, root)
		ckptPath := filepath.Join(t.TempDir(), "checkpoint.json")
		ckpt, err := watcher.LoadCheckpoint(ckptPath)
		if err != nil {
			t.Fatal(err)
		}

		var usageSeen, editsSeen atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req types.IngestRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			usageSeen.Add(int32(len(req.Records)))
			editsSeen.Add(int32(len(req.Edits)))
			_ = json.NewEncoder(w).Encode(types.IngestResponse{
				Accepted:      len(req.Records),
				EditsAccepted: len(req.Edits),
			})
		}))
		defer srv.Close()

		sc := &watcher.Scanner{
			Sources:    []watcher.Source{{Tool: "claude-code", Root: root}},
			Checkpoint: ckpt,
		}
		up := &watcher.Uploader{
			Endpoint:  srv.URL,
			MachineID: "m1",
			APIKey:    "tuk_test",
			Client:    srv.Client(),
		}

		runOnce(context.Background(), sc, up, ckpt, 1)
		if _, ok := ckpt.Get(fixturePath); !ok {
			t.Fatal("checkpoint did not advance after successful run")
		}
		if usageSeen.Load() != 2 || editsSeen.Load() != 1 {
			t.Fatalf("seen usage=%d edits=%d, want 2/1", usageSeen.Load(), editsSeen.Load())
		}

		// Second run should send nothing new.
		runOnce(context.Background(), sc, up, ckpt, 1)
		if usageSeen.Load() != 2 || editsSeen.Load() != 1 {
			t.Fatalf("second run re-sent records: usage=%d edits=%d", usageSeen.Load(), editsSeen.Load())
		}
	})

	t.Run("offline buffer advances checkpoint and flushes on recovery", func(t *testing.T) {
		root := t.TempDir()
		fixturePath := writeClaudeFixture(t, root)
		ckptPath := filepath.Join(t.TempDir(), "checkpoint.json")
		bufDir := filepath.Join(t.TempDir(), "buffer")
		ckpt, err := watcher.LoadCheckpoint(ckptPath)
		if err != nil {
			t.Fatal(err)
		}

		var online atomic.Bool
		var usageSeen, editsSeen atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !online.Load() {
				http.Error(w, "offline", http.StatusServiceUnavailable)
				return
			}
			var req types.IngestRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			usageSeen.Add(int32(len(req.Records)))
			editsSeen.Add(int32(len(req.Edits)))
			_ = json.NewEncoder(w).Encode(types.IngestResponse{
				Accepted:      len(req.Records),
				EditsAccepted: len(req.Edits),
			})
		}))
		defer srv.Close()

		sc := &watcher.Scanner{
			Sources:    []watcher.Source{{Tool: "claude-code", Root: root}},
			Checkpoint: ckpt,
		}
		up := &watcher.Uploader{
			Endpoint:  srv.URL,
			MachineID: "m1",
			APIKey:    "tuk_test",
			Client:    srv.Client(),
			BufferDir: bufDir,
		}

		// Offline tick: spooled to buffer, checkpoint advances.
		runOnce(context.Background(), sc, up, ckpt, 10)
		if _, ok := ckpt.Get(fixturePath); !ok {
			t.Fatal("checkpoint should advance when batches are spooled")
		}

		// Server recovers: next tick flushes spooled usage + edit batches.
		online.Store(true)
		runOnce(context.Background(), sc, up, ckpt, 10)
		if usageSeen.Load() != 2 || editsSeen.Load() != 1 {
			t.Fatalf("flushed usage=%d edits=%d, want 2/1", usageSeen.Load(), editsSeen.Load())
		}
	})
}
