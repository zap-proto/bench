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

## Reading the output

Allocation counts and wire sizes are counts. They do not move with machine
load and they repeat to the digit across runs. Throughput is a rate, and the
stored run was taken on a machine at a load average of 122–190 across ten
cores. In that run ZAP-HTTP at the 16-byte workload measured 10,408, 796 and
4,018 req/s across three repetitions of the identical loop — a factor of
thirteen — and the concurrent margins between the two fastest arms move by
more than the margin itself between runs. `run.sh` records `uptime` before
and after for exactly that reason. Take the allocation and wire columns as
measurements; take the throughput columns as the conditions allow.

## License

MIT
