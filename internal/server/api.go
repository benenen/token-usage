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
	mux.HandleFunc("/users", a.handleUsers)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(types.IngestResponse{Accepted: acc, Duplicates: dup})
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
			Cost:     a.Pricer.Cost(row.Model, row.Input, row.Output, row.CacheCC, row.CacheRR),
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
