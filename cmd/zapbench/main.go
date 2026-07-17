// Command zapbench is a bombardier-style load generator for the ZAP binary
// transport — the load tool HTTP benchmarkers (bombardier, hey, wrk) can't be,
// because they speak HTTP text and ZAP is a binary frame protocol. It drives a
// zap-proto/http server with N warm keep-alive connections for a fixed
// duration and reports req/sec + latency percentiles + client-side allocs/req,
// so ZAP is measured apples-to-apples with an HTTP tool pointed at the same
// handler.
//
// Design mirrors bombardier: fixed connection count, warm-up window excluded
// from the measurement, per-connection reused request/response (the ZAP
// transport's Do is allocation-free after warm-up, so the loop is too), and a
// merged latency distribution for percentiles.
//
//	zapbench -addr 192.168.77.2:8391 -c 125 -d 10s
//	zapbench -addr 127.0.0.1:8391 -path /v1/chat -m POST -body @payload.json -c 64 -d 10s
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8391", "ZAP server host:port")
	path := flag.String("path", "/health", "request path")
	method := flag.String("m", "GET", "HTTP method")
	bodyArg := flag.String("body", "", "request body, or @file to read from a file")
	conns := flag.Int("c", 125, "number of warm keep-alive connections (concurrency)")
	dur := flag.Duration("d", 10*time.Second, "measurement duration")
	warmup := flag.Duration("warmup", 1*time.Second, "warm-up duration (excluded from results)")
	maxprocs := flag.Int("gomaxprocs", 0, "GOMAXPROCS override (0 = leave as is)")
	flag.Parse()

	if *maxprocs > 0 {
		runtime.GOMAXPROCS(*maxprocs)
	}

	body, err := loadBody(*bodyArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zapbench:", err)
		os.Exit(1)
	}

	r := run(*addr, *method, *path, body, *conns, *warmup, *dur)
	r.print(*addr, *conns, *dur, *method, *path, len(body))
}

// result is the merged outcome of a run.
type result struct {
	ops      int64
	errs     int64
	elapsed  time.Duration
	lat      []time.Duration // merged, unsorted
	allocsPR float64         // client allocs/req during the measured window
	bytesPR  float64         // client bytes/req during the measured window
}

func run(addr, method, path string, body []byte, conns int, warmup, dur time.Duration) result {
	// Phase 1: warm up. Each worker opens its own connection (its own hot conn,
	// no shared-pool mutex — a shared pool would serialize N goroutines and
	// dominate the tail) and loops for the warm-up window, so by measurement
	// every conn is established, buffers are grown, and the codec is hot.
	// Warm-up op counts also size each worker's latency buffer so the measured
	// loop never grows a slice.
	type worker struct {
		t   *zaphttp.Transport
		req *fasthttp.Request
		res *fasthttp.Response
		lat []time.Duration
		ops int64
		err int64
	}
	workers := make([]*worker, conns)

	var wg sync.WaitGroup
	warmDeadline := time.Now().Add(warmup)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t := zaphttp.NewTransport(addr)
			t.SetMaxIdleConns(2)
			t.SetReadTimeout(30 * time.Second)
			req := fasthttp.AcquireRequest()
			res := fasthttp.AcquireResponse()
			req.Header.SetMethod(method)
			req.SetRequestURI(path)
			req.Header.SetHost(addr)
			if len(body) > 0 {
				req.SetBody(body)
			}
			var ops int64
			for time.Now().Before(warmDeadline) {
				if err := t.Do(req, res); err == nil {
					ops++
				}
			}
			workers[i] = &worker{t: t, req: req, res: res, ops: ops}
		}(i)
	}
	wg.Wait()

	// Size each worker's latency buffer from its warm-up rate (× 1.3 headroom)
	// so appends during measurement don't allocate.
	warmSecs := warmup.Seconds()
	if warmSecs <= 0 {
		warmSecs = 1
	}
	for _, w := range workers {
		est := int(float64(w.ops)/warmSecs*dur.Seconds()*1.3) + 1024
		w.lat = make([]time.Duration, 0, est)
	}

	// Phase 2: measure. Snapshot MemStats around the window so client allocs/req
	// is reported (should be ~0 with the zero-alloc codec + reused conn buffers).
	runtime.GC()
	var msBefore, msAfter runtime.MemStats
	runtime.ReadMemStats(&msBefore)

	var totalOps, totalErrs int64
	start := time.Now()
	deadline := start.Add(dur)
	wg.Add(conns)
	for i := 0; i < conns; i++ {
		go func(w *worker) {
			defer wg.Done()
			var ops, errs int64
			for time.Now().Before(deadline) {
				t0 := time.Now()
				if err := w.t.Do(w.req, w.res); err != nil {
					errs++
					continue
				}
				w.lat = append(w.lat, time.Since(t0))
				ops++
			}
			w.ops, w.err = ops, errs
			atomic.AddInt64(&totalOps, ops)
			atomic.AddInt64(&totalErrs, errs)
		}(workers[i])
	}
	wg.Wait()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&msAfter)

	// Merge latencies and release conns.
	var all []time.Duration
	for _, w := range workers {
		all = append(all, w.lat...)
		w.t.CloseIdleConnections()
		fasthttp.ReleaseRequest(w.req)
		fasthttp.ReleaseResponse(w.res)
	}

	res := result{ops: totalOps, errs: totalErrs, elapsed: elapsed, lat: all}
	if totalOps > 0 {
		res.allocsPR = float64(msAfter.Mallocs-msBefore.Mallocs) / float64(totalOps)
		res.bytesPR = float64(msAfter.TotalAlloc-msBefore.TotalAlloc) / float64(totalOps)
	}
	return res
}

func (r result) print(addr string, conns int, dur time.Duration, method, path string, bodyLen int) {
	sort.Slice(r.lat, func(i, j int) bool { return r.lat[i] < r.lat[j] })
	pct := func(p float64) time.Duration {
		if len(r.lat) == 0 {
			return 0
		}
		i := int(float64(len(r.lat)) * p)
		if i >= len(r.lat) {
			i = len(r.lat) - 1
		}
		return r.lat[i]
	}
	var sum time.Duration
	for _, d := range r.lat {
		sum += d
	}
	var mean time.Duration
	if len(r.lat) > 0 {
		mean = sum / time.Duration(len(r.lat))
	}

	fmt.Printf("ZAP  %s %s  addr=%s  c=%d  d=%s\n", method, path, addr, conns, r.elapsed.Round(time.Millisecond))
	if bodyLen > 0 {
		fmt.Printf("  Body      %d B\n", bodyLen)
	}
	fmt.Printf("  Reqs/sec  %.0f\n", float64(r.ops)/r.elapsed.Seconds())
	fmt.Printf("  Requests  %d  (errors %d)\n", r.ops, r.errs)
	fmt.Printf("  Latency   avg=%s  p50=%s  p90=%s  p99=%s  max=%s\n",
		mean.Round(time.Microsecond), pct(0.50).Round(time.Microsecond),
		pct(0.90).Round(time.Microsecond), pct(0.99).Round(time.Microsecond),
		durMax(r.lat).Round(time.Microsecond))
	fmt.Printf("  Client    %.2f allocs/req  %.0f B/req\n", r.allocsPR, r.bytesPR)
}

func durMax(sorted []time.Duration) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}

// loadBody returns the request body: empty, a literal string, or the contents
// of a file when the argument starts with '@'.
func loadBody(arg string) ([]byte, error) {
	if arg == "" {
		return nil, nil
	}
	if strings.HasPrefix(arg, "@") {
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}
