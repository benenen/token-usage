#!/usr/bin/env bash
# token-usage-watcher installer
#
# Installs the watcher binary and a systemd unit that auto-starts on boot.
# Defaults to a USER-level service (no root, runs as $USER, reads $HOME/.claude
# and $HOME/.codex). Pass --system for a root-installed system service.
#
# Usage:
#   ./installer.sh --api-key tuk_xxx --endpoint http://server:8080/ingest
#   ./installer.sh --uninstall
#
#   # From a release, no local binary:
#   ./installer.sh --repo owner/repo --api-key tuk_... --endpoint http://...
#
#   # Pipe install (paste this on the dev's machine):
#   curl -fsSL https://example.com/installer.sh | bash -s -- \
#       --repo owner/repo --api-key tuk_... --endpoint http://server:8080/ingest

set -euo pipefail

SERVICE_NAME="token-usage-watcher"
SCOPE="user"          # user | system
API_KEY=""
ENDPOINT=""
EXTRA_ARGS=""         # forwarded to the watcher (e.g. "--source x=/y")
BINARY_PATH=""        # if set, install this file instead of downloading
REPO=""               # owner/repo on GitHub; used when no local binary
VERSION="latest"      # tag name; "latest" → /releases/latest
DO_UNINSTALL=0

usage() {
  cat <<EOF
Usage: $0 [options]

Required (unless --uninstall):
  --api-key  KEY         API key minted by token-usage-server admin
  --endpoint URL         server /ingest URL  (e.g. http://server:8080/ingest)

Source of the binary (pick one; if none, --repo is required):
  --binary  PATH         use a local watcher binary
  --repo    OWNER/REPO   download latest release tarball from github.com
  --version TAG          with --repo, install this tag instead of latest

Service scope:
  --system               install as root-level systemd service
  --user                 install as user-level service (default)

Other:
  --extra-args "..."     extra flags appended to ExecStart (quoted)
  --uninstall            stop, disable, and remove the service
  -h, --help             show this help
EOF
}

log()  { printf '\033[36m[installer]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# ----- arg parsing ----------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key)    API_KEY="${2:-}"; shift 2 ;;
    --endpoint)   ENDPOINT="${2:-}"; shift 2 ;;
    --binary)     BINARY_PATH="${2:-}"; shift 2 ;;
    --repo)       REPO="${2:-}"; shift 2 ;;
    --version)    VERSION="${2:-}"; shift 2 ;;
    --extra-args) EXTRA_ARGS="${2:-}"; shift 2 ;;
    --system)     SCOPE=system; shift ;;
    --user)       SCOPE=user; shift ;;
    --uninstall)  DO_UNINSTALL=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown flag: $1 (use --help)" ;;
  esac
done

# ----- shell helpers --------------------------------------------------------
need() { command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }

systemctl_cmd() {
  if [[ "$SCOPE" == "system" ]]; then
    sudo systemctl "$@"
  else
    systemctl --user "$@"
  fi
}

# ----- platform paths -------------------------------------------------------
if [[ "$SCOPE" == "system" ]]; then
  BIN_DIR=/usr/local/bin
  UNIT_DIR=/etc/systemd/system
  ENV_DIR=/etc/token-usage-watcher
  RUN_AS_USER=""    # systemd default = root; override below if needed
else
  BIN_DIR="$HOME/.local/bin"
  UNIT_DIR="$HOME/.config/systemd/user"
  ENV_DIR="$HOME/.config/token-usage-watcher"
fi
BIN_PATH="$BIN_DIR/$SERVICE_NAME"
UNIT_PATH="$UNIT_DIR/$SERVICE_NAME.service"
ENV_PATH="$ENV_DIR/env"

# ----- uninstall path -------------------------------------------------------
if (( DO_UNINSTALL )); then
  log "stopping and disabling $SERVICE_NAME ($SCOPE)"
  systemctl_cmd stop    "$SERVICE_NAME.service" 2>/dev/null || true
  systemctl_cmd disable "$SERVICE_NAME.service" 2>/dev/null || true
  if [[ "$SCOPE" == "system" ]]; then
    sudo rm -f "$UNIT_PATH" "$BIN_PATH"
    sudo rm -rf "$ENV_DIR"
  else
    rm -f "$UNIT_PATH" "$BIN_PATH"
    rm -rf "$ENV_DIR"
  fi
  systemctl_cmd daemon-reload
  log "uninstalled. Checkpoint at \$HOME/.token-usage-watcher/ left untouched."
  exit 0
