#!/usr/bin/env bash
set -euo pipefail

if [ -n "${PG_IMAGE:-}" ]; then
  IMAGES=("$PG_IMAGE")
else
  IMAGES=("postgres:14" "postgres:15" "postgres:16" "postgres:17")
fi

PORT="${PG_PORT:-5433}"

for IMAGE in "${IMAGES[@]}"; do
  CONTAINER_NAME="grizzle-test-$(echo "$IMAGE" | tr ':/' '--')"

  echo "================================================================================"
  echo "==> Starting $IMAGE on port $PORT..."
  echo "================================================================================"
  docker run --rm -d --name "$CONTAINER_NAME" -p "$PORT:5432" \
    -e POSTGRES_PASSWORD=password -e POSTGRES_DB=grizzle_test "$IMAGE" > /dev/null

  cleanup() {
    docker stop "$CONTAINER_NAME" > /dev/null 2>&1 || true
  }
  trap cleanup EXIT

  echo "==> Waiting for $IMAGE to accept connections..."
  for _ in {1..30}; do
    if docker exec "$CONTAINER_NAME" pg_isready -U postgres > /dev/null 2>&1; then
      break
    fi
    sleep 1
  done

  export DATABASE_URL="postgres://postgres:password@127.0.0.1:$PORT/grizzle_test?sslmode=disable"
  export POSTGRES_DSN="$DATABASE_URL"

  echo "==> Running ALL integration tests against $IMAGE..."
  go test -tags integration -count=1 ./...

  echo "==> Stopping $CONTAINER_NAME..."
  docker stop "$CONTAINER_NAME" > /dev/null 2>&1 || true
  trap - EXIT
done
