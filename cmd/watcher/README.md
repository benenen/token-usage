# token-usage-watcher

Per-machine daemon. Tails Claude Code (and other tools') JSONL transcripts, parses
`message.usage` rows, and POSTs them to a `token-usage-server`. Survives server
outages (offline spool), file rotation/truncation (inode + offset checkpoint), and
machine reboots (native service supervisor).

For the system-level overview (HTTP endpoints, schema, dashboard), see the
[root README](../../README.md).

---

## 1. Install the binary

Pick one.

### Download a release archive

Releases ship five archives — one per platform/arch — at the project's GitHub
Releases page:

| Platform        | File                                                       |
| --------------- | ---------------------------------------------------------- |
| Linux x86_64    | `token-usage-watcher_<ver>_linux_amd64.tar.gz`             |
| Linux arm64     | `token-usage-watcher_<ver>_linux_arm64.tar.gz`             |
| macOS Intel     | `token-usage-watcher_<ver>_darwin_amd64.tar.gz`            |
| macOS Apple Si  | `token-usage-watcher_<ver>_darwin_arm64.tar.gz`            |
| Windows x86_64  | `token-usage-watcher_<ver>_windows_amd64.zip`              |

Extract and drop `token-usage-watcher` (or `.exe`) somewhere on `$PATH` — e.g.
`/usr/local/bin/` on Unix, `C:\Program Files\token-usage\` on Windows. Verify
the checksum against the `SHA256SUMS` in the same release.

### Build from source

```bash
make build           # bin/token-usage-watcher (and the server)
# or just the watcher:
go build -o token-usage-watcher ./cmd/watcher
```

Pure Go, no CGO. Any platform Go supports works.

---

## 2. Get an API key

Ask whoever runs the server to mint one for you:

```bash
token-usage-server admin key-create <your-user-id>
# prints once:  tuk_Qt6umygPR0np1xwjW_CS3Hq3EE-CsF6N
```

Keys are formatted `tuk_<43 base64url chars>`. The server only stores the
sha256 — if you lose it, mint a new one. See the [root README](../../README.md#admin-cli)
for `user-add` and the full admin surface.

---

## 3. Run

Two modes. Use foreground first to smoke-test the connection; install as a
service once you're happy.

### Foreground (smoke test)

```bash
token-usage-watcher \
    --endpoint http://server:8080/ingest \
    --api-key  tuk_Qt6umygPR0np1xwjW_CS3Hq3EE-CsF6N
```

Or via env (handy for service installs):

```bash
export TOKENUSAGE_ENDPOINT=http://server:8080/ingest
export TOKENUSAGE_API_KEY=tuk_…
token-usage-watcher
```

You should see one line per scan tick:

```
watcher: endpoint=… machine=hostname key=tuk_Qt6umygPR… sources=[claude-code→/…/.claude/projects] interval=5s
scan: 312 records across 8 files in 47ms — uploading in 2 batch(es)
ingested 312 records across 8 files
```

On first run the watcher ships **all** historical records (older than 1h are
tagged `backfill=true` so they don't spike live curves). Subsequent ticks ship
only the new tail.

### Install as a service (recommended)

```bash
token-usage-watcher install \
    --endpoint http://server:8080/ingest \
    --api-key  tuk_…
```

The binary self-copies into a stable location and a 0600 env file holds the
API key. There is no `--backend` flag — `install` walks a per-OS ordered
list of candidates and picks the first one whose pre-flight check passes,
printing what it tried and what it picked. `uninstall` / `restart` walk
the same list and act on every candidate that has a unit on disk;
`status` reports each candidate's state.

| OS              | Preference order (root vs non-root)                                       |
| --------------- | ------------------------------------------------------------------------- |
| Linux root      | systemd-system → supervisord → systemd-user                               |
| Linux non-root  | systemd-user                                                              |
| macOS root      | launchd LaunchDaemon → launchd LaunchAgent                                |
| macOS non-root  | launchd LaunchAgent                                                       |
| Windows         | Windows SCM (requires Administrator; no per-user services on Win)         |

Examples:

```bash
# Linux non-root: installs systemd-user (only candidate)
token-usage-watcher install --api-key … --endpoint …

# Linux root: installs systemd-system; falls back to supervisord
# (if systemctl is missing or fails) and then systemd-user
sudo token-usage-watcher install --api-key … --endpoint …

# Windows: in an elevated PowerShell
token-usage-watcher.exe install --api-key … --endpoint …
```

`install` is idempotent — re-running replaces any existing unit cleanly.

#### Status / restart / uninstall

```bash
token-usage-watcher status      # walks every candidate backend on this OS
token-usage-watcher restart     # restart whichever candidate is installed
token-usage-watcher uninstall   # uninstall whichever candidates are installed
```

Or use the native tools directly:

```bash
# Linux user
systemctl --user status token-usage-watcher
journalctl --user -u token-usage-watcher -f

# Linux system
sudo systemctl status token-usage-watcher
sudo journalctl -u token-usage-watcher -f

# macOS user
launchctl list | grep token-usage-watcher
log stream --predicate 'subsystem == "token-usage-watcher"'   # if you wire logging that way

# Windows
sc query token-usage-watcher
# logs: Event Viewer → Windows Logs → Application
```

---

## 4. Flags

```
--api-key            tuk_… key (required)            $TOKENUSAGE_API_KEY
--endpoint           server ingest URL               $TOKENUSAGE_ENDPOINT
                                                     (default: http://localhost:8080/ingest)
--source             repeatable "tool=path" for multi-source mode,
                     e.g. --source claude-code=/p1 --source codex=/p2
--root               JSONL root dir (single-source)  (default: $HOME/.claude/projects)
--tool               tool tag stamped on records     (default: claude-code)
--checkpoint         JSON checkpoint path            (default: ~/.token-usage-watcher/checkpoint.json)
--buffer             offline buffer dir              (default: ~/.token-usage-watcher/buffer; "" disables)
--interval           scan interval                   (default: 5s)
--batch              records per HTTP batch          (default: 200)
--backfill-cutoff    records older than now-cutoff tagged backfill=true (default: 1h; 0 disables)
--once               run one scan and exit           (cron / manual backfill)
```

Without `--source`, `--root`, or `--tool`, the watcher auto-detects every known
tool directory under `$HOME` (currently: `~/.claude/projects`).

`--once` is useful for cron-driven setups or manually re-pulling a window of
history after fixing a config — the checkpoint guarantees no double-ingest.

---

## 5. State on disk

Everything writeable lives under `~/.token-usage-watcher/` (override via
`--checkpoint` / `--buffer`):

```
~/.token-usage-watcher/
  checkpoint.json        # per-file (inode, offset) — atomic tmp+rename writes
  buffer/                # batches that couldn't reach the server; replayed
                         # automatically on the next successful tick
```

Safe to delete `buffer/` if you want to drop spooled batches; safe to delete
`checkpoint.json` if you want to re-ship history (you'll temporarily double the
records — the server dedups on `(machine, tool, request_id, message_id)`).

The watcher's `machine_id` comes from `os.Hostname()` — no flag, no config.
