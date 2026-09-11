// Memory, wire and throughput comparison across five arms on loopback.
//
//	net/http     Go stdlib client + server. The everywhere baseline.
//	fasthttp     fasthttp client + server over HTTP/1.1. Same object
//	             model as the ZAP-HTTP arm, so the difference between
//	             the two is the wire and nothing else.
//	ZAP-HTTP     zap-proto/http: HTTP semantics on the ZAP wire.
//	native ZAP   a typed zap-proto/go message over length-prefixed
//	             frames (native_test.go).
//	floor        a length-prefixed byte echo with no protocol at all
//	             (floor_test.go). A lower bound, not a protocol.
//
// Reading a transport number against net/http alone confounds two
// changes at once: fasthttp's object model is far cheaper than
// net/http's whatever wire it runs on. The fasthttp arm exists to
// separate them. Report ZAP against fasthttp to talk about the wire,
// and against net/http to talk about what a stdlib service would see
// if it moved.
//
// Every arm sends the same body and the same headers over a warm
// connection and consumes the whole response. Each client is used the
// way its library is meant to be used: net/http builds a fresh Request
// per call because it offers nothing else; the fasthttp-family arms
// acquire and release from fasthttp's pools. That difference is part
// of what is being measured and is not a defect in the comparison.
//
// Run:
//
//	go test -run='TestMemoryPressure|TestWireBytes|TestConcurrentThroughput' -v
//	go test -bench=. -benchmem -benchtime=2s -count=3

package bench_test

import (
	"bytes"
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

	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

// ---- workloads ------------------------------------------------------------

type workload struct {
	name    string
	body    []byte
	headers http.Header
}

func makeWorkloads() []workload {
	return []workload{
		{name: "tiny", body: makeBody(16), headers: makeHeaders(4)},
		{name: "small", body: makeBody(256), headers: makeHeaders(8)},
		{name: "chat", body: chatBody(), headers: makeHeaders(12)},
		{name: "medium", body: makeBody(4 * 1024), headers: makeHeaders(12)},
		{name: "large", body: makeBody(64 * 1024), headers: makeHeaders(8)},
	}
}

// chatBody is a chat-completion-shaped JSON payload of about 1 KiB: the
// realistic case, where an application handler unmarshals and marshals the
// body on either transport, so the arm difference is transport and headers
// rather than anything the synthetic bodies exercise.
func chatBody() []byte {
	return []byte(`{"model":"zen-omni-30b","messages":[` +
		`{"role":"system","content":"You are a helpful assistant that answers concisely and cites sources when possible."},` +
		`{"role":"user","content":"Explain the ZAP wire format versus HTTP/1.1 for a small RPC, and when the binary framing wins on a saturated link."},` +
		`{"role":"assistant","content":"ZAP frames a message as a 16-byte header plus fixed-offset object slots and a packed variable tail; HTTP/1.1 serializes a text request line, CRLF-delimited headers, and the body. For tiny requests HTTP text can be fewer bytes, but ZAP avoids per-field text parsing, so decode is a bounded scan rather than tokenizing an open-ended header block."}` +
		`],"temperature":0.7,"top_p":0.95,"max_tokens":512,"stream":false,"stop":["\n\n"],` +
		`"presence_penalty":0.0,"frequency_penalty":0.0,"user":"u-alice@acme.dev"}`)
}

func makeBody(n int) []byte {
	// Deterministic, non-repeating enough that no arm gets a compression
	// advantage on patterns.
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
	h.Set("X-Org-Id", "acme-corp")
	h.Set("X-User-Id", "u-alice@acme.dev")
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

// ---- handlers -------------------------------------------------------------

// echoStd returns the body verbatim with its Content-Type and the request's
// X- headers. The shape of a backend that reads a payload, does something,
// and answers with one of similar size.
func echoStd(w http.ResponseWriter, r *http.Request) {
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

// echoFast is echoStd against fasthttp's objects. Used by both the fasthttp
// arm and the ZAP-HTTP arm, so those two differ only in transport.
func echoFast(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.SetContentTypeBytes(ctx.Request.Header.ContentType())
	for k, v := range ctx.Request.Header.All() {
		if len(k) > 2 && (k[0] == 'X' || k[0] == 'x') && k[1] == '-' {
			ctx.Response.Header.SetBytesKV(k, v)
		}
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody(ctx.PostBody())
}

// ---- servers --------------------------------------------------------------

func startStdServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(echoStd))
	return srv.Listener.Addr().String(), srv.Close
}

func startFastServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fasthttp.Server{Handler: echoFast}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Shutdown() }
}

func startZAPServer(t testing.TB) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &zaphttp.Server{Handler: echoFast}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

