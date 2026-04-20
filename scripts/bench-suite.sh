#!/usr/bin/env bash
# bench-suite.sh — run the canonical PaladinCore benchmark sweep.
#
# Prerequisites: a 3-node cluster on 127.0.0.1:{8080,8081,8082}.
# Start it with:
#   ./scripts/cluster-local.sh --fresh
#
# Usage:
#   ./scripts/bench-suite.sh                         # default sweep
#   DURATION=10s CONCS="1 8 64" ./scripts/bench-suite.sh
#
# Output:
#   bench/results/<timestamp>_<scenario>_c<conc>.json
#   bench/results/<timestamp>_report.md

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/paladin-bench"
OUT="$ROOT/bench/results"
TS="$(date +%Y%m%d-%H%M%S)"

DURATION="${DURATION:-30s}"
WARMUP="${WARMUP:-3s}"
ADDRS="${ADDRS:-127.0.0.1:8080,127.0.0.1:8081,127.0.0.1:8082}"
CONCS="${CONCS:-1 8 32 128}"

mkdir -p "$OUT"

# --- build the bench binary if needed ---
MAIN="$ROOT/cmd/paladin-bench/main.go"
if [ ! -x "$BIN" ] || find "$ROOT/cmd/paladin-bench" "$ROOT/bench" -type f -newer "$BIN" 2>/dev/null | grep -q .; then
  echo "==> building paladin-bench"
  (cd "$ROOT" && go build -o paladin-bench ./cmd/paladin-bench)
fi

# --- quick reachability check ---
if ! curl -sf "http://${ADDRS%%,*}/healthz" >/dev/null; then
  echo "cluster not reachable at ${ADDRS%%,*}; start it with ./scripts/cluster-local.sh" >&2
  exit 1
fi

run() {
  local label="$1"; shift
  local json="$OUT/${TS}_${label}.json"
  echo "==> $label"
  "$BIN" run \
    --addrs "$ADDRS" \
    --duration "$DURATION" \
    --warmup "$WARMUP" \
    --json "$json" \
    "$@"
  echo "    -> $json"
}

# Sweep concurrency on every workload to find each one's knee point.
for c in $CONCS; do
  run "write_only_c${c}"  --scenario=write_only  --concurrency=$c
  run "read_only_c${c}"   --scenario=read_only   --concurrency=$c
  run "mixed95_c${c}"     --scenario=mixed --read-percent=95 --concurrency=$c
done

REPORT="$OUT/${TS}_report.md"
"$BIN" report --in "$OUT" --out "$REPORT" \
  --title "PaladinCore Benchmark Sweep — $TS"

echo
echo "Report:  $REPORT"
echo "Raw:     $OUT/${TS}_*.json"
