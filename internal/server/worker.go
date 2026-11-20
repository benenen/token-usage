package server

import (
	"context"
	"log"
	"sync"
	"time"
)

// Worker runs background reconciliation loops against usage_daily and
// edit_daily, plus periodic model_prices syncing. The daily loops call
// Store.RebuildDaily and Store.RebuildEditDaily, which DELETE + re-aggregate
// the target date range atomically — so even if /ingest's inline upsert ever
// drifts (bug, manual SQL, partial replay), the next pass corrects it.
//
//   - TodayEvery       : how often to rebuild today's row(s).        0 disables.
//   - DeepHour         : local-clock hour to run the deep rebuild.   -1 disables.
//   - DeepDays         : how many days back (incl. today) to rebuild deeply.
//
// Day boundaries are always UTC (matches the schema); only the deep-rebuild
// trigger uses the server's local clock so "8AM" matches operator intuition.
type Worker struct {
	Store       *Store
	TodayEvery  time.Duration
	DeepHour    int
	DeepDays    int
	PriceEvery  time.Duration // how often to sync model_prices; 0 disables
	PriceSource string        // LiteLLM URL or mirror; "" → DefaultPriceSourceURL

	wg sync.WaitGroup
}

func (w *Worker) Start(ctx context.Context) {
	if w.TodayEvery > 0 {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.todayLoop(ctx)
		}()
	}
	if w.DeepHour >= 0 && w.DeepHour <= 23 && w.DeepDays > 0 {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.deepLoop(ctx)
		}()
	}
	if w.PriceEvery > 0 {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.priceLoop(ctx)
		}()
	}
}

// Wait blocks until every goroutine launched by Start has exited.
// Call after cancelling the context passed to Start and before closing Store.
func (w *Worker) Wait() {
	w.wg.Wait()
}

func (w *Worker) priceLoop(ctx context.Context) {
	log.Printf("worker: price-sync every %s (source: %s)",
		w.PriceEvery, displayPriceSource(w.PriceSource))
	// First sync at startup so a brand-new server doesn't wait 12h for prices.
	w.syncPrices(ctx)
	t := time.NewTicker(w.PriceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.syncPrices(ctx)
		}
	}
}

func (w *Worker) syncPrices(ctx context.Context) {
	start := time.Now()
	considered, changed, err := SyncPrices(ctx, w.Store, w.PriceSource, nil)
	if err != nil {
		log.Printf("worker: price-sync failed: %v", err)
		return
	}
	log.Printf("worker: price-sync ok — considered=%d changed=%d in %s",
		considered, changed, time.Since(start).Round(time.Millisecond))
}

func displayPriceSource(s string) string {
	if s == "" {
		return DefaultPriceSourceURL
	}
	return s
}

func (w *Worker) todayLoop(ctx context.Context) {
	log.Printf("worker: today-rebuild every %s", w.TodayEvery)
	// First pass right away so a fresh server publishes a sane today row.
	w.rebuildToday(ctx)
	t := time.NewTicker(w.TodayEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.rebuildToday(ctx)
		}
	}
}

func (w *Worker) deepLoop(ctx context.Context) {
	for {
		next := nextLocalHour(w.DeepHour)
		log.Printf("worker: next deep-rebuild (%d days) at %s (in %s)",
			w.DeepDays, next.Format("2006-01-02 15:04 MST"),
			time.Until(next).Round(time.Second))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		w.rebuildDeep(ctx)
	}
}

func (w *Worker) rebuildToday(ctx context.Context) {
	today := utcDay(time.Now())
	start := time.Now()
	usageRows, uerr := w.Store.RebuildDaily(ctx, today, today)
	if uerr != nil {
		log.Printf("worker: today usage rebuild failed: %v", uerr)
	}
	editRows, eerr := w.Store.RebuildEditDaily(ctx, today, today)
	if eerr != nil {
		log.Printf("worker: today edit rebuild failed: %v", eerr)
	}
	if uerr == nil && eerr == nil {
		log.Printf("worker: today rebuild ok — day=%s usage_rows=%d edit_rows=%d in %s",
			today.Format("2006-01-02"), usageRows, editRows, time.Since(start).Round(time.Millisecond))
	}
}

func (w *Worker) rebuildDeep(ctx context.Context) {
	today := utcDay(time.Now())
	from := today.AddDate(0, 0, -(w.DeepDays - 1))
	start := time.Now()
	usageRows, uerr := w.Store.RebuildDaily(ctx, from, today)
	if uerr != nil {
		log.Printf("worker: deep usage rebuild [%s..%s] failed: %v",
			from.Format("2006-01-02"), today.Format("2006-01-02"), uerr)
	}
	editRows, eerr := w.Store.RebuildEditDaily(ctx, from, today)
	if eerr != nil {
		log.Printf("worker: deep edit rebuild [%s..%s] failed: %v",
			from.Format("2006-01-02"), today.Format("2006-01-02"), eerr)
	}
	if uerr == nil && eerr == nil {
		log.Printf("worker: deep rebuild ok — range=[%s..%s] usage_rows=%d edit_rows=%d in %s",
			from.Format("2006-01-02"), today.Format("2006-01-02"), usageRows, editRows,
			time.Since(start).Round(time.Millisecond))
	}
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// nextLocalHour returns the next time the wall clock (server local) hits `hour:00`.
// If we're past that today, it returns the same time tomorrow.
func nextLocalHour(hour int) time.Time {
	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.Local)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}
