# token-usage

A central token-usage accounting system for [Claude Code](https://claude.com/claude-code) sessions
across many machines. Each developer's box runs a small **watcher** that tails the local
JSONL transcripts under `~/.claude/projects/` and ships only the `message.usage` metadata
(no prompt content) to a shared **server**. The server dedups, rolls per-day aggregates,
prices in USD, and exposes both a JSON API and an embedded web dashboard.

```
[Mac1] watcher ──┐
[Mac2] watcher ──┼──>  /ingest  ──>  PostgreSQL ──>  /summary  ──>  Dashboard
[Linux] watcher ─┘     (HTTPS)       usage_detail                    (embedded HTML)
                                     usage_daily
```

Built as two single binaries (`token-usage-server`, `token-usage-watcher`). No CGO,
no external runtime beyond PostgreSQL.

---

## Components

| Binary                  | Role                                                                 |
| ----------------------- | -------------------------------------------------------------------- |
| `token-usage-server`    | HTTP API + dashboard + admin CLI for users/keys.                     |
| `token-usage-watcher`   | Per-machine daemon: tails JSONLs, posts `usage` deltas to /ingest.   |

## Data model

Two tables, both managed automatically by the server on first start.

**`usage_detail`** — one row per assistant message; the source of truth.

| Column                   | Notes                                                            |
| ------------------------ | ---------------------------------------------------------------- |
| `message_id, request_id` | composite primary key — dedup across re-uploads / resumed sessions |
| `session_id`             | Claude Code session UUID                                         |
| `user_id`                | resolved server-side from the API key                            |
| `machine_id`             | the watcher's hostname                                           |
| `tool`                   | e.g. `claude-code`; per-record so a watcher can serve multi-tool |
| `model`                  | full model name as Claude Code reported it                       |
| `ts`                     | message timestamp                                                |
| `input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens` | raw counts; pricing happens at read time |
| `project_path`           | last path segment of the JSONL directory                         |
| `backfill`               | `true` if the record was older than `--backfill-cutoff` at scan  |
| `received_at`            | server clock at insert                                           |

**`usage_daily`** — pre-aggregated per `(day, user_id, machine_id, tool, model)`.
Maintained atomically in the same transaction as detail inserts via
`INSERT … RETURNING` → `GROUP BY` → `INSERT … ON CONFLICT … DO UPDATE SET … += …`.

**`users` / `api_keys`** — multi-tenant identity. Watchers authenticate with a
`tuk_…` bearer token; the server stores only the sha256 of each key.

---

## Quickstart

### 0. PostgreSQL

```bash
createdb tokenusage     # or your hosted PG; the server only creates tables, never the DB
```

### 1. Start the server

```bash
export TOKENUSAGE_DSN='postgres://user:pass@host:5432/tokenusage?sslmode=disable'
./bin/token-usage-server --addr :8080
```

The schema (4 tables + indexes) is created idempotently on startup. The dashboard is
served at `http://host:8080/`.

> Password with special chars? Use the key=value DSN form to dodge URL-encoding:
> `'host=… port=5432 user=postgres password=p@ssw0rd! dbname=tokenusage sslmode=disable'`,
> and quote with **single quotes** in bash so `!` isn't history-expanded.

### 2. Create a user and a watcher key

```bash
./bin/token-usage-server admin user-add --email alice@x.com alice
./bin/token-usage-server admin key-create --name laptop alice
# api key for "alice" (prefix tuk_Qt6umygP):
#
#    tuk_Qt6umygPR0np1xwjW_CS3Hq3EE-CsF6N
#
# save it now — the full key won't be shown again.
```

`--dsn` may be given as a global flag (before the subcommand), per-subcommand, or via
`$TOKENUSAGE_DSN`.

### 3. Run a watcher on each developer's machine

```bash
./bin/token-usage-watcher \
    --endpoint http://server:8080/ingest \
    --api-key tuk_Qt6umygPR0np1xwjW_CS3Hq3EE-CsF6N
```

That's it. The watcher will:

1. Walk `~/.claude/projects/**/*.jsonl`.
2. On first run, ship the **whole history** (records older than 1h are tagged
   `backfill=true` so they don't spike the live curves).
3. On every subsequent tick (default 5s), ship only the new tail; checkpoint is
   keyed on `(inode, offset)` so file rotation and truncation are handled.
4. If the server is unreachable, batches spool to disk under
   `~/.token-usage-watcher/buffer/` and replay on the next tick.

The watcher's hostname auto-detects via `os.Hostname()` — no `--machine` flag needed.

### 4. View the dashboard

Open `http://server:8080/` in a browser. The dashboard refreshes every 30s and
supports per-user filtering, day-range presets (24h / 7d / 30d / 90d / all),
a sortable ledger, and an at-a-glance daily cost chart broken down by model.

---

## HTTP endpoints

| Method | Path        | Auth          | Purpose                                     |
| ------ | ----------- | ------------- | ------------------------------------------- |
| POST   | `/ingest`   | Bearer key    | Batch usage records from a watcher.         |
| GET    | `/summary`  | open          | Per-day per-user per-model totals + cost.   |
| GET    | `/users`    | open          | List of registered users.                   |
| GET    | `/healthz`  | open          | Liveness probe.                             |
| GET    | `/`         | open          | Embedded dashboard (HTML/CSS/JS).           |
| GET    | `/static/*` | open          | Dashboard CSS / JS.                         |

`/summary` accepts `?user=<id>&from=<RFC3339 | unix>&to=<…>`. Empty params mean no filter.

Read endpoints are open within the trusted network — front with nginx / mTLS for
stricter access.

---

## Admin CLI

```
token-usage-server admin [--dsn <DSN>] <command> [flags] [args]

  user-add    [--email <e>] <user_id>            create a user
  user-list                                      list users
  key-create  [--name <label>] <user_id>         mint a new api key (printed once)
  key-list    [--user <user_id>]                 list api keys (prefix only)
  key-revoke  <prefix>                           revoke an api key by its 12-char prefix
```

API keys are formatted `tuk_<43-char-base64url>` (≈192 bits of entropy). Only the
sha256 is stored — leaked DB dumps cannot replay against the API.

---

## Watcher flags

```
--api-key            tuk_… key (required). Also: $TOKENUSAGE_API_KEY
--endpoint           http://server/ingest        ($TOKENUSAGE_ENDPOINT)
--root               JSONL root dir              (default: $HOME/.claude/projects)
--tool               tool tag stamped on records (default: claude-code)
--source             repeatable "tool=path" for multi-source mode
                     e.g. --source claude-code=/path1 --source codex=/path2
--checkpoint         JSON checkpoint path        (default: ~/.token-usage-watcher/checkpoint.json)
--buffer             offline buffer dir          (default: ~/.token-usage-watcher/buffer; "" disables)
--interval           scan interval               (default: 5s)
--batch              records per HTTP batch      (default: 200)
--backfill-cutoff    records older than now-cutoff tagged backfill=true (default: 1h; 0 disables)
--once               run one scan and exit       (useful for cron / manual backfill)
```

Today only the Claude Code JSONL format is parsed. Adding Codex / OpenCode is a
parser branch in `internal/watcher/scanner.go`; the rest of the pipeline
(checkpoint, batching, auth, schema) is format-agnostic.

---

## Pricing

Per-1M-token rates live in `internal/server/pricing.go` (Anthropic public list,
keyed by model-name prefix; longest prefix wins). Override with
`--pricing rates.json`:

```json
{
  "claude-opus-4":   {"input": 15, "output": 75, "cache_creation": 18.75, "cache_read": 1.50},
  "claude-sonnet-4": {"input":  3, "output": 15, "cache_creation":  3.75, "cache_read": 0.30}
}
```

Subscription users (Claude Pro / Max): the displayed USD is **API-equivalent
cost**, not what you actually paid — useful as a relative comparison, not a bill.

---

## Build

```bash
make build            # produces bin/token-usage-server and bin/token-usage-watcher
make test             # go test ./...
make vet              # go vet ./...
```

Pure-Go, no CGO. `pgx/v5` for PostgreSQL.
