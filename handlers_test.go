package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"maxapi/maxclient"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// main.go's init() calls flag.Parse(), which rejects the -test.* flags unless
// they are registered first. Package-level variables are initialised before
// any init() runs, so registering them here keeps `go test` working.
var _ = func() bool { testing.Init(); return true }()

const (
	testSelfID    int64 = 1000 // MaxUserID of the sending account
	testContactID int64 = 2002 // MAX id behind testPhone
	testPhone           = "79001234567"
)

// fakeFrame is one request the fake MAX server received.
type fakeFrame struct {
	Opcode  int
	Payload map[string]interface{}
}

// fakeMAX is an in-process stand-in for the MAX WebSocket server. reply
// returns the payload to answer a request with; nil leaves the request
// unanswered, like a server that never acknowledges it.
type fakeMAX struct {
	srv   *httptest.Server
	reply func(f fakeFrame) map[string]interface{}

	mu     sync.Mutex
	frames []fakeFrame
	seen   chan int // opcode of every received frame
}

func newFakeMAX(t *testing.T, reply func(f fakeFrame) map[string]interface{}) *fakeMAX {
	t.Helper()
	f := &fakeMAX{reply: reply, seen: make(chan int, 16)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var req maxclient.Response
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			frame := fakeFrame{Opcode: req.Opcode, Payload: req.Payload}
			f.mu.Lock()
			f.frames = append(f.frames, frame)
			f.mu.Unlock()
			select {
			case f.seen <- req.Opcode:
			default:
			}
			payload := f.reply(frame)
			if payload == nil {
				continue
			}
			resp, _ := json.Marshal(map[string]interface{}{
				"ver": req.Ver, "cmd": 1, "seq": req.Seq, "opcode": req.Opcode, "payload": payload,
			})
			if err := conn.WriteMessage(websocket.TextMessage, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// count returns how many frames with the opcode the server received.
func (f *fakeMAX) count(op maxclient.Opcode) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fr := range f.frames {
		if fr.Opcode == int(op) {
			n++
		}
	}
	return n
}

// lastPayload returns the payload of the last frame with the opcode.
func (f *fakeMAX) lastPayload(op maxclient.Opcode) map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.frames) - 1; i >= 0; i-- {
		if f.frames[i].Opcode == int(op) {
			return f.frames[i].Payload
		}
	}
	return nil
}

// connectedUser connects a real maxclient.Client to the fake server and
// registers it as the session of a fresh user, whose id it returns.
func connectedUser(t *testing.T, f *fakeMAX) (string, *maxclient.Client) {
	t.Helper()
	c := maxclient.NewClient("test-device", zerolog.Nop())
	c.URI = "ws" + strings.TrimPrefix(f.srv.URL, "http")
	c.MaxUserID = testSelfID
	if err := c.Connect(); err != nil {
		t.Fatalf("connect to fake MAX: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return registerClient(t, c), c
}

func registerClient(t *testing.T, c *maxclient.Client) string {
	t.Helper()
	userID := t.Name()
	clientManager.SetMaxClient(userID, c)
	t.Cleanup(func() { clientManager.DeleteMaxClient(userID) })
	return userID
}

// call runs a handler as the given user and decodes the JSON body.
func call(t *testing.T, h http.HandlerFunc, userID, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), "userinfo", Values{map[string]string{"Id": userID}})
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

// wantError checks the error envelope {success:false, error, code}.
func wantError(t *testing.T, status int, body map[string]interface{}, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus || body["code"] != wantCode {
		t.Fatalf("got %d %v, want %d with code %q", status, body, wantStatus, wantCode)
	}
	if body["success"] != false {
		t.Fatalf("success = %v, want false", body["success"])
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("error message is empty: %v", body)
	}
}

func contactReply(f fakeFrame) map[string]interface{} {
	return map[string]interface{}{"contact": map[string]interface{}{
		"id": testContactID, "names": []interface{}{map[string]interface{}{"name": "Ivan"}},
	}}
}

func sentReply(f fakeFrame) map[string]interface{} {
	chatID := f.Payload["chatId"]
	return map[string]interface{}{"chatId": chatID, "message": map[string]interface{}{
		"id": "msg-1", "text": "hello", "time": 1700000000000, "type": "TEXT",
	}}
}

func TestSendTextNotConnected(t *testing.T) {
	cases := []struct {
		name   string
		client *maxclient.Client
	}{
		{"no session", nil},
		{"session down", maxclient.NewClient("test-device", zerolog.Nop())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID := t.Name()
			if tc.client != nil {
				userID = registerClient(t, tc.client)
			}
			status, body := call(t, (&server{}).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)
			wantError(t, status, body, http.StatusServiceUnavailable, "not_connected")
		})
	}
}

// Without a target maxapi used to send to chatId 0, the account's own
// "Favorites" chat.
func TestSendTextWithoutTargetIsInvalidTarget(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)

	status, body := call(t, (&server{}).SendMessage(), userID, `{"text": "hello"}`)

	wantError(t, status, body, http.StatusBadRequest, "invalid_target")
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d send frames, want 0", n)
	}
}

