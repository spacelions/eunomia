#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIN_COVERAGE="${EUNOMIA_MIN_COVERAGE:-95.0}"
VET_ONLY=false

if [ "${1:-}" = "--vet-only" ]; then
  VET_ONLY=true
fi

# Ensure we run with the Nix-pinned Go & PostgreSQL toolchain even if git commit
# is invoked outside `nix develop` (e.g., from an IDE or plain shell).
if ! command -v postgres >/dev/null 2>&1 || [ -n "${GOROOT:-}" ]; then
  NIX_BIN="$(command -v nix || echo /nix/var/nix/profiles/default/bin/nix)"
  if [ -x "$NIX_BIN" ]; then
    exec "$NIX_BIN" develop "$ROOT_DIR" --command bash "$ROOT_DIR/scripts/check-coverage.sh" "$@"
  fi
fi

cd "$ROOT_DIR"

if [ "$VET_ONLY" = true ]; then
  exec go vet ./...
fi

TMP_RAW="$(mktemp)"
TMP_FILTERED="$(mktemp)"
trap 'rm -f "$TMP_RAW" "$TMP_FILTERED"' EXIT

# Exclude test-only infrastructure (test/) from coverage accounting.
go test -race -coverprofile="$TMP_RAW" ./...

# Filter out test/ helpers and cmd/eunomia/main.go (4-line os.Exit wrapper).
grep -v -E '/test/|/cmd/eunomia/main\.go:' "$TMP_RAW" > "$TMP_FILTERED"

TOTAL_LINE="$(go tool cover -func="$TMP_FILTERED" | tail -n 1)"
TOTAL_PCT="$(echo "$TOTAL_LINE" | awk '{gsub(/%/, "", $3); print $3}')"

echo "Total statement coverage: ${TOTAL_PCT}% (minimum required: ${MIN_COVERAGE}%)"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Go Test Coverage"
    echo ""
    echo "- **Total statement coverage:** \`${TOTAL_PCT}%\`"
    echo "- **Minimum required:** \`${MIN_COVERAGE}%\`"
  } >> "$GITHUB_STEP_SUMMARY"
fi

awk -v got="$TOTAL_PCT" -v min="$MIN_COVERAGE" 'BEGIN {
  if (got + 0 < min + 0) {
    printf "FAIL: coverage %.1f%% is below required %.1f%%\n", got, min > "/dev/stderr"
    exit 1
  }
}'
