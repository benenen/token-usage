#!/usr/bin/env bash
# token-usage-watcher installer
#
# Installs the watcher binary and a process-supervisor entry that
# auto-restarts on failure. Picks a supervisor backend:
#
#   --user        (default) systemd user service in ~/.config/systemd/user/
#   --system      systemd system service in /etc/systemd/system/  (needs root)
#   --supervisor  supervisord program in /etc/supervisor/conf.d/   (no systemd)
#   --no-systemd  install binary + env only; print how to run by hand
#
# Examples:
#   ./installer.sh --api-key tuk_xxx --endpoint http://server:8080/ingest
#   ./installer.sh --supervisor --api-key ... --endpoint ...   # for containers
#   ./installer.sh --uninstall                                 # same backend
#
#   # download from a release, no local binary:
#   ./installer.sh --repo owner/repo --api-key ... --endpoint ...

set -euo pipefail

SERVICE_NAME="token-usage-watcher"
BACKEND="user"        # user | system | supervisor | files-only
API_KEY=""
ENDPOINT=""
EXTRA_ARGS=""
BINARY_PATH=""
REPO=""
VERSION="latest"
DO_UNINSTALL=0
SUPERVISOR_CONF_DIR=""

usage() {
  cat <<EOF
Usage: $0 [options]

Required (unless --uninstall):
  --api-key   KEY        API key minted by token-usage-server admin
  --endpoint  URL        server /ingest URL  (e.g. http://server:8080/ingest)

Binary source (pick one; if none, --repo is required):
  --binary    PATH       use a local watcher binary
  --repo      OWNER/REPO download latest release tarball from github.com
  --version   TAG        with --repo, install this tag instead of latest

Backend (pick at most one; default is --user):
  --user                 systemd user service             (\$HOME/.config/...)
  --system               systemd system service           (/etc/systemd/...)
  --supervisor           supervisord program              (/etc/supervisor/...)
  --no-systemd           no supervisor; just install files + print run cmd
  --supervisor-conf-dir  override supervisord include dir
                         (default: /etc/supervisor/conf.d/)

Other:
  --extra-args "..."     extra flags appended to ExecStart / command (quoted)
  --uninstall            stop and remove the service for the chosen backend
  -h, --help             show this help
EOF
}

log()  { printf '\033[36m[installer]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }

# ----- arg parsing ----------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key)             API_KEY="${2:-}"; shift 2 ;;
    --endpoint)            ENDPOINT="${2:-}"; shift 2 ;;
    --binary)              BINARY_PATH="${2:-}"; shift 2 ;;
    --repo)                REPO="${2:-}"; shift 2 ;;
    --version)             VERSION="${2:-}"; shift 2 ;;
    --extra-args)          EXTRA_ARGS="${2:-}"; shift 2 ;;
    --supervisor-conf-dir) SUPERVISOR_CONF_DIR="${2:-}"; shift 2 ;;
    --user)                BACKEND=user; shift ;;
    --system)              BACKEND=system; shift ;;
    --supervisor)          BACKEND=supervisor; shift ;;
    --no-systemd)          BACKEND=files-only; shift ;;
    --uninstall)           DO_UNINSTALL=1; shift ;;
    -h|--help)             usage; exit 0 ;;
    *)                     die "unknown flag: $1 (use --help)" ;;
  esac
done

# ----- platform paths (per-backend) -----------------------------------------
case "$BACKEND" in
  user)
    BIN_DIR="$HOME/.local/bin"
    UNIT_DIR="$HOME/.config/systemd/user"
    ENV_DIR="$HOME/.config/token-usage-watcher"
    NEEDS_SUDO=0
    ;;
  system|supervisor|files-only)
    BIN_DIR=/usr/local/bin
    UNIT_DIR=/etc/systemd/system
    ENV_DIR=/etc/token-usage-watcher
    NEEDS_SUDO=$([[ $EUID -eq 0 ]] && echo 0 || echo 1)
    ;;
esac
BIN_PATH="$BIN_DIR/$SERVICE_NAME"
UNIT_PATH="$UNIT_DIR/$SERVICE_NAME.service"
ENV_PATH="$ENV_DIR/env"

