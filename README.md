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

**`edit_detail` / `edit_daily`** — file-edit events (code lines added/removed,
per language) mirroring the usage pair. The watcher extracts them from the same
transcripts: Claude Code Edit/Write `structuredPatch`es, codex `apply_patch`
calls (only ones whose output confirms success), opencode edit/write tool parts,
pi `write`/`edit` toolCall arguments.
Language is derived from the file extension (`.go`→`golang`, `.java`→`java`, …,
unknown→`other`); file paths and contents never leave the machine — only the
language tag and line counts are uploaded. Deduped on `event_id` (the
transcript's own uuid / call id / part id), rolled up per
`(day, user_id, machine_id, tool, lang)`.

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

1. **Auto-detect** every supported tool on the box (currently `claude-code`,
   `codex`, `opencode`, `pi`) and walk each one's transcript root in parallel.
2. On first run, ship the **whole history** (records older than 1h are tagged
   `backfill=true` so they don't spike the live curves).
3. On every subsequent tick (default 5s), ship only the new tail; per-file
   checkpoint is keyed on `(inode, offset)` for tail-style sources or
   `(inode, time_updated)` for SQLite (opencode), so rotation and truncation
   are handled.
4. If the server is unreachable, batches spool to disk under
   `~/.token-usage-watcher/buffer/` and replay on the next tick.

The watcher's hostname auto-detects via `os.Hostname()` — no `--machine` flag needed.

For service-mode install on each developer's box, the watcher self-installs:

```bash
./bin/token-usage-watcher install \
    --endpoint http://server:8080/ingest \
    --api-key tuk_…
```

`install` auto-picks the best backend for the OS (systemd / launchd / SCM /
supervisord) without you having to know what's available; `uninstall`,
`restart`, `status` do the obvious thing. See
[`cmd/watcher/README.md`](cmd/watcher/README.md) for the full install /
service-management guide.

### 4. View the dashboard

Open `http://server:8080/` in a browser. The dashboard refreshes every 30s and
supports per-user filtering, day-range presets (24h / 7d / 30d / 90d / all),
a sortable ledger, and an at-a-glance daily cost chart broken down by model.

---

## HTTP endpoints

| Method | Path        | Auth          | Purpose                                                   |
| ------ | ----------- | ------------- | --------------------------------------------------------- |
| POST   | `/ingest`   | Bearer key    | Batch usage records and/or edit events from a watcher.    |
| GET    | `/summary`  | open          | Per-day per-user per-model totals + cost.                 |
| GET    | `/langs`    | open          | Per-day per-user per-tool per-language code-edit stats.   |
| GET    | `/users`    | open          | List of registered users.                                 |
| GET    | `/prices`   | open          | Active model prices + per-prefix history (LiteLLM-synced).|
| GET    | `/healthz`  | open          | Liveness probe.                                           |
| GET    | `/`         | open          | Embedded dashboard (HTML/CSS/JS).                         |
| GET    | `/static/*` | open          | Dashboard CSS / JS.                                       |

`/summary` and `/langs` accept `?user=<id>&from=<RFC3339 | unix>&to=<…>`. Empty params mean no filter.

Note on upgrades: edit events ride in a separate `/ingest` request from usage
records, so an upgraded watcher keeps working against a pre-`/langs` server
(the edits-only batches are rejected and dropped; token accounting is
unaffected). Upgrade the server first to capture everything. Historical edits
older than the watcher's checkpoint can be backfilled by deleting the
checkpoint file and letting the watcher re-scan — the server dedups.

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

## Watcher

Subcommands (run with `--help` for details):

```
token-usage-watcher [run]   tail transcripts and ship usage (default if no subcommand)
                  install   self-install as a native service (auto-pick backend)
                  uninstall stop and remove the installed service
                  start     start the installed service
                  stop      stop the installed service (without removing it)
                  restart   restart the installed service
                  status    show service status across every per-OS backend
                  logs      show service log output (-f to tail-follow)
                  cleanup   stop → clear ~/.token-usage-watcher/buffer → start
                            (--with-checkpoint also drops checkpoint.json
                             to force a full re-scan)
```

`run`-mode flags:

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

Supported tools today: **claude-code** (`~/.claude/projects/**/*.jsonl`),
**codex** (`~/.codex/sessions/**/*.jsonl`), **opencode**
(`~/.local/share/opencode/opencode.db`, a single SQLite file), and **pi**
(`~/.pi/agent/sessions/**/*.jsonl`). The watcher
auto-detects whichever of these exist on the box. Adding a new tool is a
new file under `internal/watcher/parsers/`; the rest of the pipeline
(checkpoint, batching, auth, schema) is format-agnostic.

For end-user-facing install / service-management docs (per-OS backend
preference order, native-tool commands, on-disk state), see
[`cmd/watcher/README.md`](cmd/watcher/README.md).

---

## Pricing

Per-1M-token rates live in the **`model_prices`** table, time-versioned
`(model_prefix, valid_from, valid_to)` with longest-prefix-wins lookup.
Pricing is applied **at read time** in `/summary` via a LATERAL JOIN, so
rate changes re-price history without any backfill.

How the table gets populated:

1. **First start** seeds it from the hardcoded defaults in
   `internal/server/pricing.go` (`source='default-seed'`, `valid_from=epoch`).
   That ensures `/summary` always finds a row even before any sync runs.
2. The server worker refreshes from
   [LiteLLM's `model_prices_and_context_window.json`](https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json)
   every `--price-sync-every` (default `12h`; `0` disables). Each new
   prefix's first sync inserts with `valid_from=epoch` so historical
   usage is re-priced too; subsequent rate changes for the same prefix
   close the previous row (`valid_to=NOW`) and insert a new one — i.e.
   real LiteLLM updates show up as a proper history step.
3. Manual override at server start: `--pricing rates.json`
   (`$TOKENUSAGE_PRICING`):

   ```json
   {
     "claude-opus-4":   {"input": 15, "output": 75, "cache_creation": 18.75, "cache_read": 1.50},
     "claude-sonnet-4": {"input":  3, "output": 15, "cache_creation":  3.75, "cache_read": 0.30}
   }
   ```

The dashboard's price-history modal (click a model name) renders the
full `valid_from / valid_to` chain per prefix.

Subscription users (Claude Pro / Max): the displayed USD is **API-equivalent
cost**, not what you actually paid — useful as a relative comparison, not a bill.

---

## Build & release

```bash
make build            # produces bin/token-usage-server and bin/token-usage-watcher
make test             # go test ./...
make vet              # go vet ./...
make release          # cross-compile ./cmd/watcher for 5 platforms
                      # → dist/token-usage-watcher_<ver>_<os>_<arch>.{tar.gz,zip}
                      # + dist/SHA256SUMS (stamps `git describe`; override via RELEASE_VERSION=…)
```

Helper scripts under `scripts/`:

- `scripts/upload-dist.sh user@host:/path` — scp every `dist/` artifact to a server.
- `scripts/push-image.sh harbor.example.com/project [tag]` — `docker tag` +
  `docker push` the locally-built server image into a Harbor (or any) registry.
  `HARBOR_USER` / `HARBOR_PASSWORD` env trigger a `docker login` via `--password-stdin`.

Pure-Go, no CGO. `pgx/v5` for PostgreSQL; `modernc.org/sqlite` (CGO-free) for
the opencode parser.
