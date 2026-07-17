// Fair in-process comparison: fasthttp HTTP vs zap-proto/http (ZAP transport),
// same fasthttp handler on both, plus the native-ZAP echo floor.
//
// Every benchmark runs an in-process server + client over loopback. Both
// transports serve the IDENTICAL fasthttp.RequestHandler and exchange the same
// reused fasthttp.Request/Response, so any difference is the wire encoding +
// transport, not the application. testing.B's ReportAllocs snapshots
// process-wide MemStats, so allocs/op counts BOTH client and server allocations
// per request — the number that decides whether the path is GC-bound.
//
// Workloads:
//   - tiny:   2 B body, no extra headers  (the /health shape)
//   - small:  256 B JSON body, 8 headers
//   - chat:   ~2 KiB chat-completion-shaped JSON body, 12 headers
//   - large:  64 KiB body, 8 headers
//
// Run:
//
//	go test -bench=. -benchmem -benchtime=2s | tee bench-results.txt
//	go test -run 'TestFairThroughput|TestMemoryPressure|TestWireBytes' -v

package bench_test

import (
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

// ---- workloads ----

type workload struct {
	name    string
	body    []byte
	headers map[string]string
}

func workloads() []workload {
	return []workload{
		{"tiny", []byte("ok"), nil},
		{"small", makeBody(256), headers(8)},
		{"chat", chatBody(), headers(12)},
		{"large", makeBody(64 * 1024), headers(8)},
	}
}

func makeBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return b
}

// chatBody is a chat-completion-shaped JSON payload (~2 KiB), the realistic
// typed-message case: an application handler unmarshals/marshals it either way,
// so this isolates transport + header cost on a body-heavy request.
func chatBody() []byte {
	return []byte(`{"model":"zen-omni-30b","messages":[` +
		`{"role":"system","content":"You are a helpful assistant that answers concisely and cites sources when possible."},` +
		`{"role":"user","content":"Explain the ZAP wire format versus HTTP/1.1 for a small RPC, and when the binary framing wins on a saturated link."},` +
		`{"role":"assistant","content":"ZAP frames a message as a 16-byte header plus fixed-offset object slots and a packed variable tail; HTTP/1.1 serializes a text request line, CRLF-delimited headers, and the body. For tiny requests HTTP text can be fewer bytes, but ZAP avoids per-field text parsing and its header map is a single sorted JSON blob, so decode is a bounded scan rather than tokenizing an open-ended header block."}` +
		`],"temperature":0.7,"top_p":0.95,"max_tokens":512,"stream":false,"stop":["\n\n"],"presence_penalty":0.0,"frequency_penalty":0.0,"user":"u-z@example.com"}`)
}

func headers(count int) map[string]string {
	h := map[string]string{
		"Content-Type": "application/json",
		"X-Request-Id": "deadbeef-1234-5678-90ab-cdef00000000",
		"X-Org-Id":     "acme",
		"X-User-Id":    "u-z@example.com",
	}
	if count > 4 {
		h["Cache-Control"] = "no-store"
		h["Accept"] = "application/json"
		h["Accept-Encoding"] = "gzip, deflate"
		h["User-Agent"] = "zap-bench/1.0"
	}
	if count > 8 {
		h["X-Trace-Id"] = "trace-0000aaaa1111bbbb2222cccc3333dddd"
		h["X-Span-Id"] = "span-44445555"
		h["Authorization"] = "Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6ImNlcnQtYnVpbHQtaW4ifQ.fake"
		h["X-Forwarded-For"] = "10.0.0.1, 10.0.0.2"
	}
	return h
}

// ---- handler (identical on both transports) ----

// echoHandler returns the request body with its Content-Type and echoes back
// any X-* headers — the shape of a typical RPC backend.
func echoHandler(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.SetContentTypeBytes(ctx.Request.Header.ContentType())
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		if len(k) > 2 && k[0] == 'X' && k[1] == '-' {
			ctx.Response.Header.AddBytesKV(k, v)
		}
	})
	ctx.Write(ctx.PostBody()) //nolint:errcheck
}

// ---- server harnesses ----

func startHTTP(tb testing.TB) (addr string, stop func()) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	srv := &fasthttp.Server{Handler: echoHandler, Name: "-", DisableKeepalive: false}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Shutdown() }
}

func startZAP(tb testing.TB) (addr string, stop func()) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	srv := &zaphttp.Server{Handler: echoHandler}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

// doer abstracts a fasthttp-style Do over either transport.
type doer interface {
	Do(*fasthttp.Request, *fasthttp.Response) error
}

func fillReq(req *fasthttp.Request, addr string, wl workload) {
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI("/echo")
	req.Header.SetHost(addr)
	req.Header.SetContentType("application/json")
	for k, v := range wl.headers {
		req.Header.Set(k, v)
	}
	req.SetBody(wl.body)
}

// ---- benchmarks: ns/op + allocs/op (client + server, process-wide) ----

func benchDo(b *testing.B, addr string, client doer, wl workload) {
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)
	fillReq(req, addr, wl)

	// warm the connection + buffers
	for i := 0; i < 64; i++ {
		if err := client.Do(req, res); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
	b.SetBytes(int64(len(wl.body) * 2))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.Do(req, res); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHTTP(b *testing.B) {
	for _, wl := range workloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := startHTTP(b)
			defer stop()
			benchDo(b, addr, &fasthttp.HostClient{Addr: addr, MaxConns: 1}, wl)
		})
	}
}

