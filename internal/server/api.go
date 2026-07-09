package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/types"
)

type API struct {
	Store  *Store
	Pricer *Pricer
}

func (a *API) Register(mux *http.ServeMux) {
	// /ingest requires an API key (auth via api_keys table → user_id).
	// /summary and the embedded dashboard are open within the trusted
	// network — put nginx auth / mTLS in front for stricter access.
	mux.HandleFunc("/ingest", a.handleIngest)
	mux.HandleFunc("/summary", a.handleSummary)
	mux.HandleFunc("/langs", a.handleLangs)
	mux.HandleFunc("/clock", a.handleClock)
	mux.HandleFunc("/users", a.handleUsers)
	mux.HandleFunc("/prices", a.handlePrices)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.Handle("/", webHandler())
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (a *API) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bearer := extractBearer(r.Header.Get("Authorization"))
	if bearer == "" {
		http.Error(w, "missing api key (Authorization: Bearer tuk_...)", http.StatusUnauthorized)
		return
	}
	userID, err := a.Store.ResolveAPIKey(r.Context(), bearer)
	if err != nil {
		if errors.Is(err, ErrInvalidKey) {
			http.Error(w, "invalid or revoked api key", http.StatusUnauthorized)
			return
		}
		log.Printf("ingest auth error: %v", err)
		http.Error(w, "auth error", http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // one batch is typically <100 KiB
	var req types.IngestRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.MachineID == "" {
		http.Error(w, "machine_id required", http.StatusBadRequest)
		return
	}

	acc, dup, err := a.Store.Insert(r.Context(), req.MachineID, userID, req.Records)
	if err != nil {
		log.Printf("ingest insert error from machine=%s user=%s: %v", req.MachineID, userID, err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	eacc, edup, err := a.Store.InsertEdits(r.Context(), req.MachineID, userID, req.Edits)
	if err != nil {
		log.Printf("ingest edits insert error from machine=%s user=%s: %v", req.MachineID, userID, err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(types.IngestResponse{
		Accepted: acc, Duplicates: dup,
		EditsAccepted: eacc, EditsDuplicates: edup,
	})
}

func extractBearer(h string) string {
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

type summaryRow struct {
	Day      string  `json:"day"`
	User     string  `json:"user"`
	Tool     string  `json:"tool"`
	Model    string  `json:"model"`
	Input    int64   `json:"input_tokens"`
	Output   int64   `json:"output_tokens"`
	CacheCC  int64   `json:"cache_creation_tokens"`
	CacheRR  int64   `json:"cache_read_tokens"`
	Total    int64   `json:"total_tokens"` // derived: input + output + cache_creation + cache_read
	Messages int64   `json:"messages"`
	Cost     float64 `json:"cost_usd"`
}

func (a *API) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	user := q.Get("user")
	from := parseTime(q.Get("from"))
	to := parseTime(q.Get("to"))

	rows, err := a.Store.Aggregate(r.Context(), user, from, to)
	if err != nil {
		log.Printf("summary aggregate error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]summaryRow, 0, len(rows))
	for _, row := range rows {
		// Cost is computed in SQL via time-aware lookup against
		// model_prices. Fall back to the in-memory Pricer for models
		// not yet known to the price table (e.g. brand-new SKU before
		// the next litellm sync, or a custom Codex variant).
		cost := row.Cost
		if cost == 0 {
			cost = a.Pricer.Cost(row.Model, row.Input, row.Output, row.CacheCC, row.CacheRR)
		}
		out = append(out, summaryRow{
			Day:      row.Day,
			User:     row.User,
			Tool:     row.Tool,
			Model:    row.Model,
			Input:    row.Input,
			Output:   row.Output,
			CacheCC:  row.CacheCC,
			CacheRR:  row.CacheRR,
			Total:    row.Input + row.Output + row.CacheCC + row.CacheRR,
			Messages: row.Messages,
			Cost:     cost,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type langRow struct {
	Day          string `json:"day"`
	User         string `json:"user"`
	Tool         string `json:"tool"`
	Lang         string `json:"lang"`
	LinesAdded   int64  `json:"lines_added"`
	LinesRemoved int64  `json:"lines_removed"`
	Edits        int64  `json:"edits"`
}

// /langs?user=&from=&to=
//
// Per-day per-user per-tool per-language code-edit stats (lines added /
// removed, edit-event count), read from edit_daily. Open like /summary.
func (a *API) handleLangs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	rows, err := a.Store.LangAggregate(r.Context(), q.Get("user"),
		parseTime(q.Get("from")), parseTime(q.Get("to")))
	if err != nil {
		log.Printf("langs aggregate error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]langRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, langRow{
			Day:          row.Day,
			User:         row.User,
			Tool:         row.Tool,
			Lang:         row.Lang,
			LinesAdded:   row.LinesAdded,
			LinesRemoved: row.LinesRemoved,
			Edits:        row.Edits,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type clockRow struct {
	DOW      int     `json:"dow"`  // 0=Sun .. 6=Sat
	Hour     int     `json:"hour"` // 0..23, in the requested timezone
	Tokens   int64   `json:"tokens"`
	Messages int64   `json:"messages"`
	Cost     float64 `json:"cost_usd"`
}

// /clock?user=&from=&to=&tz=America/New_York
//
// Per-(weekday, hour) usage distribution read from usage_detail and bucketed in
// the caller's timezone (tz, default UTC). Open like /summary — no auth within
// the trusted network. Cost is priced per detail row server-side.
func (a *API) handleClock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	rows, err := a.Store.ClockAggregate(r.Context(), q.Get("user"), q.Get("tz"),
		parseTime(q.Get("from")), parseTime(q.Get("to")))
	if err != nil {
		log.Printf("clock aggregate error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]clockRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, clockRow{
			DOW:      row.DOW,
			Hour:     row.Hour,
			Tokens:   row.Tokens,
			Messages: row.Messages,
			Cost:     row.Cost,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type userView struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email,omitempty"`
	CreatedAt string `json:"created_at"`
	Disabled  bool   `json:"disabled,omitempty"`
}

func (a *API) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	users, err := a.Store.ListUsers(r.Context())
	if err != nil {
		log.Printf("users list error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, userView{
			UserID:    u.UserID,
			Email:     u.Email,
			CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
			Disabled:  u.Disabled,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type priceView struct {
	Model         string  `json:"model_prefix"`
	ValidFrom     string  `json:"valid_from"`
	ValidTo       string  `json:"valid_to,omitempty"`
	InputPer1M    float64 `json:"input_per_1m"`
	OutputPer1M   float64 `json:"output_per_1m"`
	CacheCreate1M float64 `json:"cache_creation_per_1m"`
	CacheRead1M   float64 `json:"cache_read_per_1m"`
	Source        string  `json:"source"`
}

// /prices?model=<prefix>&active=1
//
//	default       -> full history for every model
//	?active=1     -> only the currently-in-effect row per model
//	?model=foo    -> filter to that model's prefix
func (a *API) handlePrices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var (
		rows []PriceRow
		err  error
	)
	if q.Get("active") == "1" {
		rows, err = a.Store.ListActivePrices(r.Context())
	} else {
		rows, err = a.Store.ListPriceHistory(r.Context(), q.Get("model"))
	}
	if err != nil {
		log.Printf("prices list error: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	out := make([]priceView, 0, len(rows))
	for _, p := range rows {
		var to string
		if p.ValidTo != nil {
			to = p.ValidTo.UTC().Format(time.RFC3339)
		}
		out = append(out, priceView{
			Model:         p.ModelPrefix,
			ValidFrom:     p.ValidFrom.UTC().Format(time.RFC3339),
			ValidTo:       to,
			InputPer1M:    p.InputPer1M,
			OutputPer1M:   p.OutputPer1M,
			CacheCreate1M: p.CacheCreate1M,
			CacheRead1M:   p.CacheRead1M,
			Source:        p.Source,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// parseTime accepts RFC3339 or unix seconds. Empty string -> zero time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}