# Detect supervisord include dir if needed
if [[ "$BACKEND" == "supervisor" && -z "$SUPERVISOR_CONF_DIR" ]]; then
  for d in /etc/supervisor/conf.d /etc/supervisord.d; do
    [[ -d "$d" ]] && { SUPERVISOR_CONF_DIR="$d"; break; }
  done
  SUPERVISOR_CONF_DIR="${SUPERVISOR_CONF_DIR:-/etc/supervisor/conf.d}"
fi
SUPERVISOR_CONF_PATH="$SUPERVISOR_CONF_DIR/$SERVICE_NAME.conf"

# wrapper around sudo so user mode skips it cleanly
maybe_sudo() { if (( NEEDS_SUDO )); then sudo "$@"; else "$@"; fi; }

# ----- uninstall path -------------------------------------------------------
if (( DO_UNINSTALL )); then
  log "uninstalling $SERVICE_NAME (backend=$BACKEND)"
  case "$BACKEND" in
    user)
      systemctl --user stop    "$SERVICE_NAME.service" 2>/dev/null || true
      systemctl --user disable "$SERVICE_NAME.service" 2>/dev/null || true
      systemctl --user daemon-reload 2>/dev/null || true
      rm -f "$UNIT_PATH" "$BIN_PATH"
      rm -rf "$ENV_DIR"
      ;;
    system)
      sudo systemctl stop    "$SERVICE_NAME.service" 2>/dev/null || true
      sudo systemctl disable "$SERVICE_NAME.service" 2>/dev/null || true
      sudo systemctl daemon-reload
      sudo rm -f "$UNIT_PATH" "$BIN_PATH"
      sudo rm -rf "$ENV_DIR"
      ;;
    supervisor)
      maybe_sudo supervisorctl stop   "$SERVICE_NAME" 2>/dev/null || true
      maybe_sudo supervisorctl remove "$SERVICE_NAME" 2>/dev/null || true
      maybe_sudo rm -f "$SUPERVISOR_CONF_PATH"
      maybe_sudo supervisorctl reread 2>/dev/null || true
      maybe_sudo supervisorctl update 2>/dev/null || true
      maybe_sudo rm -f "$BIN_PATH"
      maybe_sudo rm -rf "$ENV_DIR"
      ;;
    files-only)
      maybe_sudo rm -f "$BIN_PATH"
      maybe_sudo rm -rf "$ENV_DIR"
      ;;
  esac
  log "uninstalled. (Checkpoint at \$HOME/.token-usage-watcher/ left untouched.)"
  exit 0
fi

# ----- install pre-flight ---------------------------------------------------
[[ "$(uname -s)" == "Linux" ]] || die "this installer is Linux only (on macOS use launchd)"
[[ -n "$API_KEY" ]]  || die "--api-key is required"
[[ -n "$ENDPOINT" ]] || die "--endpoint is required"

case "$BACKEND" in
  user)
    need systemctl
    if ! systemctl --user show >/dev/null 2>&1; then
      cat >&2 <<EOF
[error] systemctl --user can't reach a session bus (likely no user D-Bus,
        e.g. inside a container or a non-graphical login).

        Pick one of:
          1) Run as a system service (needs root + working systemd):
                $0 --system --api-key ... --endpoint ...
          2) Use supervisord instead:
                $0 --supervisor --api-key ... --endpoint ...
          3) Skip supervisor entirely, install files + print run cmd:
                $0 --no-systemd --api-key ... --endpoint ...
          4) Enable session lingering and start a new login:
                sudo loginctl enable-linger \$USER
EOF
      exit 1
    fi
    ;;
  system)
    need systemctl
    [[ -d /run/systemd/system ]] || die "system systemd isn't running on this host"
    ;;
  supervisor)
    need supervisorctl
    if ! maybe_sudo supervisorctl version >/dev/null 2>&1; then
      die "supervisorctl can't reach the supervisord daemon. Start it first (e.g. systemctl start supervisor, or supervisord -c /etc/supervisor/supervisord.conf)"
    fi
    [[ -d "$SUPERVISOR_CONF_DIR" ]] || maybe_sudo install -d "$SUPERVISOR_CONF_DIR"
    ;;
  files-only)
    :  # nothing required
    ;;
