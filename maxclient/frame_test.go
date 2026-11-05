package maxclient

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// frameHead is the head of one WebSocket frame: FIN, opcode, payload length.
type frameHead struct {
	fin    bool
	opcode byte
	length uint64
}

// newFrameServer starts a stand-in for MAX that, after the handshake, reports
// the head of every frame the client writes. It answers nothing.
func newFrameServer(t *testing.T) (string, <-chan frameHead) {
	t.Helper()
	heads := make(chan frameHead, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = rw.Flush()
		readFrames(rw.Reader, heads)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), heads
}

func readFrames(r *bufio.Reader, heads chan<- frameHead) {
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}
		n := uint64(hdr[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(ext[:])
		}
		skip := int64(n)
		if hdr[1]&0x80 != 0 {
			skip += 4 // the mask key
		}
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return
		}
		heads <- frameHead{fin: hdr[0]&0x80 != 0, opcode: hdr[0] & 0x0f, length: n}
	}
}

// MAX closes the connection on a request split into continuation frames:
// live on 2026-11-05 every request over gorilla's default 4 KiB write buffer
// dropped the session. A long request goes out as one whole frame.
func TestLongRequestGoesAsOneFrame(t *testing.T) {
	uri, heads := newFrameServer(t)
	c := newTestClient()
	c.URI = uri
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	text := strings.Repeat("ж", 4000) // 8000 bytes
	go func() {
		_, _ = c.sendAndWaitWithTimeout(OpMsgSend, map[string]interface{}{"chatId": -1, "message": map[string]interface{}{"text": text}}, time.Second)
	}()
	select {
	case h := <-heads:
		if !h.fin || h.opcode != 1 || h.length < 8000 {
			t.Fatalf("first frame: %+v, want a whole text frame of the request", h)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server got no frame")
	}
}

// A request longer than one frame is refused, not written.
func TestTooLongRequestIsNotWritten(t *testing.T) {
	uri, heads := newFrameServer(t)
	c := newTestClient()
	c.URI = uri
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	text := strings.Repeat("a", wsWriteBufferSize)
	_, err := c.sendAndWaitWithTimeout(OpMsgSend, map[string]interface{}{"text": text}, time.Second)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	select {
	case h := <-heads:
		t.Fatalf("a frame was written: %+v", h)
	case <-time.After(200 * time.Millisecond):
	}
}
