# zap-proto/bench

> **Docs:** [ZAP-HTTP benchmark](https://zap-proto.dev/docs/benchmarks) · part of the [ZAP Protocol](https://zap-proto.io)

Reproducible memory + latency benchmark comparing **ZAP-HTTP** to Go's `net/http`. Single TCP connection, request loop, p50/p95/p99/p999, RSS samples.

## Run

```bash
go install github.com/zap-proto/bench/cmd/zapbench@latest
zapbench -duration 30s -conns 64 -body 1KiB
```

## Results

See full results on [zap-proto.dev/docs/benchmarks](https://zap-proto.dev/docs/benchmarks). TL;DR on commodity hardware (AMD 9950X, Linux 6.10):

| Metric | net/http | zap-http | ratio |
|---|---|---|---|
| p50 latency | 110 µs | **18 µs** | 6.1× |
| p99 latency | 1.2 ms | **62 µs** | 19× |
| RSS / 1k conn | 142 MiB | **23 MiB** | 6.2× |
| req/s (1 core) | 184k | **1.1M** | 6.0× |

## Methodology

- Single physical host, taskset to a dedicated core
- Both servers identically configured for keep-alive
- Same `BenchmarkBody` payload, same Content-Type
- 60-sample warm-up, 30-sample measurement window
- Numbers are reproducible — see `cmd/zapbench/README.md` for the exact reproduce script

## License

MIT
