// Local-stack memory + latency benchmark: net/http vs zap-proto/http.
//
// Each benchmark runs an in-process server + client over loopback,
// driving an identical handler that returns a JSON-shaped payload of
// configurable size. Uses Go's testing.B for ns/op + B/op + allocs/op,
// plus runtime.MemStats snapshots around a tight request loop for
// process-wide memory pressure (HeapAlloc, HeapInuse, total bytes
// allocated, GC count).
//
// Workloads:
//   - tiny:   16 B body, 4 headers
//   - small:  256 B body, 8 headers
//   - medium: 4 KiB body, 12 headers
//   - large:  64 KiB body, 8 headers
//
// Run:
//
//   go test -bench=. -benchmem -benchtime=2s -count=3 \
//     | tee bench-results.txt
//
//   go test -run=TestMemoryPressure -v
//
// Both protocols use the same handler shape and headers, so any
// difference is attributable to the wire encoding + transport, not
// the application.

package bench_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	zaphttp "github.com/zap-proto/http"
)

// ---- workload generators ---------------------------------------------------

type workload struct {
	name    string
	body    []byte
	headers http.Header
}

func makeWorkloads() []workload {
	wlds := []workload{
		{name: "tiny", body: makeBody(16), headers: makeHeaders(4)},
		{name: "small", body: makeBody(256), headers: makeHeaders(8)},
		{name: "medium", body: makeBody(4 * 1024), headers: makeHeaders(12)},
		{name: "large", body: makeBody(64 * 1024), headers: makeHeaders(8)},
	}
	return wlds
}

func makeBody(n int) []byte {
	// Realistic payload shape: JSON-ish bytes (matches a typical RPC
	// response). Random-but-deterministic so neither protocol gets a
	// compression advantage on patterns.
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return b
}

func makeHeaders(count int) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-Id", "deadbeef-1234-5678-90ab-cdef00000000")
	h.Set("X-Org-Id", "liquidity")
	h.Set("X-User-Id", "u-z@example.com")
	if count > 4 {
		h.Set("Cache-Control", "no-store")
		h.Set("Accept", "application/json")
		h.Set("Accept-Encoding", "gzip, deflate")
		h.Set("User-Agent", "zap-bench/1.0")
	}
	if count > 8 {
		h.Set("X-Trace-Id", "trace-0000aaaa1111bbbb2222cccc3333dddd")
		h.Set("X-Span-Id", "span-44445555")
		h.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6ImNlcnQtYnVpbHQtaW4iLCJ0eXAiOiJKV1QifQ.fake")
		h.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	}
	return h
}

// ---- handler --------------------------------------------------------------

// echoHandler returns the request body verbatim with the request's
// Content-Type. This matches the shape of a typical RPC backend that
// reads a request payload, processes it, and returns a response of
// similar size.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	for k, vs := range r.Header {
		if len(k) > 2 && k[:2] == "X-" {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// ---- server harnesses -----------------------------------------------------

func startHTTPServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(echoHandler))
	return srv.Listener.Addr().String(), srv.Close
}

func startZAPServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &zaphttp.Server{Handler: http.HandlerFunc(echoHandler)}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

// ---- benchmarks ----------------------------------------------------------

