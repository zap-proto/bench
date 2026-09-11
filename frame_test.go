// Length-prefixed framing, shared by the two non-HTTP arms.
//
// A frame is a 4-byte big-endian length followed by that many bytes.
// The native-ZAP arm carries a ZAP message in the payload; the floor
// arm carries the raw body. Both use these helpers so the framing cost
// is identical between them and the only difference measured is what
// the payload is and what each end does with it.

package bench_test

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const frameMax = 64 << 20

// writeFrame writes payload behind a 4-byte big-endian length and flushes.
// hdr is caller-owned scratch so the write path allocates nothing.
func writeFrame(w *bufio.Writer, hdr *[4]byte, payload []byte) error {
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

// readFrame reads one frame into buf, growing buf only when the frame does
// not fit. The returned slice aliases buf and is valid until the next read.
func readFrame(r *bufio.Reader, hdr *[4]byte, buf []byte) ([]byte, error) {
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n <= 0 || n > frameMax {
		return nil, fmt.Errorf("frame: length %d out of range", n)
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
