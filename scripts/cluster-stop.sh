#!/usr/bin/env bash
# cluster-stop.sh — Stop the local 3-node cluster started by cluster-local.sh.
#
# Usage:
#   ./scripts/cluster-stop.sh           # stop processes, keep data
#   ./scripts/cluster-stop.sh --clean   # stop and remove data-node{1,2,3}/

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PID_FILE="$ROOT/.cluster-pids"
LOG_DIR="$ROOT/.cluster-logs"

stop_by_pidfile() {
  [ -f "$PID_FILE" ] || return 0
  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    if kill -0 "$pid" 2>/dev/null; then
      echo "==> killing pid $pid"
      kill "$pid" 2>/dev/null || true
    fi
  done < "$PID_FILE"
  # Give them a moment to exit, then SIGKILL stragglers.
  sleep 0.3
  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  done < "$PID_FILE"
  rm -f "$PID_FILE"
}

# Fallback: kill any paladin-core cluster process if pid file is missing.
stop_by_pattern() {
  pkill -f "paladin-core cluster --id node[123]" 2>/dev/null || true
}

stop_by_pidfile
stop_by_pattern

echo "Cluster stopped."

if [ "${1:-}" = "--clean" ]; then
  rm -rf "$ROOT/data-node1" "$ROOT/data-node2" "$ROOT/data-node3" "$LOG_DIR"
  echo "Wiped data-node{1,2,3}/ and $LOG_DIR."
fi