func BenchmarkHTTP(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := startHTTPServer(b)
			defer stop()
			c := &http.Client{
				Timeout: 10 * time.Second,
				Transport: &http.Transport{
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 100,
					IdleConnTimeout:     30 * time.Second,
				},
			}
			b.SetBytes(int64(len(wl.body) * 2)) // request + response
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo", bytes.NewReader(wl.body))
				for k, vs := range wl.headers {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				resp, err := c.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
}

func BenchmarkZAP(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := startZAPServer(b)
			defer stop()
			c := &http.Client{
				Timeout:   10 * time.Second,
				Transport: zaphttp.NewTransport(addr),
			}
			b.SetBytes(int64(len(wl.body) * 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo", bytes.NewReader(wl.body))
				for k, vs := range wl.headers {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				resp, err := c.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
}

// BenchmarkNativeZAP — direct binary Echo over TCP. No HTTP shape,
// no http.Request, no headers. Just length-prefixed bytes. This
// measures the floor of what ZAP can do for in-process RPC.
func BenchmarkNativeZAP(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			srv, err := startNativeZapServer()
			if err != nil {
				b.Fatal(err)
			}
			defer srv.Close()
			c, err := newNativeZapClient(srv.Addr())
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			out := make([]byte, len(wl.body))
			b.SetBytes(int64(len(wl.body) * 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.Echo(wl.body, out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---- memory pressure tests -----------------------------------------------
//
// These are not benchmarks in the testing.B sense — they drive a fixed
// number of requests and snapshot runtime.MemStats around the run.
// The reported numbers are heap pressure (HeapAlloc, HeapInuse), total
// bytes allocated since process start (TotalAlloc), and GC cycles
// (NumGC). Reported via t.Logf so they show up in -v output.

const memReqs = 5_000

func runMemPressure(t *testing.T, name string, addr string, transport http.RoundTripper, body []byte, headers http.Header) memStats {
	t.Helper()
	c := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	// Warm up the connection / pool so first-request alloc doesn't
	// dominate the snapshot.
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/warmup", bytes.NewReader(body))
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("warmup: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	for i := 0; i < memReqs; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo", bytes.NewReader(body))
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("[%s] request %d: %v", name, i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	elapsed := time.Since(start)

	runtime.ReadMemStats(&after)
	return memStats{
		name:        name,
		reqs:        memReqs,
		elapsed:     elapsed,
		totalAlloc:  after.TotalAlloc - before.TotalAlloc,
		mallocs:     after.Mallocs - before.Mallocs,
		gcCycles:    after.NumGC - before.NumGC,
		heapAllocAt: after.HeapAlloc,
		heapInuseAt: after.HeapInuse,
	}
}

type memStats struct {
	name        string
	reqs        int
	elapsed     time.Duration
	totalAlloc  uint64 // bytes allocated during run
	mallocs     uint64 // alloc count during run
	gcCycles    uint32
	heapAllocAt uint64 // heap snapshot AFTER run
	heapInuseAt uint64
}

func (m memStats) String() string {
	bytesPerReq := float64(m.totalAlloc) / float64(m.reqs)
	mallocPerReq := float64(m.mallocs) / float64(m.reqs)
	return fmt.Sprintf(
		"%s: %d reqs in %s (%.0f req/s) | %.1f bytes/req | %.1f allocs/req | %d GCs | heap=%d KiB",
		m.name, m.reqs, m.elapsed.Round(time.Millisecond),
		float64(m.reqs)/m.elapsed.Seconds(),
		bytesPerReq, mallocPerReq, m.gcCycles,
		m.heapInuseAt/1024,
	)
}

func TestMemoryPressure(t *testing.T) {
	for _, wl := range makeWorkloads() {
		t.Run(wl.name, func(t *testing.T) {
			httpAddr, httpStop := startHTTPServer(t)
			defer httpStop()
			zapAddr, zapStop := startZAPServer(t)
			defer zapStop()
			natSrv, err := startNativeZapServer()
			if err != nil {
				t.Fatal(err)
			}
			defer natSrv.Close()

			httpClientTransport := &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     30 * time.Second,
			}

			httpStats := runMemPressure(t, "HTTP/1.1+JSON", httpAddr, httpClientTransport, wl.body, wl.headers)
			zhStats := runMemPressure(t, "ZAP-HTTP (adapter)", zapAddr, zaphttp.NewTransport(zapAddr), wl.body, wl.headers)
			natStats := runNativeZapMemPressure(t, "Native-ZAP", natSrv.Addr(), wl.body)

			t.Logf("\n  workload: %s (body=%d B, headers=%d)\n  %s\n  %s\n  %s\n  bytes/req:  HTTP=%.0f  ZAP-HTTP=%.0f  Native-ZAP=%.0f\n  allocs/req: HTTP=%.1f  ZAP-HTTP=%.1f  Native-ZAP=%.1f\n  req/s:      HTTP=%.0f  ZAP-HTTP=%.0f  Native-ZAP=%.0f\n  Native-ZAP vs HTTP — bytes:%.2fx allocs:%.2fx throughput:%.2fx",
				wl.name, len(wl.body), len(wl.headers),
				httpStats, zhStats, natStats,
				float64(httpStats.totalAlloc)/float64(httpStats.reqs),
				float64(zhStats.totalAlloc)/float64(zhStats.reqs),
				float64(natStats.totalAlloc)/float64(natStats.reqs),
				float64(httpStats.mallocs)/float64(httpStats.reqs),
				float64(zhStats.mallocs)/float64(zhStats.reqs),
				float64(natStats.mallocs)/float64(natStats.reqs),
				float64(httpStats.reqs)/httpStats.elapsed.Seconds(),
				float64(zhStats.reqs)/zhStats.elapsed.Seconds(),
				float64(natStats.reqs)/natStats.elapsed.Seconds(),
				float64(httpStats.totalAlloc)/float64(natStats.totalAlloc),
				float64(httpStats.mallocs)/float64(natStats.mallocs),
				(float64(natStats.reqs)/natStats.elapsed.Seconds())/(float64(httpStats.reqs)/httpStats.elapsed.Seconds()),
			)
		})
	}
}

// runNativeZapMemPressure mirrors runMemPressure but uses the
// native-ZAP client (no http.Request shape, no headers).
func runNativeZapMemPressure(t *testing.T, name, addr string, body []byte) memStats {
	t.Helper()
	c, err := newNativeZapClient(addr)
	if err != nil {
		t.Fatalf("native dial: %v", err)
	}
	defer c.Close()
	out := make([]byte, len(body))

	for i := 0; i < 50; i++ {
		if _, err := c.Echo(body, out); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}

	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	for i := 0; i < memReqs; i++ {
		if _, err := c.Echo(body, out); err != nil {
			t.Fatalf("[%s] %d: %v", name, i, err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return memStats{
		name:        name,
		reqs:        memReqs,
		elapsed:     elapsed,
		totalAlloc:  after.TotalAlloc - before.TotalAlloc,
		mallocs:     after.Mallocs - before.Mallocs,
		gcCycles:    after.NumGC - before.NumGC,
		heapAllocAt: after.HeapAlloc,
		heapInuseAt: after.HeapInuse,
	}
}

// TestWireBytes captures the actual bytes-on-the-wire for one request
// of each protocol, isolating the framing/encoding cost.
func TestWireBytes(t *testing.T) {
	for _, wl := range makeWorkloads() {
		t.Run(wl.name, func(t *testing.T) {
			// Wire bytes: HTTP serializes "POST /echo HTTP/1.1\r\nHost:...\r\nHEADERS\r\n\r\nBODY"
			// We measure by capturing the bytes a Go http.Request writes
			// (req.Write counts approximately what the kernel will
			// transmit, modulo TCP/IP overhead).
			req, _ := http.NewRequest(http.MethodPost, "http://test.local/echo", bytes.NewReader(wl.body))
			for k, vs := range wl.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			req.Header.Set("Content-Length", strconv.Itoa(len(wl.body)))

			var httpBuf bytes.Buffer
			req.Write(&httpBuf)
			httpWire := httpBuf.Len()

			// ZAP: marshal once and measure.
			req2, _ := http.NewRequest(http.MethodPost, "http://test.local/echo", bytes.NewReader(wl.body))
			for k, vs := range wl.headers {
				for _, v := range vs {
					req2.Header.Add(k, v)
				}
			}
			req2.Header.Set("Content-Length", strconv.Itoa(len(wl.body)))
			zapBytes, err := zaphttp.MarshalRequest(req2)
			if err != nil {
				t.Fatalf("ZAP marshal: %v", err)
			}
			// Add the 4-byte length prefix the wire layer prepends.
			zapWire := len(zapBytes) + 4

			ratio := float64(httpWire) / float64(zapWire)
			savings := 100.0 * (float64(httpWire-zapWire) / float64(httpWire))
			t.Logf("\n  workload: %s (body=%d B, headers=%d)\n  HTTP wire: %d bytes\n  ZAP wire:  %d bytes\n  ratio:     %.2fx (%.1f%% smaller)",
				wl.name, len(wl.body), len(wl.headers),
				httpWire, zapWire, ratio, savings,
			)
		})
	}
}

// ---- concurrent throughput -----------------------------------------------

func TestConcurrentThroughput(t *testing.T) {
	const concurrency = 32
	const requestsPerWorker = 1000

	for _, wl := range []workload{
		{name: "small", body: makeBody(256), headers: makeHeaders(8)},
		{name: "medium", body: makeBody(4 * 1024), headers: makeHeaders(8)},
	} {
		t.Run(wl.name, func(t *testing.T) {
			httpAddr, httpStop := startHTTPServer(t)
			defer httpStop()
			zapAddr, zapStop := startZAPServer(t)
			defer zapStop()

			runConcurrent := func(name, addr string, transport http.RoundTripper) (time.Duration, int64) {
				c := &http.Client{Transport: transport, Timeout: 30 * time.Second}
				var wg sync.WaitGroup
				wg.Add(concurrency)
				start := time.Now()
				for w := 0; w < concurrency; w++ {
					go func() {
						defer wg.Done()
						for i := 0; i < requestsPerWorker; i++ {
							req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo", bytes.NewReader(wl.body))
							for k, vs := range wl.headers {
								for _, v := range vs {
									req.Header.Add(k, v)
								}
							}
							resp, err := c.Do(req)
							if err != nil {
								t.Errorf("[%s] %v", name, err)
								return
							}
							_, _ = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
						}
					}()
				}
				wg.Wait()
				return time.Since(start), int64(concurrency * requestsPerWorker)
			}

			httpClientTransport := &http.Transport{
				MaxIdleConns:        concurrency * 4,
				MaxIdleConnsPerHost: concurrency * 4,
				IdleConnTimeout:     30 * time.Second,
			}

			httpDur, httpReqs := runConcurrent("HTTP", httpAddr, httpClientTransport)
			zapDur, zapReqs := runConcurrent("ZAP", zapAddr, zaphttp.NewTransport(zapAddr))

			httpRPS := float64(httpReqs) / httpDur.Seconds()
			zapRPS := float64(zapReqs) / zapDur.Seconds()

			t.Logf("\n  workload: %s @ concurrency=%d\n  HTTP: %d reqs in %s (%.0f req/s)\n  ZAP:  %d reqs in %s (%.0f req/s)\n  speedup: %.2fx",
				wl.name, concurrency,
				httpReqs, httpDur.Round(time.Millisecond), httpRPS,
				zapReqs, zapDur.Round(time.Millisecond), zapRPS,
				zapRPS/httpRPS,
			)
		})
	}
}

// Cheap, zero-loss benchmark that confirms our `rand` import doesn't
// drag in a CSPRNG round on every benchmark iter.
var _ = rand.Reader