// doer is what the fasthttp-family clients have in common: fasthttp's own
// HostClient and zaphttp's Transport differ in the wire, not the call.
type doer interface {
	Do(*fasthttp.Request, *fasthttp.Response) error
}

func fastRequest(req *fasthttp.Request, addr string, body []byte, headers http.Header) {
	req.SetRequestURI("http://" + addr + "/echo")
	req.Header.SetMethod(fasthttp.MethodPost)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.SetBody(body)
}

func stdRequest(addr string, body []byte, headers http.Header) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/echo", bytes.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req
}

// ---- benchmarks -----------------------------------------------------------

func BenchmarkStd(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := startStdServer(b)
			defer stop()
			c := &http.Client{
				Timeout: 10 * time.Second,
				Transport: &http.Transport{
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 100,
					IdleConnTimeout:     30 * time.Second,
				},
			}
			b.SetBytes(int64(len(wl.body) * 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp, err := c.Do(stdRequest(addr, wl.body, wl.headers))
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
}

func benchFast(b *testing.B, start func(testing.TB) (string, func()), client func(string) doer) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			addr, stop := start(b)
			defer stop()
			c := client(addr)
			b.SetBytes(int64(len(wl.body) * 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
				fastRequest(req, addr, wl.body, wl.headers)
				if err := c.Do(req, resp); err != nil {
					b.Fatal(err)
				}
				_ = resp.Body()
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			}
		})
	}
}

func BenchmarkFast(b *testing.B) {
	benchFast(b, startFastServer, func(addr string) doer {
		return &fasthttp.HostClient{Addr: addr, MaxConns: 100}
	})
}

func BenchmarkZAP(b *testing.B) {
	benchFast(b, startZAPServer, func(addr string) doer { return zaphttp.Dial("tcp", addr) })
}

