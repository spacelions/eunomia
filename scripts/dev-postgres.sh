#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PGDATA="$ROOT_DIR/.dev/pgdata"
PGRUN="$ROOT_DIR/.dev/pgrun"
PGPORT="${PGPORT:-54329}"
PGUSER="${PGUSER:-postgres}"

mkdir -p "$PGDATA" "$PGRUN"

if [ ! -f "$PGDATA/PG_VERSION" ]; then
  echo "Initializing ephemeral PostgreSQL data directory at $PGDATA..."
  initdb \
    -D "$PGDATA" \
    -U "$PGUSER" \
    --auth=trust \
    --encoding=UTF8 \
    --locale=C \
    --no-instructions
fi

exec postgres \
  -D "$PGDATA" \
  -k "$PGRUN" \
  -h 127.0.0.1 \
  -p "$PGPORT"
