# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For end-user documentation (Quickstart, HTTP endpoints, admin CLI, watcher flags), see `README.md`. This file is for editing the code.

## Build / test / run

```bash
make build       # bin/token-usage-server  +  bin/token-usage-watcher  (pure Go, no CGO)
make test        # go test ./...
make vet
go mod tidy

# single test
go test ./internal/server/ -run TestWebHandlerEmbedAndRoutes -v
```

There is currently exactly one test (`internal/server/web_test.go`) — it exercises the embedded dashboard via `httptest.Server`. Anything DB-related is verified end-to-end against a real Postgres (see `README.md` for spinning one up).

## Architecture

Two binaries built from the same module:

- **`cmd/server`** — HTTP server *and* admin CLI in one. `main.go` dispatches on `os.Args[1] == "admin"`; without that argument it runs the server. `admin.go` holds all `admin <cmd>` handlers.
- **`cmd/watcher`** — long-running daemon. Walks JSONL roots, parses `message.usage` rows, POSTs batches to `/ingest`.

Internal packages:

- **`internal/types`** — wire types shared by both binaries. `IngestRequest` has NO `user_id` (resolved server-side from API key); `UsageRecord` carries `tool` per-record.
- **`internal/server/store.go`** — `*Store` wraps `pgxpool`. Owns the schema string and `Insert` / `Aggregate` / `ResolveAPIKey` queries.
- **`internal/server/auth.go`** — API key minting (`tuk_` + 192 bits of base64url) and resolution. Keys are stored as `sha256` hex.
- **`internal/server/api.go`** — HTTP handlers + `Register(mux)`. Only `/ingest` requires auth.
- **`internal/server/web.go`** + **`internal/server/web/`** — dashboard embedded with `//go:embed`.
- **`internal/watcher/scanner.go`** — incremental source walker. Per-tool parser lives in `internal/watcher/parsers/` and is selected by `Source.Tool`. Today: claude-code + codex (JSONL files under a dir) and opencode (single SQLite db file). `Source.Root` may be a directory or a regular file — Scanner stats it and dispatches accordingly.
- **`internal/watcher/uploader.go`** — batched HTTP POST with offline spool dir + bearer auth.
- **`internal/watcher/checkpoint.go`** — atomic `(tmp + rename)` JSON persistence.

## Load-bearing invariants

These are easy to break if you touch the wrong file:

1. **`usage_daily` is maintained by the write path, not by a job.** The atomic upsert is a single CTE chain inside `store.go`'s `insertSQL` constant: detail `INSERT … ON CONFLICT DO NOTHING RETURNING` feeds `GROUP BY` feeds `usage_daily INSERT … ON CONFLICT … DO UPDATE SET col = col + EXCLUDED.col`. Only rows that actually inserted into detail get rolled into daily — dedup guarantees no double-counting. **Any schema change to either table must update that query in lockstep.** The same pattern (and the same rule) applies to `edit_detail`/`edit_daily` and `editInsertSQL` — code-edit events deduped on `event_id`, rolled up per `(day, user, machine, tool, lang)`.

2. **Schema is the `const schema` string in `store.go`**, executed on every server startup; it must stay idempotent (`CREATE … IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, `DROP INDEX IF EXISTS`). There's a `DO $$ … ALTER TABLE usage RENAME TO usage_detail` block for v0.1 → v0.2 upgrades — keep that path working if you rename tables again.

3. **Dashboard JS must never use `innerHTML` with dynamic content.** A pre-tool-use security hook blocks Write/Edit calls that introduce `innerHTML` writes. `app.js` uses a tiny `el(tag, attrs, children)` helper plus `clear(node)` + `textContent` everywhere. If you add a new section, use the helper.

4. **Watcher identity is resolved server-side.** Don't reintroduce a `--user` flag on the watcher or a `user_id` field on `IngestRequest` — `user_id` comes exclusively from `ResolveAPIKey`. The watcher's `MachineID` auto-fills from `os.Hostname()`; there is no `--machine` flag.

5. **Checkpoint advance semantics.** `cmd/watcher/main.go`'s `runOnce` only advances the checkpoint after **all** batches in a scan are durable (either 200-OK from server, or spooled to `--buffer`). On any failure it returns without saving, and the server dedups any rows that did make it through. Don't change this to per-batch advance without thinking about the failure mode.

6. **`--dsn` works in three positions** for `admin`: as global flag (before subcommand), per-subcommand flag (after), or `$TOKENUSAGE_DSN`. This is intentional UX; `runAdmin` parses global flags with a `flag.ContinueOnError` set, then each subcommand re-declares `--dsn` defaulting to the global value.

## Adding a new tool source

The pipeline below the parser is format-agnostic — `Scanner.Sources`, checkpointing, batching, auth, the schema's `tool` column, the dashboard's color palette — none of it needs to change. What you add:

- A new file `internal/watcher/parsers/<tool>.go` with a struct implementing the `Parser` interface and a `func init() { register("<tool>", parser{}) }`. See `claudecode.go` (tail-style append-only JSONL), `codex.go` (re-parse-on-size-change JSONL), and `opencode.go` (single SQLite db, no walk) for the three live patterns. `Scan` returns a `ScanResult{Usage, Edits}` — emitting `Edits` (file-edit events with lang + line counts, see `lang.go` for `langFromPath`/`diffLineCounts`) is optional; a parser that only tracks tokens just leaves it empty.
- An entry in `KnownToolDefaults` in `internal/watcher/defaults.go` so the watcher auto-detects the tool when no `--source` is given. The path may be a directory OR a regular file — Scanner stats `Source.Root` and dispatches accordingly.
- A model-name → color entry in `web/static/app.js`'s `MODEL_PALETTE` if the new tool emits unfamiliar model names.

The CLI already supports `--source tool=path` repeated, so users just add more `--source` entries.

## Pricing

`internal/server/pricing.go` has hardcoded per-1M-token rates keyed by model-name prefix, longest-prefix-wins. Overridable at server start with `--pricing rates.json`. Pricing is computed at **read time** in `/summary`, never stored — so updating rates retroactively re-prices history without a backfill.

## Git conventions

Commit with the repo-local identity — it is already configured here via `git config` (`benshi <shiben789@163.com>`); plain `git commit` picks it up. Never set `~/.gitconfig` globals and never hardcode a different identity with `-c`.
