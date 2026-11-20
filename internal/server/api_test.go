package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tokenusage/internal/types"
)

func TestResolveAPIKeyRejectsDisabledUser(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()

	if err := store.CreateUser(ctx, "alice", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.CreateAPIKey(ctx, "alice", "laptop")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.ResolveAPIKey(ctx, raw)
	if err != nil || got != "alice" {
		t.Fatalf("active user ResolveAPIKey = (%q, %v), want (alice, nil)", got, err)
	}

	if err := store.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAPIKey(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("disabled user ResolveAPIKey err = %v, want ErrInvalidKey", err)
	}

	if err := store.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	got, err = store.ResolveAPIKey(ctx, raw)
	if err != nil || got != "alice" {
		t.Fatalf("re-enabled user ResolveAPIKey = (%q, %v), want (alice, nil)", got, err)
	}

	if err := store.SetUserDisabled(ctx, "nobody", true); err == nil {
		t.Fatal("SetUserDisabled on unknown user = nil, want error")
	}
}

func TestAPIQueryParamValidation(t *testing.T) {
	store := openRepairTestStore(t)
	pricer, err := NewPricer("")
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: store, Pricer: pricer}
	mux := http.NewServeMux()
	api.Register(mux)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"clock invalid tz", "/clock?tz=Invalid/Zone", http.StatusBadRequest},
		{"clock Local sentinel", "/clock?tz=Local", http.StatusBadRequest},
		{"clock valid tz", "/clock?tz=Asia/Shanghai", http.StatusOK},
		{"summary invalid from", "/summary?from=2026-11-01", http.StatusBadRequest},
		{"summary invalid to", "/summary?to=not-a-time", http.StatusBadRequest},
		{"langs invalid from", "/langs?from=bad", http.StatusBadRequest},
		{"clock invalid from", "/clock?from=bad", http.StatusBadRequest},
		{"activity invalid to", "/activity?to=bad", http.StatusBadRequest},
		{"summary valid RFC3339 and unix", "/summary?from=2026-08-01T00:00:00Z&to=1786000000", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("GET %s = %d (%q), want %d", tc.path, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestClockAggregateInvalidTimezoneReturnsErrInvalidTimezone(t *testing.T) {
	store := openRepairTestStore(t)
	seedQueryTestData(t, store)
	_, err := store.ClockAggregate(context.Background(), "", "Invalid/Zone", time.Time{}, time.Time{})
	if !errors.Is(err, ErrInvalidTimezone) {
		t.Fatalf("err = %v, want ErrInvalidTimezone", err)
	}
}

func TestRebuildEditDailyOnlyTouchesRequestedUTCDays(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()
	edits := []types.EditRecord{
		{EventID: "e1", SessionID: "s", Tool: "claude-code", Lang: "golang", Timestamp: time.Date(2026, 8, 3, 23, 59, 59, 0, time.UTC), LinesAdded: 1, LinesRemoved: 0},
		{EventID: "e2", SessionID: "s", Tool: "claude-code", Lang: "golang", Timestamp: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC), LinesAdded: 2, LinesRemoved: 1},
		{EventID: "e3", SessionID: "s", Tool: "claude-code", Lang: "golang", Timestamp: time.Date(2026, 8, 4, 23, 59, 59, 0, time.UTC), LinesAdded: 4, LinesRemoved: 3},
		{EventID: "e4", SessionID: "s", Tool: "claude-code", Lang: "golang", Timestamp: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC), LinesAdded: 8, LinesRemoved: 0},
	}
	if _, _, err := store.InsertEdits(ctx, "machine", "user", edits); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE edit_daily SET lines_added = 0, lines_removed = 0, edits = 0`); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
	n, err := store.RebuildEditDaily(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RebuildEditDaily wrote %d rows, want 1", n)
	}
	got := map[string][3]int64{}
	rows, err := store.pool.Query(ctx, `SELECT to_char(day, 'YYYY-MM-DD'), lines_added, lines_removed, edits FROM edit_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var add, rem, cnt int64
		if err := rows.Scan(&d, &add, &rem, &cnt); err != nil {
			t.Fatal(err)
		}
		got[d] = [3]int64{add, rem, cnt}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]int64{
		"2026-08-03": {0, 0, 0},
		"2026-08-04": {6, 4, 2},
		"2026-08-05": {0, 0, 0},
	}
	for d, w := range want {
		if got[d] != w {
			t.Errorf("%s = %v, want %v (all: %v)", d, got[d], w, got)
		}
	}
}

func TestWorkerStartAndWaitExitsCleanly(t *testing.T) {
	store := openRepairTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		Store:       store,
		TodayEvery:  time.Hour,
		DeepHour:    3,
		DeepDays:    2,
		PriceEvery:  time.Hour,
		PriceSource: srv.URL,
	}
	w.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Worker.Wait did not return after context cancellation")
	}
}

func TestSyncPricesSkipsUnchangedPrices(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()

	payload := map[string]liteLLMEntry{
		"claude-sonnet-4": {
			InputCostPerToken:  ptr(3e-6),
			OutputCostPerToken: ptr(15e-6),
			Mode:               "chat",
		},
		"gpt-4o": {
			InputCostPerToken:  ptr(2.5e-6),
			OutputCostPerToken: ptr(10e-6),
			Mode:               "chat",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	considered, changed, err := SyncPrices(ctx, store, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if considered != 2 || changed != 2 {
		t.Fatalf("first sync = (%d, %d), want (2, 2)", considered, changed)
	}

	// Second sync with identical payload should skip all upserts in memory.
	considered, changed, err = SyncPrices(ctx, store, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if considered != 2 || changed != 0 {
		t.Fatalf("second sync = (%d, %d), want (2, 0)", considered, changed)
	}

	// Changing one model updates only that model and creates a history step.
	payload["gpt-4o"] = liteLLMEntry{
		InputCostPerToken:  ptr(5e-6),
		OutputCostPerToken: ptr(20e-6),
		Mode:               "chat",
	}
	considered, changed, err = SyncPrices(ctx, store, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if considered != 2 || changed != 1 {
		t.Fatalf("third sync = (%d, %d), want (2, 1)", considered, changed)
	}
	hist, err := store.ListPriceHistory(ctx, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("gpt-4o history len = %d, want 2: %+v", len(hist), hist)
	}
}
