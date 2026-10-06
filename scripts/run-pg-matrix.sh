#!/usr/bin/env bash
set -euo pipefail

IMAGE="${PG_IMAGE:-postgres:17}"
PORT="${PG_PORT:-5433}"
CONTAINER_NAME="grizzle-test-$(echo "$IMAGE" | tr ':/' '--')"

echo "==> Starting $IMAGE on port $PORT..."
docker run --rm -d --name "$CONTAINER_NAME" -p "$PORT:5432" \
  -e POSTGRES_PASSWORD=password -e POSTGRES_DB=grizzle_test "$IMAGE" > /dev/null

cleanup() {
  echo "==> Stopping $CONTAINER_NAME..."
  docker stop "$CONTAINER_NAME" > /dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Waiting for PostgreSQL to accept connections..."
for _ in {1..30}; do
  if docker exec "$CONTAINER_NAME" pg_isready -U postgres > /dev/null 2>&1; then
    break
  fi
  sleep 1
done

export DATABASE_URL="postgres://postgres:password@127.0.0.1:$PORT/grizzle_test?sslmode=disable"
export POSTGRES_DSN="$DATABASE_URL"

echo "==> Running integration tests against $IMAGE..."
go test -v -count=1 -run "TestPartialAndFunctionalIndexes_RoundtripIdempotency|TestPartition" .
