// Native ZAP RPC — no HTTP shape. Direct binary Echo(body) -> Data
// over TCP, length-prefixed frames, zero http.Request overhead.
//
// Wire frame:
//   [4 bytes BE length] [body bytes]
//
// Both directions use the same shape. The protocol semantics are
// "echo the body back" — that's the entire RPC. No methods, no
// routing, no headers.
//
// This is the floor of what ZAP can do for in-process RPC and is
// what the "native ZAP" benchmark in bench_test.go measures.

package bench_test

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// nativeZapServer reads one length-prefixed frame, writes the same
// frame back. Persistent connection — clients reuse it across many
// RPCs without TCP re-establishment.
type nativeZapServer struct {
	ln   net.Listener
	wg   sync.WaitGroup
	stop chan struct{}
}

func startNativeZapServer() (*nativeZapServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &nativeZapServer{ln: ln, stop: make(chan struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *nativeZapServer) Addr() string { return s.ln.Addr().String() }

func (s *nativeZapServer) Close() error {
	close(s.stop)
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *nativeZapServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *nativeZapServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	bw := bufio.NewWriterSize(conn, 64*1024)
	var hdr [4]byte
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > 64*1024*1024 {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		// Echo: reuse the buffer. Same length prefix + bytes.
		if _, err := bw.Write(hdr[:]); err != nil {
			return
		}
		if _, err := bw.Write(buf); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}

// nativeZapClient holds a single persistent TCP conn. Sequential
// RPCs only (matches the sequential workload of bench_test.go).
// Multi-conn pooling is a separate concern; the goal here is to
// measure the raw native-wire overhead.
type nativeZapClient struct {
	conn net.Conn
	bw   *bufio.Writer
	br   *bufio.Reader
	hdr  [4]byte
}

func newNativeZapClient(addr string) (*nativeZapClient, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
	}
	return &nativeZapClient{
		conn: c,
		bw:   bufio.NewWriterSize(c, 64*1024),
		br:   bufio.NewReaderSize(c, 64*1024),
	}, nil
}

func (c *nativeZapClient) Close() error {
	return c.conn.Close()
}

// Echo sends body, reads back the echoed bytes into out (caller-
// owned, sized to len(body)). Returns the n bytes echoed.
func (c *nativeZapClient) Echo(body []byte, out []byte) (int, error) {
	binary.BigEndian.PutUint32(c.hdr[:], uint32(len(body)))
	if _, err := c.bw.Write(c.hdr[:]); err != nil {
		return 0, err
	}
	if _, err := c.bw.Write(body); err != nil {
		return 0, err
	}
	if err := c.bw.Flush(); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c.br, c.hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint32(c.hdr[:]))
	if n != len(body) {
		return 0, fmt.Errorf("native-zap: server echoed %d bytes, want %d", n, len(body))
	}
	if cap(out) < n {
		return 0, fmt.Errorf("native-zap: output buffer too small")
	}
	if _, err := io.ReadFull(c.br, out[:n]); err != nil {
		return 0, err
	}
	return n, nil
}
