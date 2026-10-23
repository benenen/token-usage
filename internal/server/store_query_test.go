package server

import (
	"context"
	"math"
	"testing"
	"time"

	"tokenusage/internal/types"
)

// seedQueryTestData prices "claude" at a flat rate and "claude-opus" with a
// mid-day price change on 2026-08-05, then inserts usage on two machines so
// usage_daily holds more rows than the per-(day, user, tool, model) output.
func seedQueryTestData(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	change := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO model_prices (model_prefix, valid_from, valid_to,
		    input_per_1m, output_per_1m, cache_creation_per_1m,
		    cache_creation_1h_per_1m, cache_read_per_1m)
		VALUES ('claude',      $1, NULL, 1,  0, 0, 0, 0),
		       ('claude-opus', $1, $2,   10, 0, 5, 8, 0),
		       ('claude-opus', $2, NULL, 20, 0, 5, 8, 0)
	`, epoch, change); err != nil {
		t.Fatal(err)
	}
	at := func(day, hour int) time.Time { return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC) }
	recs := map[string][]types.UsageRecord{
		"machine-a": {
			{MessageID: "a1", SessionID: "s", Model: "claude-opus-4", Timestamp: at(3, 10), InputTokens: 1_000_000},
			{MessageID: "a2", SessionID: "s", Model: "claude-opus-4", Timestamp: at(6, 10), InputTokens: 1_000_000,
				CacheCreationTokens: 1_000_000, CacheCreation1hTokens: 500_000},
			{MessageID: "a3", SessionID: "s", Model: "claude-haiku", Timestamp: at(6, 11), InputTokens: 1_000_000},
			{MessageID: "a4", SessionID: "s", Model: "gpt-x", Timestamp: at(6, 11), InputTokens: 1_000_000},
		},
		"machine-b": {
			{MessageID: "b1", SessionID: "s", Model: "claude-opus-4", Timestamp: at(3, 10), InputTokens: 1_000_000},
		},
	}
	for machine, rs := range recs {
		if _, _, err := store.Insert(ctx, machine, "user", rs); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAggregatePricesByLongestPrefixValidOnDay(t *testing.T) {
	store := openRepairTestStore(t)
	seedQueryTestData(t, store)

	rows, err := store.Aggregate(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ day, model string }
	want := map[key]struct {
		msgs int64
		cost float64
	}{
		{"2026-08-03", "claude-opus-4"}: {2, 20},   // old opus rate, both machines in one row
		{"2026-08-06", "claude-opus-4"}: {1, 26.5}, // 20 input + 5 cache write + 1.5 1h premium
		{"2026-08-06", "claude-haiku"}:  {1, 1},    // falls back to the shorter "claude" prefix
		{"2026-08-06", "gpt-x"}:         {1, 0},    // unpriced
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		w, ok := want[key{r.Day, r.Model}]
		if !ok {
			t.Fatalf("unexpected row %+v", r)
		}
		if r.Messages != w.msgs || math.Abs(r.Cost-w.cost) > 1e-9 {
			t.Errorf("%s %s: messages=%d cost=%v, want %d / %v", r.Day, r.Model, r.Messages, r.Cost, w.msgs, w.cost)
		}
	}
}

func TestClockAggregatePricesLikeAggregate(t *testing.T) {
	store := openRepairTestStore(t)
	seedQueryTestData(t, store)

	rows, err := store.ClockAggregate(context.Background(), "", "UTC", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var msgs int64
	var cost float64
	for _, r := range rows {
		msgs += r.Messages
		cost += r.Cost
	}
	if msgs != 5 || math.Abs(cost-47.5) > 1e-9 {
		t.Fatalf("messages=%d cost=%v, want 5 / 47.5", msgs, cost)
	}
}

func TestRebuildDailyOnlyTouchesRequestedUTCDays(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()
	recs := []types.UsageRecord{
		{MessageID: "m1", SessionID: "s", Model: "m", Timestamp: time.Date(2026, 8, 3, 23, 59, 59, 0, time.UTC), InputTokens: 1},
		{MessageID: "m2", SessionID: "s", Model: "m", Timestamp: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC), InputTokens: 2},
		{MessageID: "m3", SessionID: "s", Model: "m", Timestamp: time.Date(2026, 8, 4, 23, 59, 59, 0, time.UTC), InputTokens: 4},
		{MessageID: "m4", SessionID: "s", Model: "m", Timestamp: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC), InputTokens: 8},
	}
	if _, _, err := store.Insert(ctx, "machine", "user", recs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE usage_daily SET input_tokens = 0`); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
	n, err := store.RebuildDaily(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RebuildDaily wrote %d rows, want 1", n)
	}
	got := map[string]int64{}
	rows, err := store.pool.Query(ctx, `SELECT to_char(day, 'YYYY-MM-DD'), input_tokens FROM usage_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var in int64
		if err := rows.Scan(&d, &in); err != nil {
			t.Fatal(err)
		}
		got[d] = in
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"2026-08-03": 0, "2026-08-04": 6, "2026-08-05": 0}
	for d, w := range want {
		if got[d] != w {
			t.Errorf("%s input_tokens = %d, want %d (all: %v)", d, got[d], w, got)
		}
	}
}

func TestActivityAggregateSumsTokensPerDay(t *testing.T) {
	store := openRepairTestStore(t)
	seedQueryTestData(t, store)
	ctx := context.Background()

	rows, err := store.ActivityAggregate(ctx, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []ActivityRow{
		{Day: "2026-08-03", Tokens: 2_000_000, Messages: 2},
		{Day: "2026-08-06", Tokens: 4_000_000, Messages: 3}, // 1h tokens are a subset, not added
	}
	if len(rows) != len(want) {
		t.Fatalf("got %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	rows, err = store.ActivityAggregate(ctx, "nobody", time.Time{}, time.Time{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown user: rows=%+v err=%v", rows, err)
	}
	rows, err = store.ActivityAggregate(ctx, "user", time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC), time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Day != "2026-08-06" {
		t.Fatalf("from filter: rows=%+v err=%v", rows, err)
	}
}
