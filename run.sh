#!/usr/bin/env bash
# Regenerate bench-results.txt. Everything the paper reports comes out of
# this one command, so a claim with no matching line here has no evidence.
#
#   ./run.sh > bench-results.txt 2>&1
set -euo pipefail
cd "$(dirname "$0")"

# Allocation counts and wire sizes are counts, and they do not move with load:
# between load 99 and load 208 the allocations-per-request figure never varied
# at all and the bytes-per-request figure varied by four bytes in a hundred and
# thirty thousand. Throughput is a rate, and on a busy box it measures the
# scheduler. So the count sections run always and the rate sections run only on
# a quiet machine, and the file says which it contains. A number missing here
# was not measured; none is ever estimated.
cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
load=$(uptime | sed 's/.*averages*: *//' | tr -d ',' | awk '{print $1}')
# Quiet means most of the machine is idle: a one-minute load under two fifths
# of the core count. On the ten-core M1 Max this run was developed on that is
# a load of four.
limit=$(awk -v c="$cores" 'BEGIN{printf "%.1f", c*0.4}')
quiet=$(awk -v l="$load" -v m="$limit" 'BEGIN{print (l < m) ? "yes" : "no"}')

echo "=== environment ==="
go version
uname -sr
[ "$(uname -s)" = Darwin ] && sysctl -n machdep.cpu.brand_string hw.ncpu || true
uptime
echo "quiet enough for rates: $quiet (load $load, $cores cores, threshold $limit)"
go list -m all | grep -E 'zap-proto|fasthttp' || true

echo
echo "=== allocations, 3 repetitions ==="
go test -timeout=120m -run=TestMemoryPressure -v -count=3

echo
echo "=== wire bytes ==="
go test -timeout=120m -run=TestWireBytes -v -count=1

echo
if [ "$quiet" = yes ]; then
	echo "=== concurrent throughput, 3 repetitions ==="
	go test -timeout=120m -run=TestConcurrentThroughput -v -count=3

	echo
	echo "=== per-op benchmarks ==="
	go test -timeout=120m -run=XXX -bench=. -benchmem -benchtime=2000x -count=3
else
	echo "=== concurrent throughput: NOT MEASURED ==="
	echo "Load average was $load on $cores cores when this run started. A rate"
	echo "taken there is scheduler contention, not transport cost. Re-run on a"
	echo "quiet machine to fill this section in."
	echo
	echo "=== per-op benchmarks: NOT MEASURED ==="
	echo "Same reason. The B/op and allocs/op columns these would report are"
	echo "already covered by the allocation section above, which is valid at"
	echo "any load; only the ns/op column needs a quiet box, so the whole"
	echo "section waits for one."
fi

echo
echo "=== environment after ==="
uptime