func TestSendTextUnknownPhoneIsAddressNotFound(t *testing.T) {
	cases := []struct {
		name, phone string
		lookups     int
	}{
		{"MAX has no such user", testPhone, 1}, // ErrUserNotFound
		{"malformed number", "12-34", 0},       // ErrInvalidPhone, rejected before asking MAX
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return map[string]interface{}{} })
			userID, _ := connectedUser(t, f)

			status, body := call(t, (&server{}).SendMessage(), userID, `{"phone": "`+tc.phone+`", "text": "hello"}`)

			wantError(t, status, body, http.StatusNotFound, "address_not_found")
			if n := f.count(maxclient.OpContactInfoByPhone); n != tc.lookups {
				t.Fatalf("MAX received %d lookups, want %d", n, tc.lookups)
			}
			if n := f.count(maxclient.OpMsgSend); n != 0 {
				t.Fatalf("MAX received %d send frames, want 0", n)
			}
		})
	}
}

func TestSendTextLookupTimeoutIsLookupFailed(t *testing.T) {
	t.Parallel()
	const limit = 4 * time.Second
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return nil })
	userID, _ := connectedUser(t, f)

	start := time.Now()
	status, body := call(t, (&server{}).SendMessage(), userID, `{"phone": "`+testPhone+`", "text": "hello"}`)
	elapsed := time.Since(start)

	wantError(t, status, body, http.StatusServiceUnavailable, "lookup_failed")
	if elapsed > limit+500*time.Millisecond {
		t.Fatalf("answered after %v, want within the %v lookup limit", elapsed, limit)
	}
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d send frames, want 0", n)
	}
}

func TestSendTextRejectedByMAXIsSendRejected(t *testing.T) {
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{
			"error": "too.many.requests", "message": "Too many requests", "title": "Slow down",
		}
	})
	userID, _ := connectedUser(t, f)

	status, body := call(t, (&server{}).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)

	wantError(t, status, body, http.StatusUnprocessableEntity, "send_rejected")
	if body["upstreamCode"] != "too.many.requests" {
		t.Fatalf("upstreamCode = %v, want MAX's code too.many.requests", body["upstreamCode"])
	}
}

func TestSendTextReplyTimeoutIsSendFailed(t *testing.T) {
	t.Parallel()
	const limit = 5 * time.Second
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return nil })
	userID, _ := connectedUser(t, f)

	start := time.Now()
	status, body := call(t, (&server{}).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)
	elapsed := time.Since(start)

	wantError(t, status, body, http.StatusInternalServerError, "send_failed")
	if elapsed > limit+500*time.Millisecond {
		t.Fatalf("answered after %v, want within the %v reply limit", elapsed, limit)
	}
	if n := f.count(maxclient.OpMsgSend); n != 1 {
		t.Fatalf("MAX received %d send frames, want 1", n)
	}
}

// The session dying after the frame went out surfaces from maxclient as
// ErrNotConnected; MAX may already have delivered the message, so it must not
// be reported as not_connected, which callers retry.
func TestSendTextConnectionLostAfterWriteIsSendFailed(t *testing.T) {
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return nil })
	userID, c := connectedUser(t, f)
	go func() {
		for op := range f.seen {
			if op == int(maxclient.OpMsgSend) {
				c.Close()
				return
			}
		}
	}()

	status, body := call(t, (&server{}).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)

	wantError(t, status, body, http.StatusInternalServerError, "send_failed")
}

func TestSendTextSuccessKeepsResponse(t *testing.T) {
	cases := []struct {
		name, body string
		chatID     int64
	}{
		{"by chatId", `{"chatId": 5, "text": "hello"}`, 5},
		{"by phone", `{"phone": "` + testPhone + `", "text": "hello"}`, maxclient.GetDialogID(testSelfID, testContactID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
				if fr.Opcode == int(maxclient.OpContactInfoByPhone) {
					return contactReply(fr)
				}
				return sentReply(fr)
			})
			userID, _ := connectedUser(t, f)

			status, body := call(t, (&server{}).SendMessage(), userID, tc.body)

			want := map[string]interface{}{"success": true, "messageId": "msg-1", "chatId": float64(tc.chatID)}
			if status != http.StatusOK || !reflect.DeepEqual(body, want) {
				t.Fatalf("got %d %v, want 200 %v", status, body, want)
			}
			if got := f.lastPayload(maxclient.OpMsgSend)["chatId"]; got != float64(tc.chatID) {
				t.Fatalf("sent to chatId %v, want %d", got, tc.chatID)
			}
		})
	}
}

func TestCheckUserReportsLookupErrors(t *testing.T) {
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		switch fr.Payload["phone"] {
		case "79000000001":
			return contactReply(fr)
		case "79000000003":
			return map[string]interface{}{"error": "too.many.requests", "message": "Too many requests"}
		}
		return map[string]interface{}{}
	})
	userID, _ := connectedUser(t, f)

	status, body := call(t, (&server{}).CheckUser(), userID,
		`{"phone": ["79000000001", "79000000002", "79000000003", "12-34"]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d %v, want 200", status, body)
	}
	users, _ := body["users"].([]interface{})
	if len(users) != 4 {
		t.Fatalf("users = %v, want 4 entries", body["users"])
	}
	item := func(i int) map[string]interface{} { return users[i].(map[string]interface{}) }

	if found := item(0); found["exists"] != true || found["maxUserId"] != float64(testContactID) || found["error"] != nil {
		t.Fatalf("found number: %v", found)
	}
	if missing := item(1); missing["exists"] != false || missing["error"] != nil {
		t.Fatalf("number MAX does not know: %v, want exists:false without error", missing)
	}
	for _, i := range []int{2, 3} {
		failed := item(i)
		if msg, _ := failed["error"].(string); failed["exists"] != false || msg == "" {
			t.Fatalf("lookup error for %v: %v, want exists:false with error", failed["phone"], failed)
		}
	}
}