esac

# ----- obtain binary --------------------------------------------------------
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported arch: $(uname -m)" ;;
  esac
}

download_tarball() {
  need curl
  need tar
  local arch tag asset_url tmp
  arch=$(detect_arch)
  if [[ "$VERSION" == "latest" ]]; then
    tag="latest"
    asset_url=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -oE '"browser_download_url":[[:space:]]*"[^"]+"' \
      | grep -- "_linux_${arch}\.tar\.gz" \
      | head -n1 \
      | sed -E 's/.*"browser_download_url":[[:space:]]*"([^"]+)".*/\1/') \
      || die "could not query github releases for $REPO"
  else
    tag="$VERSION"
    asset_url="https://github.com/$REPO/releases/download/$tag/token-usage_${tag}_linux_${arch}.tar.gz"
  fi
  [[ -n "$asset_url" ]] || die "no linux/$arch release asset found for $REPO ($VERSION)"
  tmp=$(mktemp -d)
  log "downloading $asset_url"
  curl -fsSL "$asset_url" -o "$tmp/release.tar.gz"
  tar -xzf "$tmp/release.tar.gz" -C "$tmp"
  [[ -f "$tmp/$SERVICE_NAME" ]] || die "tarball did not contain $SERVICE_NAME"
  echo "$tmp/$SERVICE_NAME"
}

if [[ -z "$BINARY_PATH" ]]; then
  for candidate in ./bin/$SERVICE_NAME ./$SERVICE_NAME; do
    if [[ -x "$candidate" ]]; then BINARY_PATH="$candidate"; break; fi
  done
fi
if [[ -z "$BINARY_PATH" ]]; then
  [[ -n "$REPO" ]] || die "no local binary found; pass --binary or --repo"
  BINARY_PATH=$(download_tarball)
fi
[[ -x "$BINARY_PATH" ]] || die "binary not executable: $BINARY_PATH"

# ----- install binary + env -------------------------------------------------
log "installing binary to $BIN_PATH"
maybe_sudo install -m 0755 -D "$BINARY_PATH" "$BIN_PATH"
maybe_sudo install -d -m 0700 "$ENV_DIR"

log "writing env file $ENV_PATH (mode 0600, contains api key)"
ENV_CONTENT="# Managed by installer.sh. Edit by hand if you must, then restart the service.
TOKENUSAGE_API_KEY=$API_KEY
TOKENUSAGE_ENDPOINT=$ENDPOINT
"
if (( NEEDS_SUDO )); then
  printf '%s' "$ENV_CONTENT" | sudo tee "$ENV_PATH" >/dev/null
  sudo chmod 0600 "$ENV_PATH"
else
  printf '%s' "$ENV_CONTENT" > "$ENV_PATH"
  chmod 0600 "$ENV_PATH"
fi

# ----- backend-specific final step ------------------------------------------
EXEC_LINE="$BIN_PATH"
[[ -n "$EXTRA_ARGS" ]] && EXEC_LINE="$BIN_PATH $EXTRA_ARGS"

case "$BACKEND" in
  files-only)
    cat <<EOF

[installer] --no-systemd: binary + env file installed, no supervisor wired up.

   Foreground:
       set -a; . "$ENV_PATH"; set +a
       "$BIN_PATH"

   Background with nohup:
       set -a; . "$ENV_PATH"; set +a
       nohup "$BIN_PATH" > /var/log/$SERVICE_NAME.log 2>&1 &

EOF
    exit 0
    ;;

  user|system)
    WANTED_BY="default.target"
    [[ "$BACKEND" == "system" ]] && WANTED_BY="multi-user.target"
    log "writing systemd unit $UNIT_PATH"
    UNIT_CONTENT="[Unit]
Description=Token usage watcher (Claude Code / Codex)
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_PATH
ExecStart=$EXEC_LINE
Restart=on-failure
RestartSec=10s
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true

