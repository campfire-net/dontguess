#!/bin/sh
# test/reliability/run.sh — entry point for reliability test suite
#
# Targets:
#   all         — run all reliability tests (default)
#   regression  — wrapper regression tests (wrapper_test.sh)
#   parallel    — parallel buy harness, 50 concurrent, >=94% reach gate (wrapper_parallel.sh)

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TARGET="${1:-all}"

case "$TARGET" in
  regression)
    echo "=== Running wrapper regression tests ==="
    sh "${SCRIPT_DIR}/wrapper_test.sh"
    ;;
  parallel)
    echo "=== Running parallel buy harness ==="
    bash "${SCRIPT_DIR}/wrapper_parallel.sh"
    ;;
  all)
    echo "=== Running all reliability tests ==="
    sh "${SCRIPT_DIR}/wrapper_test.sh"
    bash "${SCRIPT_DIR}/wrapper_parallel.sh"
    ;;
  *)
    echo "Usage: $0 [all|regression|parallel]"
    exit 1
    ;;
esac

echo "=== All requested targets passed ==="
