package maxclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

func newTestClient() *Client {
	return NewClient("test-device", zerolog.Nop())
}

// newEchoServer starts a stand-in for MAX that answers every request at once
// with an empty payload, and returns its WebSocket URI.
func newEchoServer(t *testing.T) string {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req Response
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			resp, _ := json.Marshal(map[string]interface{}{
				"ver": req.Ver, "cmd": 1, "seq": req.Seq, "opcode": req.Opcode, "payload": map[string]interface{}{},
			})
			if err := conn.WriteMessage(websocket.TextMessage, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// HTTP handlers and the ping goroutine write to one connection at the same
// time. gorilla/websocket panics on concurrent writes, which took the whole
// gateway down with every account's session.
func TestConcurrentRequestsDoNotWriteConcurrently(t *testing.T) {
	c := newTestClient()
	c.URI = newEchoServer(t)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// Frames larger than the write buffer go out in pieces, which widens the
	// window two unsynchronized writers would overlap in.
	pad := strings.Repeat("x", 64<<10)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := c.sendAndWaitWithTimeout(OpPing, map[string]interface{}{"pad": pad}, 5*time.Second); err != nil {
					t.Errorf("request: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The payload of a pushed frame goes to the log at Info, without the tokens
// it may carry and cut to maxLoggedPayload bytes.
func TestPayloadForLogMasksTokensAndTruncates(t *testing.T) {
	got := payloadForLog(map[string]interface{}{
		"token":  "secret-1",
		"reason": "session.closed",
		"profile": map[string]interface{}{
			"tokenAttrs": map[string]interface{}{"LOGIN": map[string]interface{}{"token": "secret-2"}},
		},
		"sessions": []interface{}{map[string]interface{}{"authToken": "secret-3", "id": 7}},
	})
	if strings.Contains(got, "secret") {
		t.Fatalf("logged payload carries a token: %s", got)
	}
	if !strings.Contains(got, "session.closed") || !strings.Contains(got, `"id":7`) {
		t.Fatalf("logged payload lost its other fields: %s", got)
	}

	long := payloadForLog(map[string]interface{}{"text": strings.Repeat("я", 4*maxLoggedPayload)})
	if len(long) > maxLoggedPayload+len("...(truncated)") || !strings.HasSuffix(long, "...(truncated)") {
		t.Fatalf("long payload logged as %d bytes, want at most %d and marked truncated", len(long), maxLoggedPayload)
	}
}

func TestNextSeqStartsAtOne(t *testing.T) {
	c := newTestClient()

	if got := c.nextSeq(); got != 1 {
		t.Fatalf("first seq = %d, want 1", got)
	}
	if got := c.nextSeq(); got != 2 {
		t.Fatalf("second seq = %d, want 2", got)
	}
}

// MAX parses seq as a signed 16-bit integer and silently drops the connection
// for anything above 32767, so the counter must never emit such a value.
func TestNextSeqWrapsAtMaxSeq(t *testing.T) {
	c := newTestClient()
	atomic.StoreInt32(&c.seq, MaxSeq-1)

	if got := c.nextSeq(); got != MaxSeq {
		t.Fatalf("seq before wrap = %d, want %d", got, MaxSeq)
	}
	if got := c.nextSeq(); got != 1 {
		t.Fatalf("seq after wrap = %d, want 1", got)
	}
}

func TestNextSeqStaysInRangeUnderConcurrency(t *testing.T) {
	c := newTestClient()
	atomic.StoreInt32(&c.seq, MaxSeq-50)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := c.nextSeq(); got < 1 || got > MaxSeq {
					t.Errorf("seq out of range: %d", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
