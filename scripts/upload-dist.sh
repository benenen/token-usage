#!/usr/bin/env bash
# Upload every artifact under dist/ to a remote host via scp.
#
# Usage:
#   scripts/upload-dist.sh user@host:/remote/path
#   UPLOAD_TARGET=user@host:/remote/path scripts/upload-dist.sh
#
# Optional env:
#   DIST_DIR    local source dir (default: dist)
#   SSH_PORT    remote ssh port  (default: 22)
#   SSH_KEY     identity file to pass to scp -i
set -euo pipefail

TARGET="${1:-${UPLOAD_TARGET:-}}"
if [ -z "$TARGET" ]; then
    echo "usage: $0 user@host:/remote/path  (or set UPLOAD_TARGET)" >&2
    exit 2
fi
case "$TARGET" in
    *:*) ;;
    *) echo "target must be of form user@host:/path (got: $TARGET)" >&2; exit 2 ;;
esac

DIST_DIR="${DIST_DIR:-dist}"
SSH_PORT="${SSH_PORT:-22}"

if [ ! -d "$DIST_DIR" ]; then
    echo "$DIST_DIR does not exist — run 'make release' first" >&2
    exit 1
fi
shopt -s nullglob
files=("$DIST_DIR"/*.tar.gz "$DIST_DIR"/*.zip "$DIST_DIR"/SHA256SUMS)
if [ ${#files[@]} -eq 0 ]; then
    echo "$DIST_DIR is empty — run 'make release' first" >&2
    exit 1
fi

scp_args=(-P "$SSH_PORT")
[ -n "${SSH_KEY:-}" ] && scp_args+=(-i "$SSH_KEY")

echo "==> uploading ${#files[@]} file(s) to $TARGET"
for f in "${files[@]}"; do
    printf '    %s\n' "$(basename "$f")"
done
scp "${scp_args[@]}" "${files[@]}" "$TARGET"
echo "==> done"
