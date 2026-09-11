#!/usr/bin/env bash
# Regenerate bench-results.txt. Everything the paper reports comes out of
# this one command, so a claim with no matching line here has no evidence.
#
#   ./run.sh > bench-results.txt 2>&1
set -euo pipefail
cd "$(dirname "$0")"

echo "=== environment ==="
go version
uname -sr
[ "$(uname -s)" = Darwin ] && sysctl -n machdep.cpu.brand_string hw.ncpu || true
# Throughput is a rate and a loaded machine depresses it. Allocation counts
# and wire bytes are not rates and do not move with load. Record the load so
# a reader can tell which columns of a run are worth anything.
uptime
go list -m all | grep -E 'zap-proto|fasthttp' || true

echo
echo "=== allocation + throughput, 3 repetitions ==="
go test -timeout=120m -run=TestMemoryPressure -v -count=3

echo
echo "=== wire bytes ==="
go test -timeout=120m -run=TestWireBytes -v -count=1

echo
echo "=== concurrent throughput, 3 repetitions ==="
go test -timeout=120m -run=TestConcurrentThroughput -v -count=3

echo
echo "=== per-op benchmarks ==="
go test -timeout=120m -run=XXX -bench=. -benchmem -benchtime=2000x -count=3

echo
echo "=== environment after ==="
uptime
