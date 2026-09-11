# zap-proto/bench

Allocation, wire-size and throughput comparison for the ZAP wire against
HTTP/1.1, on loopback, in one Go test binary.

Five arms answer the same request — a body plus a header set, echoed back
with the `X-` headers — over a warm connection:

| arm | client + server | wire |
|---|---|---|
| `net/http` | Go stdlib | HTTP/1.1 |
| `fasthttp` | fasthttp | HTTP/1.1 |
| `ZAP-HTTP` | fasthttp objects, `zap-proto/http` | ZAP |
| `native ZAP` | `zap-proto/go` typed message | ZAP, length-prefixed |
| `floor` | a byte echo, no protocol | length-prefixed |

The `fasthttp` arm is what makes the ZAP number readable. fasthttp's object
model is far cheaper than `net/http`'s on any wire, so a ZAP-vs-`net/http`
ratio moves for two reasons at once. `ZAP-HTTP` against `fasthttp` isolates
the wire; against `net/http` it says what a stdlib service would see if it
moved. `floor` bounds everything from below: it is a lower bound on transport
cost, not a protocol, and no number from it describes ZAP.

## Run

```bash
./run.sh > bench-results.txt 2>&1
```

`bench-results.txt` in this repository is the output of that command, in
full, including the machine it ran on. A claim with no matching line in it
has no evidence behind it.

Every dependency is a published version, so a clone and `./run.sh` is the
whole reproduction.

## Load generator

`cmd/zapbench` drives a running `zap-proto/http` server the way bombardier
drives an HTTP one — fixed warm keep-alive connections, a warm-up window
excluded from the measurement, req/s with latency percentiles and client
allocations per request. HTTP load tools cannot point at a ZAP server because
they speak HTTP text and ZAP is a binary frame.

```bash
go run ./cmd/zapbench -addr 127.0.0.1:8391 -c 64 -d 10s
```

Its numbers are not in `bench-results.txt`: it measures a server you started,
not the in-process arms above.

## Counts always, rates only on a quiet machine

Allocation counts and wire sizes are counts. They do not move with machine
load. Across runs taken between load 99 and load 208 on the same box, the
native arm reported 6.0 allocations per round trip in every repetition
without exception, its byte figure never moved at the 16-byte workload, and
at 64 KiB it spanned 131 264.8 to 131 268.9 — four bytes in a hundred and
thirty thousand, which is `MemStats` granularity rather than an effect of
load. Wire sizes are byte-identical every run. Those sections run
unconditionally.

Throughput is a rate, and on a loaded machine it measures the scheduler. The
run that first carried a throughput table here was taken at load 122–190 on
ten cores, where the same 5,000-request loop returned 10,408, 796 and 4,018
req/s in three consecutive repetitions — a factor of thirteen on identical
work. Ratios did not survive it either: ZAP-HTTP against fasthttp came out
1.32×, 0.93× and 1.28× at 256 B. That is not a close contest between two
libraries, it is a busy CPU, and there is no way to read a transport result
out of it.

So `run.sh` checks the one-minute load average before it starts and skips the
rate sections above two fifths of the core count — load 4 on a ten-core box.
When it skips them the file says `NOT MEASURED` and why, in place of the
table. A number absent from `bench-results.txt` was not measured; nothing
here is ever estimated or carried over from an earlier run.

## License

MIT
