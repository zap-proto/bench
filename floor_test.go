// The floor: a length-prefixed byte echo over TCP. No protocol.
//
// The server reads a frame and writes the same bytes back without
// looking at them. The client sends a body and reads the echo into a
// caller-owned buffer. There is no message format, no schema, no
// headers, no status, and no dispatch — the payload is opaque to both
// ends.
//
// This arm exists to bound the others from below. Whatever a real
// protocol costs, it costs at least this, and the gap between this arm
// and the native-ZAP arm is the price of having a message format at
// all. It is NOT a measurement of ZAP, and a number taken from here
// must never be reported as one: an earlier revision of this harness
// called this arm "native ZAP", and the paper built on it claimed for
// the protocol what is really just the cost of copying bytes.

package bench_test

import (
	"bufio"
	"fmt"
	"net"
	"sync"
)

type floorServer struct {
	ln   net.Listener
	wg   sync.WaitGroup
	stop chan struct{}
}

func startFloorServer() (*floorServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &floorServer{ln: ln, stop: make(chan struct{})}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *floorServer) Addr() string { return s.ln.Addr().String() }

func (s *floorServer) Close() error {
	close(s.stop)
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *floorServer) accept() {
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

func (s *floorServer) serve(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	tune(conn)
	br := bufio.NewReaderSize(conn, connBuf)
	bw := bufio.NewWriterSize(conn, connBuf)
	var hdr [4]byte
	buf := make([]byte, 0, connBuf)
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
		if err := writeFrame(bw, &hdr, frame); err != nil {
			return
		}
	}
}

// floorClient holds one persistent connection. Sequential calls only.
type floorClient struct {
	conn net.Conn
	bw   *bufio.Writer
	br   *bufio.Reader
	hdr  [4]byte
}

func newFloorClient(addr string) (*floorClient, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	tune(c)
	return &floorClient{
		conn: c,
		bw:   bufio.NewWriterSize(c, connBuf),
		br:   bufio.NewReaderSize(c, connBuf),
	}, nil
}

func (c *floorClient) Close() error { return c.conn.Close() }

// Echo sends body and reads the echoed bytes into out, which the caller
// sizes to len(body). Steady state allocates nothing.
func (c *floorClient) Echo(body, out []byte) (int, error) {
	if err := writeFrame(c.bw, &c.hdr, body); err != nil {
		return 0, err
	}
	got, err := readFrame(c.br, &c.hdr, out)
	if err != nil {
		return 0, err
	}
	if len(got) != len(body) {
		return 0, fmt.Errorf("floor: echoed %d bytes, want %d", len(got), len(body))
	}
	return len(got), nil
}

const connBuf = 64 << 10

func tune(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}
