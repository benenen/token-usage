#!/usr/bin/env bash
# Push a locally-built Docker image to a Harbor registry.
#
# Usage:
#   scripts/push-image.sh harbor.example.com/myproject
#   scripts/push-image.sh harbor.example.com/myproject v1.2.3
#   HARBOR_TARGET=harbor.example.com/myproject scripts/push-image.sh
#
# Defaults mirror the Makefile: IMAGE_NAME=token-usage-server, IMAGE_TAG=dev.
# The remote image becomes ${HARBOR_TARGET}/${IMAGE_NAME}:${TAG}.
#
# Optional env:
#   IMAGE_NAME      local image repo (default: token-usage-server)
#   IMAGE_TAG       local image tag  (default: dev) — i.e. source = $IMAGE_NAME:$IMAGE_TAG
#   TAG             remote tag (default: $IMAGE_TAG, or $2 if given)
#   HARBOR_USER     if set together with HARBOR_PASSWORD, runs `docker login`
#   HARBOR_PASSWORD
#   ALSO_LATEST=1   also tag + push :latest
set -euo pipefail

TARGET="${1:-${HARBOR_TARGET:-}}"
if [ -z "$TARGET" ]; then
    echo "usage: $0 <harbor-host>/<project> [tag]  (or set HARBOR_TARGET)" >&2
    exit 2
fi
case "$TARGET" in
    */*) ;;
    *) echo "target must be of form harbor-host/project (got: $TARGET)" >&2; exit 2 ;;
esac

IMAGE_NAME="${IMAGE_NAME:-token-usage-server}"
IMAGE_TAG="${IMAGE_TAG:-dev}"
SOURCE="${IMAGE_NAME}:${IMAGE_TAG}"
TAG="${2:-${TAG:-$IMAGE_TAG}}"
REMOTE="${TARGET}/${IMAGE_NAME}:${TAG}"

if ! docker image inspect "$SOURCE" >/dev/null 2>&1; then
    echo "local image $SOURCE not found — run 'make docker' first" >&2
    exit 1
fi

if [ -n "${HARBOR_USER:-}" ] && [ -n "${HARBOR_PASSWORD:-}" ]; then
    HARBOR_HOST="${TARGET%%/*}"
    echo "==> docker login $HARBOR_HOST"
    printf '%s' "$HARBOR_PASSWORD" | docker login "$HARBOR_HOST" -u "$HARBOR_USER" --password-stdin
fi

echo "==> tag $SOURCE -> $REMOTE"
docker tag "$SOURCE" "$REMOTE"
echo "==> push $REMOTE"
docker push "$REMOTE"

if [ "${ALSO_LATEST:-}" = "1" ]; then
    LATEST="${TARGET}/${IMAGE_NAME}:latest"
    echo "==> tag $SOURCE -> $LATEST"
    docker tag "$SOURCE" "$LATEST"
    echo "==> push $LATEST"
    docker push "$LATEST"
fi
echo "==> done"