func BenchmarkNative(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			srv, err := startNativeServer()
			if err != nil {
				b.Fatal(err)
			}
			defer srv.Close()
			c, err := newNativeClient(srv.Addr())
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close()
			keys, vals := flatten(wl.headers)
			b.SetBytes(int64(len(wl.body) * 2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.Call(wl.body, keys, vals); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFloor(b *testing.B) {
	for _, wl := range makeWorkloads() {
		b.Run(wl.name, func(b *testing.B) {
			srv, err := startFloorServer()
			if err != nil {
				b.Fatal(err)
			}
			defer srv.Close()
			c, err := newFloorClient(srv.Addr())
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

// ---- memory pressure ------------------------------------------------------
//
// Not benchmarks in the testing.B sense: a fixed request count with
// runtime.MemStats snapshotted around it, so the numbers are process-wide
// heap pressure rather than per-op cost attributed by the framework.

const memReqs = 5_000

const warmup = 50

type stats struct {
	name       string
	reqs       int
	elapsed    time.Duration
	totalAlloc uint64
	mallocs    uint64
	gc         uint32
	heapInuse  uint64
}

func (s stats) bytesPerReq() float64  { return float64(s.totalAlloc) / float64(s.reqs) }
func (s stats) allocsPerReq() float64 { return float64(s.mallocs) / float64(s.reqs) }
func (s stats) rps() float64          { return float64(s.reqs) / s.elapsed.Seconds() }

func (s stats) String() string {
	return fmt.Sprintf("%-12s %5d reqs in %8s (%7.0f req/s) | %10.1f bytes/req | %6.1f allocs/req | %4d GCs | heap=%d KiB",
		s.name, s.reqs, s.elapsed.Round(time.Millisecond), s.rps(),
		s.bytesPerReq(), s.allocsPerReq(), s.gc, s.heapInuse/1024)
}

// measure runs call warmup+memReqs times and reports the allocation delta
// over the measured window.
func measure(t *testing.T, name string, call func() error) stats {
	t.Helper()
	for i := 0; i < warmup; i++ {
		if err := call(); err != nil {
			t.Fatalf("[%s] warmup: %v", name, err)
		}
	}
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	for i := 0; i < memReqs; i++ {
		if err := call(); err != nil {
			t.Fatalf("[%s] request %d: %v", name, i, err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	return stats{
		name:       name,
		reqs:       memReqs,
		elapsed:    elapsed,
		totalAlloc: after.TotalAlloc - before.TotalAlloc,
		mallocs:    after.Mallocs - before.Mallocs,
		gc:         after.NumGC - before.NumGC,
		heapInuse:  after.HeapInuse,
	}
}

func stdCall(c *http.Client, addr string, body []byte, headers http.Header) func() error {
	return func() error {
		resp, err := c.Do(stdRequest(addr, body, headers))
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return err
	}
}

func fastCall(c doer, addr string, body []byte, headers http.Header) func() error {
	return func() error {
		req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		fastRequest(req, addr, body, headers)
		if err := c.Do(req, resp); err != nil {
			return err
		}
		_ = resp.Body()
		return nil
	}
}

func TestMemoryPressure(t *testing.T) {
	for _, wl := range makeWorkloads() {
		t.Run(wl.name, func(t *testing.T) {
			stdAddr, stdStop := startStdServer(t)
			defer stdStop()
			fastAddr, fastStop := startFastServer(t)
			defer fastStop()
			zapAddr, zapStop := startZAPServer(t)
			defer zapStop()
			natSrv, err := startNativeServer()
			if err != nil {
				t.Fatal(err)
			}
			defer natSrv.Close()
			floorSrv, err := startFloorServer()
			if err != nil {
				t.Fatal(err)
			}
			defer floorSrv.Close()

			stdClient := &http.Client{
				Timeout: 30 * time.Second,
				Transport: &http.Transport{
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 100,
					IdleConnTimeout:     30 * time.Second,
				},
			}
			natClient, err := newNativeClient(natSrv.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer natClient.Close()
			floorClient, err := newFloorClient(floorSrv.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer floorClient.Close()

			keys, vals := flatten(wl.headers)
			out := make([]byte, len(wl.body))

			s := []stats{
				measure(t, "net/http", stdCall(stdClient, stdAddr, wl.body, wl.headers)),
				measure(t, "fasthttp", fastCall(&fasthttp.HostClient{Addr: fastAddr, MaxConns: 100}, fastAddr, wl.body, wl.headers)),
				measure(t, "ZAP-HTTP", fastCall(zaphttp.Dial("tcp", zapAddr), zapAddr, wl.body, wl.headers)),
				measure(t, "native ZAP", func() error { _, err := natClient.Call(wl.body, keys, vals); return err }),
				measure(t, "floor", func() error { _, err := floorClient.Echo(wl.body, out); return err }),
			}

			var b bytes.Buffer
			fmt.Fprintf(&b, "\n  workload: %s (body=%d B, headers=%d)\n", wl.name, len(wl.body), len(wl.headers))
			for _, x := range s {
				fmt.Fprintf(&b, "  %s\n", x)
			}
			std, fast, zap, nat := s[0], s[1], s[2], s[3]
			fmt.Fprintf(&b, "  ZAP-HTTP vs fasthttp (wire only)  bytes:%.2fx allocs:%.2fx throughput:%.2fx\n",
				fast.bytesPerReq()/zap.bytesPerReq(), fast.allocsPerReq()/zap.allocsPerReq(), zap.rps()/fast.rps())
			fmt.Fprintf(&b, "  ZAP-HTTP vs net/http              bytes:%.2fx allocs:%.2fx throughput:%.2fx\n",
				std.bytesPerReq()/zap.bytesPerReq(), std.allocsPerReq()/zap.allocsPerReq(), zap.rps()/std.rps())
			fmt.Fprintf(&b, "  native ZAP vs fasthttp            bytes:%.2fx allocs:%.2fx throughput:%.2fx\n",
				fast.bytesPerReq()/nat.bytesPerReq(), fast.allocsPerReq()/nat.allocsPerReq(), nat.rps()/fast.rps())
			fmt.Fprintf(&b, "  native ZAP vs net/http            bytes:%.2fx allocs:%.2fx throughput:%.2fx",
				std.bytesPerReq()/nat.bytesPerReq(), std.allocsPerReq()/nat.allocsPerReq(), nat.rps()/std.rps())
			t.Log(b.String())
		})
	}
}

// ---- wire bytes -----------------------------------------------------------

// TestWireBytes captures one serialized request per arm, isolating framing
// and encoding from everything else.
func TestWireBytes(t *testing.T) {
	b := newBuilder()
	for _, wl := range makeWorkloads() {
		t.Run(wl.name, func(t *testing.T) {
			req := stdRequest("test.local", wl.body, wl.headers)
			req.Header.Set("Content-Length", strconv.Itoa(len(wl.body)))
			var buf bytes.Buffer
			if err := req.Write(&buf); err != nil {
				t.Fatalf("http marshal: %v", err)
			}
			httpWire := buf.Len()

			fastReq := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(fastReq)
			fastRequest(fastReq, "test.local", wl.body, wl.headers)
			frame, err := zaphttp.MarshalRequest(fastReq)
			if err != nil {
				t.Fatalf("ZAP-HTTP marshal: %v", err)
			}
			zapWire := len(frame) + 4 // the length prefix the wire layer prepends

			keys, vals := flatten(wl.headers)
			natWire := len(encode(b, 0, wl.body, keys, vals)) + 4

			t.Logf("\n  workload: %s (body=%d B, headers=%d)\n  HTTP/1.1 wire:   %6d bytes\n  ZAP-HTTP wire:   %6d bytes (%.2fx HTTP, %s)\n  native ZAP wire: %6d bytes (%.2fx HTTP, %s)",
				wl.name, len(wl.body), len(wl.headers),
				httpWire,
				zapWire, float64(zapWire)/float64(httpWire), compare(zapWire, httpWire),
				natWire, float64(natWire)/float64(httpWire), compare(natWire, httpWire),
			)
		})
	}
}

// compare names the direction so a ratio cannot be read backwards. The
// predecessor of this harness printed every ZAP frame as "smaller" while
// computing a ratio that said the opposite.
func compare(got, want int) string {
	switch {
	case got > want:
		return fmt.Sprintf("%.1f%% larger", 100*float64(got-want)/float64(want))
	case got < want:
		return fmt.Sprintf("%.1f%% smaller", 100*float64(want-got)/float64(want))
	}
	return "equal"
}

// ---- concurrent throughput ------------------------------------------------

func TestConcurrentThroughput(t *testing.T) {
	const workers = 32
	const perWorker = 1000

	for _, wl := range []workload{
		{name: "small", body: makeBody(256), headers: makeHeaders(8)},
		{name: "medium", body: makeBody(4 * 1024), headers: makeHeaders(8)},
	} {
		t.Run(wl.name, func(t *testing.T) {
			stdAddr, stdStop := startStdServer(t)
			defer stdStop()
			fastAddr, fastStop := startFastServer(t)
			defer fastStop()
			zapAddr, zapStop := startZAPServer(t)
			defer zapStop()
			natSrv, err := startNativeServer()
			if err != nil {
				t.Fatal(err)
			}
			defer natSrv.Close()

			run := func(name string, worker func(int) error) (string, float64) {
				var wg sync.WaitGroup
				wg.Add(workers)
				errs := make([]error, workers)
				start := time.Now()
				for w := 0; w < workers; w++ {
					go func(w int) {
						defer wg.Done()
						errs[w] = worker(w)
					}(w)
				}
				wg.Wait()
				elapsed := time.Since(start)
				for _, err := range errs {
					if err != nil {
						t.Fatalf("[%s] %v", name, err)
					}
				}
				rps := float64(workers*perWorker) / elapsed.Seconds()
				return fmt.Sprintf("  %-12s %6d reqs in %8s (%7.0f req/s)", name, workers*perWorker, elapsed.Round(time.Millisecond), rps), rps
			}

			stdClient := &http.Client{
				Timeout:   30 * time.Second,
				Transport: &http.Transport{MaxIdleConns: workers * 2, MaxIdleConnsPerHost: workers * 2},
			}
			fastClient := &fasthttp.HostClient{Addr: fastAddr, MaxConns: workers * 2}
			zapClient := zaphttp.Dial("tcp", zapAddr)

			stdLine, stdRPS := run("net/http", func(int) error {
				call := stdCall(stdClient, stdAddr, wl.body, wl.headers)
				for i := 0; i < perWorker; i++ {
					if err := call(); err != nil {
						return err
					}
				}
				return nil
			})
			fastLine, fastRPS := run("fasthttp", func(int) error {
				call := fastCall(fastClient, fastAddr, wl.body, wl.headers)
				for i := 0; i < perWorker; i++ {
					if err := call(); err != nil {
						return err
					}
				}
				return nil
			})
			zapLine, zapRPS := run("ZAP-HTTP", func(int) error {
				call := fastCall(zapClient, zapAddr, wl.body, wl.headers)
				for i := 0; i < perWorker; i++ {
					if err := call(); err != nil {
						return err
					}
				}
				return nil
			})
			// One native connection per worker: the native client is
			// sequential by construction, which is how it would be
			// deployed.
			keys, vals := flatten(wl.headers)
			natLine, natRPS := run("native ZAP", func(int) error {
				c, err := newNativeClient(natSrv.Addr())
				if err != nil {
					return err
				}
				defer c.Close()
				for i := 0; i < perWorker; i++ {
					if _, err := c.Call(wl.body, keys, vals); err != nil {
						return err
					}
				}
				return nil
			})

			t.Logf("\n  workload: %s @ %d workers\n%s\n%s\n%s\n%s\n  ZAP-HTTP vs fasthttp: %.2fx   ZAP-HTTP vs net/http: %.2fx\n  native ZAP vs fasthttp: %.2fx   native ZAP vs net/http: %.2fx",
				wl.name, workers, stdLine, fastLine, zapLine, natLine,
				zapRPS/fastRPS, zapRPS/stdRPS, natRPS/fastRPS, natRPS/stdRPS)
		})
	}
}