func BenchmarkZAP(b *testing.B) {
	for _, wl := range workloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := startZAP(b)
			defer stop()
			tr := zaphttp.NewTransport(addr)
			defer tr.CloseIdleConnections()
			benchDo(b, addr, tr, wl)
		})
	}
}

// BenchmarkNativeZAP is the floor: raw length-prefixed binary echo, no HTTP
// shape, no headers — what ZAP can do for typed in-process RPC.
func BenchmarkNativeZAP(b *testing.B) {
	for _, wl := range workloads() {
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

// ---- memory pressure: allocs/req at default GOGC over a fixed request count ----

const memReqs = 20_000

func memPressure(tb testing.TB, addr string, client doer, wl workload) (allocsPerReq, bytesPerReq float64, gcs uint32, rps float64) {
	tb.Helper()
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)
	fillReq(req, addr, wl)
	for i := 0; i < 200; i++ {
		if err := client.Do(req, res); err != nil {
			tb.Fatalf("warmup: %v", err)
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for i := 0; i < memReqs; i++ {
		if err := client.Do(req, res); err != nil {
			tb.Fatalf("req %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return float64(after.Mallocs-before.Mallocs) / memReqs,
		float64(after.TotalAlloc-before.TotalAlloc) / memReqs,
		after.NumGC - before.NumGC,
		float64(memReqs) / elapsed.Seconds()
}

func TestMemoryPressure(t *testing.T) {
	for _, wl := range workloads() {
		t.Run(wl.name, func(t *testing.T) {
			hAddr, hStop := startHTTP(t)
			defer hStop()
			zAddr, zStop := startZAP(t)
			defer zStop()

			ha, hb, hg, hr := memPressure(t, hAddr, &fasthttp.HostClient{Addr: hAddr, MaxConns: 1}, wl)
			tr := zaphttp.NewTransport(zAddr)
			defer tr.CloseIdleConnections()
			za, zb, zg, zr := memPressure(t, zAddr, tr, wl)

			t.Logf("\n  workload %s (body=%d B, headers=%d) — %d reqs, default GOGC\n"+
				"  HTTP (fasthttp):  %.2f allocs/req  %.0f B/req  %d GCs  %.0f req/s\n"+
				"  ZAP  (zap-http):  %.2f allocs/req  %.0f B/req  %d GCs  %.0f req/s\n"+
				"  ZAP vs HTTP:      allocs %.2fx  throughput %.2fx",
				wl.name, len(wl.body), len(wl.headers), memReqs,
				ha, hb, hg, hr, za, zb, zg, zr,
				safeDiv(za, ha), zr/hr)
		})
	}
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// ---- concurrent throughput (loopback) ----

func TestFairThroughput(t *testing.T) {
	const concurrency = 64
	const perWorker = 4000
	for _, wl := range []workload{{"tiny", []byte("ok"), nil}, {"chat", chatBody(), headers(12)}} {
		t.Run(wl.name, func(t *testing.T) {
			hAddr, hStop := startHTTP(t)
			defer hStop()
			zAddr, zStop := startZAP(t)
			defer zStop()

			hRPS := concurrentRPS(t, concurrency, perWorker, func() doer { return &fasthttp.HostClient{Addr: hAddr, MaxConns: 1} }, hAddr, wl)
			zRPS := concurrentRPS(t, concurrency, perWorker, func() doer { return zaphttp.NewTransport(zAddr) }, zAddr, wl)
			t.Logf("\n  workload %s @ c=%d\n  HTTP: %.0f req/s\n  ZAP:  %.0f req/s\n  ZAP/HTTP: %.2fx",
				wl.name, concurrency, hRPS, zRPS, zRPS/hRPS)
		})
	}
}

func concurrentRPS(t *testing.T, conc, perWorker int, newClient func() doer, addr string, wl workload) float64 {
	var wg sync.WaitGroup
	wg.Add(conc)
	start := time.Now()
	for w := 0; w < conc; w++ {
		go func() {
			defer wg.Done()
			client := newClient()
			req := fasthttp.AcquireRequest()
			res := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(res)
			fillReq(req, addr, wl)
			for i := 0; i < perWorker; i++ {
				if err := client.Do(req, res); err != nil {
					t.Errorf("do: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return float64(conc*perWorker) / time.Since(start).Seconds()
}

// ---- wire size (bytes on the wire per request) ----

func TestWireBytes(t *testing.T) {
	for _, wl := range workloads() {
		t.Run(wl.name, func(t *testing.T) {
			req := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(req)
			fillReq(req, "test.local", wl)

			var httpBuf fasthttp.Request
			req.CopyTo(&httpBuf)
			hb := len(req.Header.Header()) + len(wl.body)

			zapBytes, err := zaphttp.MarshalRequest(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			zb := len(zapBytes) + 4 // + length prefix

			t.Logf("\n  workload %s (body=%d B, headers=%d)\n  HTTP wire ~%d B\n  ZAP  wire  %d B\n  ratio %.2fx",
				wl.name, len(wl.body), len(wl.headers), hb, zb, float64(hb)/float64(zb))
		})
	}
}
