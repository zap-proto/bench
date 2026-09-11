// Native ZAP: a typed message over the same length-prefixed framing.
//
// This is the arm the paper calls "native ZAP", and it is built on the
// published zap-proto/go runtime (v1.8.3) rather than on a hand-rolled
// echo. The request and the response share one object:
//
//	object (fixed section 24 B)
//	  +0   bytes  body      (relative offset u32, length u32)
//	  +8   list   headers   (relative offset u32, count u32)
//	  +16  uint32 status
//
// headers is a variable-entry list holding key and value alternately,
// so the message carries the same header set as the HTTP arms rather
// than dropping it. The server parses the request, reads the body and
// every header, and builds a response echoing the body and the X-
// headers — the same work echoHandler does on the HTTP arms. Reads are
// zero-copy views into the frame; the encoder reuses one builder per
// connection.

package bench_test

import (
	"bufio"
	"net"
	"net/http"
	"sync"

	zap "github.com/zap-proto/go"
)

const (
	fieldBody    = 0
	fieldHeaders = 8
	fieldStatus  = 16
	objectSize   = 24
)

// newBuilder returns a builder sized for one connection's messages.
func newBuilder() *zap.Builder { return zap.NewBuilder(connBuf) }

// encode writes body plus headers into b and returns the finished message,
// which aliases b's buffer until the next Reset.
func encode(b *zap.Builder, status uint32, body []byte, keys, vals [][]byte) []byte {
	b.Reset()
	lb := b.StartList(0)
	for i := range keys {
		lb.AddObjectBytes(keys[i])
		lb.AddObjectBytes(vals[i])
	}
	listOffset, listLen := lb.Finish()

	ob := b.StartObject(objectSize)
	ob.SetBytes(fieldBody, body)
	ob.SetList(fieldHeaders, listOffset, listLen)
	ob.SetUint32(fieldStatus, status)
	ob.FinishAsRoot()
	return b.Finish()
}

// nativeServer answers frames carrying ZAP messages.
type nativeServer struct {
	ln   net.Listener
	wg   sync.WaitGroup
	stop chan struct{}
}

func startNativeServer() (*nativeServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &nativeServer{ln: ln, stop: make(chan struct{})}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *nativeServer) Addr() string { return s.ln.Addr().String() }

func (s *nativeServer) Close() error {
	close(s.stop)
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *nativeServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *nativeServer) serve(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	tune(conn)
	br := bufio.NewReaderSize(conn, connBuf)
	bw := bufio.NewWriterSize(conn, connBuf)
	b := newBuilder()
	var hdr [4]byte
	buf := make([]byte, 0, connBuf)
	keys := make([][]byte, 0, 16)
	vals := make([][]byte, 0, 16)

	for {
		select {
		case <-s.stop:
			return
		default:
		}
		frame, err := readFrame(br, &hdr, buf)
		if err != nil {
			return
		}
		buf = frame[:0]

		msg, err := zap.Parse(frame)
		if err != nil {
			return
		}
		root := msg.Root()
		body := root.Bytes(fieldBody)

		// Echo the X- headers, the same selection echoHandler makes.
		keys, vals = keys[:0], vals[:0]
		list := root.List(fieldHeaders)
		for i := 0; i+1 < list.Len(); i += 2 {
			k := list.BytesAt(i)
			if len(k) > 2 && k[0] == 'X' && k[1] == '-' {
				keys = append(keys, k)
				vals = append(vals, list.BytesAt(i+1))
			}
		}

		// encode reuses b's buffer, which the frame read above does not
		// alias (separate buffers), so building over it is safe.
		if err := writeFrame(bw, &hdr, encode(b, http.StatusOK, body, keys, vals)); err != nil {
			return
		}
	}
}

// nativeClient holds one persistent connection and one builder.
type nativeClient struct {
	conn net.Conn
	bw   *bufio.Writer
	br   *bufio.Reader
	b    *zap.Builder
	hdr  [4]byte
	in   []byte
}

func newNativeClient(addr string) (*nativeClient, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	tune(c)
	return &nativeClient{
		conn: c,
		bw:   bufio.NewWriterSize(c, connBuf),
		br:   bufio.NewReaderSize(c, connBuf),
		b:    newBuilder(),
		in:   make([]byte, 0, connBuf),
	}, nil
}

func (c *nativeClient) Close() error { return c.conn.Close() }

// Call sends body with headers and consumes the whole response — body
// length and every header — so the arm does the work the HTTP client
// arms do when they drain a response.
func (c *nativeClient) Call(body []byte, keys, vals [][]byte) (int, error) {
	if err := writeFrame(c.bw, &c.hdr, encode(c.b, 0, body, keys, vals)); err != nil {
		return 0, err
	}
	frame, err := readFrame(c.br, &c.hdr, c.in)
	if err != nil {
		return 0, err
	}
	c.in = frame[:0]

	msg, err := zap.Parse(frame)
	if err != nil {
		return 0, err
	}
	root := msg.Root()
	n := len(root.Bytes(fieldBody))
	list := root.List(fieldHeaders)
	for i := 0; i < list.Len(); i++ {
		_ = list.BytesAt(i)
	}
	_ = root.Uint32(fieldStatus)
	return n, nil
}

// flatten turns an http.Header into the parallel key/value slices the
// native arm sends, so every arm ships the identical header set.
//
// Callers flatten once, outside the request loop: a typed protocol has
// no string-keyed map to walk per call, so paying for one here would be
// charging the native arm for a data structure it does not have. The
// HTTP arms do walk their map on every request, and that difference is
// part of what separates a typed wire from a text one.
func flatten(h http.Header) (keys, vals [][]byte) {
	for k, vs := range h {
		for _, v := range vs {
			keys = append(keys, []byte(k))
			vals = append(vals, []byte(v))
		}
	}
	return keys, vals
}