[Install]
WantedBy=$WANTED_BY
"
    if [[ "$BACKEND" == "system" ]]; then
      printf '%s' "$UNIT_CONTENT" | sudo tee "$UNIT_PATH" >/dev/null
      sudo systemctl daemon-reload
      sudo systemctl enable --now "$SERVICE_NAME.service"
    else
      mkdir -p "$UNIT_DIR"
      printf '%s' "$UNIT_CONTENT" > "$UNIT_PATH"
      systemctl --user daemon-reload
      systemctl --user enable --now "$SERVICE_NAME.service"
    fi
    sleep 1
    SYSCTL=(systemctl); [[ "$BACKEND" == "user" ]] && SYSCTL=(systemctl --user)
    if "${SYSCTL[@]}" is-active --quiet "$SERVICE_NAME.service"; then
      log "service active. Recent log:"
      "${SYSCTL[@]}" status "$SERVICE_NAME.service" --no-pager --lines=12 | sed 's/^/    /'
    else
      "${SYSCTL[@]}" status "$SERVICE_NAME.service" --no-pager --lines=30 | sed 's/^/    /'
      die "installation finished but service failed to start"
    fi
    if [[ "$BACKEND" == "user" ]] \
        && ! loginctl show-user "$USER" 2>/dev/null | grep -q '^Linger=yes'; then
      cat <<EOF

Tip: a user-level systemd service stops when you log out. To keep it
running across logouts, run once:

    sudo loginctl enable-linger $USER

EOF
    fi
    log "done. Manage with:  ${SYSCTL[*]} {status,restart,stop} $SERVICE_NAME"
    ;;

  supervisor)
    log "writing supervisord program $SUPERVISOR_CONF_PATH"
    # The env file is 0600 so we don't embed the API key in the program
    # config (which is typically world-readable). Bash wrapper sources it
    # and exec's into the binary so signals propagate cleanly.
    SUP_CONTENT="[program:$SERVICE_NAME]
command=/bin/bash -c 'set -a; . $ENV_PATH; set +a; exec $EXEC_LINE'
autostart=true
autorestart=true
startsecs=1
startretries=10
stopsignal=TERM
stopwaitsecs=10
user=root
stdout_logfile=/var/log/$SERVICE_NAME.out.log
stderr_logfile=/var/log/$SERVICE_NAME.err.log
stdout_logfile_maxbytes=10MB
stderr_logfile_maxbytes=10MB
stdout_logfile_backups=3
stderr_logfile_backups=3
"
    if (( NEEDS_SUDO )); then
      printf '%s' "$SUP_CONTENT" | sudo tee "$SUPERVISOR_CONF_PATH" >/dev/null
    else
      printf '%s' "$SUP_CONTENT" > "$SUPERVISOR_CONF_PATH"
    fi

    log "reloading supervisord and starting program"
    maybe_sudo supervisorctl reread
    maybe_sudo supervisorctl update

    # Poll for up to ~12s: STARTING is transient, becomes RUNNING after
    # startsecs (1s). Anything else (BACKOFF / FATAL / EXITED) is final.
    state=""
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
      sleep 1
      state=$(maybe_sudo supervisorctl status "$SERVICE_NAME" 2>/dev/null | awk '{print $2}')
      case "$state" in
        RUNNING) break ;;
        STARTING) continue ;;
        *) break ;;
      esac
    done

    if [[ "$state" == "RUNNING" ]]; then
      log "program is RUNNING."
      maybe_sudo supervisorctl status "$SERVICE_NAME" | sed 's/^/    /'
      log "logs:"
      log "  out: /var/log/$SERVICE_NAME.out.log"
      log "  err: /var/log/$SERVICE_NAME.err.log"
    else
      warn "program state: $state (expected RUNNING)"
      maybe_sudo supervisorctl status "$SERVICE_NAME" | sed 's/^/    /'
      echo "--- stderr log tail ---"
      maybe_sudo tail -n 20 "/var/log/$SERVICE_NAME.err.log" 2>&1 | sed 's/^/    /' || true
      echo "--- stdout log tail ---"
      maybe_sudo tail -n 20 "/var/log/$SERVICE_NAME.out.log" 2>&1 | sed 's/^/    /' || true
      die "installation finished but supervisor program did not enter RUNNING"
    fi
    log "done. Manage with:  supervisorctl {status,restart,stop} $SERVICE_NAME"
    ;;
esac
