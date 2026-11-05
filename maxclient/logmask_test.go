package maxclient

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// newAnsweringServer is a stand-in for MAX answering every request with
// payload.
func newAnsweringServer(t *testing.T, payload map[string]interface{}) string {
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
			resp, _ := json.Marshal(map[string]interface{}{"ver": req.Ver, "cmd": 1, "seq": req.Seq, "opcode": req.Opcode, "payload": payload})
			if err := conn.WriteMessage(websocket.TextMessage, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// An error answer of MAX goes to the log with every token masked, and a
// phone lookup logs the number masked (M11).
func TestLogMasksTokensAndPhones(t *testing.T) {
	var logs bytes.Buffer
	c := NewClient("d", zerolog.New(&logs))
	c.URI = newAnsweringServer(t, map[string]interface{}{"error": "not.found", "message": "No", "token": "SECRET-TOKEN"})
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if _, err := c.SearchByPhoneWithTimeout("79001234567", time.Second); err == nil {
		t.Fatalf("the lookup did not fail")
	}
	out := logs.String()
	if !strings.Contains(out, "Server returned error") || strings.Contains(out, "SECRET-TOKEN") {
		t.Fatalf("log %s: want the error logged with its token masked", out)
	}
	if strings.Contains(out, "79001234567") || !strings.Contains(out, MaskPhone("79001234567")) {
		t.Fatalf("log %s: want the phone masked", out)
	}
	if got := MaskPhone("+79001234567"); got != "+790******67" {
		t.Fatalf("MaskPhone = %q", got)
	}
}