fi

# ----- install pre-flight ---------------------------------------------------
need systemctl
[[ "$(uname -s)" == "Linux" ]] || die "this installer is Linux/systemd only (on macOS use launchd)"
[[ -n "$API_KEY" ]]  || die "--api-key is required"
[[ -n "$ENDPOINT" ]] || die "--endpoint is required"

# ----- obtain binary --------------------------------------------------------
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
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
  # look for a local build first
  for candidate in ./bin/$SERVICE_NAME ./$SERVICE_NAME; do
    if [[ -x "$candidate" ]]; then BINARY_PATH="$candidate"; break; fi
  done
fi
if [[ -z "$BINARY_PATH" ]]; then
  [[ -n "$REPO" ]] || die "no local binary found; pass --binary or --repo"
  BINARY_PATH=$(download_tarball)
fi
[[ -x "$BINARY_PATH" ]] || die "binary not executable: $BINARY_PATH"

# ----- install binary + env + unit ------------------------------------------
log "installing binary to $BIN_PATH"
if [[ "$SCOPE" == "system" ]]; then
  sudo install -m 0755 -D "$BINARY_PATH" "$BIN_PATH"
  sudo install -d -m 0750 "$ENV_DIR"
else
  install -m 0755 -D "$BINARY_PATH" "$BIN_PATH"
  install -d -m 0700 "$ENV_DIR"
fi

log "writing env file $ENV_PATH (mode 0600, contains api key)"
ENV_CONTENT=$(cat <<EOF
# Managed by installer.sh. Edit by hand if you must, then:
#   systemctl ${SCOPE:+--user} restart $SERVICE_NAME
TOKENUSAGE_API_KEY=$API_KEY
TOKENUSAGE_ENDPOINT=$ENDPOINT
EOF
)
if [[ "$SCOPE" == "system" ]]; then
  echo "$ENV_CONTENT" | sudo tee "$ENV_PATH" >/dev/null
  sudo chmod 0600 "$ENV_PATH"
else
  printf '%s\n' "$ENV_CONTENT" > "$ENV_PATH"
  chmod 0600 "$ENV_PATH"
fi

log "writing systemd unit $UNIT_PATH"
EXEC_LINE="$BIN_PATH"
if [[ -n "$EXTRA_ARGS" ]]; then
  EXEC_LINE="$BIN_PATH $EXTRA_ARGS"
fi

if [[ "$SCOPE" == "system" ]]; then
  WANTED_BY="multi-user.target"
else
  WANTED_BY="default.target"
fi

UNIT_CONTENT=$(cat <<EOF
[Unit]
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
# Soft sandbox; the watcher only needs to read JSONLs + write its own state dir.
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true

[Install]
WantedBy=$WANTED_BY
EOF
)
if [[ "$SCOPE" == "system" ]]; then
  echo "$UNIT_CONTENT" | sudo tee "$UNIT_PATH" >/dev/null
else
  mkdir -p "$UNIT_DIR"
  printf '%s\n' "$UNIT_CONTENT" > "$UNIT_PATH"
fi

# ----- enable + start --------------------------------------------------------
log "reloading systemd and starting service"
systemctl_cmd daemon-reload
systemctl_cmd enable --now "$SERVICE_NAME.service"

# ----- verify + tail ---------------------------------------------------------
sleep 1
if systemctl_cmd is-active --quiet "$SERVICE_NAME.service"; then
  log "service is active. Recent log:"
  systemctl_cmd status "$SERVICE_NAME.service" --no-pager --lines=12 | sed 's/^/    /'
else
  warn "service did NOT enter active state. Full status:"
  systemctl_cmd status "$SERVICE_NAME.service" --no-pager --lines=30 | sed 's/^/    /'
  die "installation completed but service failed to start"
fi

if [[ "$SCOPE" == "user" ]]; then
  if ! loginctl show-user "$USER" 2>/dev/null | grep -q '^Linger=yes'; then
    cat <<EOF

Tip: by default a user-level systemd service stops when you log out.
To keep the watcher running across logouts:

    sudo loginctl enable-linger $USER

EOF
  fi
fi
log "done. Manage with:  systemctl ${SCOPE:+--user} {status,restart,stop} $SERVICE_NAME"
