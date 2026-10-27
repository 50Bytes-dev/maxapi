package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestMain(m *testing.M) {
	// init() loads .env, which may name a real global webhook: the events
	// the tests cause go only to their own receivers.
	*globalWebhook = ""
	os.Exit(m.Run())
}

// newTestServer returns a server on a fresh SQLite database.
func newTestServer(t *testing.T) *server {
	t.Helper()
	return newTestServerAt(t, t.TempDir())
}

// newTestServerAt returns a server on the SQLite database in dir, with the
// pauses stored there restored, as at a gateway start. Two servers on one dir
// are the gateway before and after a restart.
func newTestServerAt(t *testing.T, dir string) *server {
	t.Helper()
	db, err := initializeSQLite(DatabaseConfig{Path: dir})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := initializeSchema(db); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	s := &server{db: db, startTime: time.Now(), pauses: newPauseRegistry()}
	if err := s.restorePauses(); err != nil {
		t.Fatalf("restore pauses: %v", err)
	}
	return s
}

// tokenOf is the API token of test user userID.
func tokenOf(userID string) string { return userID + "-token" }

// addUser stores gateway user userID with a MAX login into account testSelfID.
func addUser(t *testing.T, s *server, userID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO users (id, name, token, auth_token, max_user_id, connected) VALUES ($1, $2, $3, 'stored-auth-token', $4, 1)`,
		userID, userID, tokenOf(userID), testSelfID); err != nil {
		t.Fatalf("add user %s: %v", userID, err)
	}
}

// sessionUser stores gateway user userID and gives it a live session with
// the fake MAX server.
func sessionUser(t *testing.T, s *server, f *fakeMAX, userID string) *maxclient.Client {
	t.Helper()
	addUser(t, s, userID)
	return startSession(t, f, userID)
}

// startSession gives stored gateway user userID a live session with the fake
// MAX server, logged into MAX account testSelfID.
func startSession(t *testing.T, f *fakeMAX, userID string) *maxclient.Client {
	t.Helper()
	c := maxclient.NewClient("test-device", zerolog.Nop())
	c.URI = f.uri()
	c.MaxUserID = testSelfID
	if err := c.Connect(); err != nil {
		t.Fatalf("connect to fake MAX: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	clientManager.SetMaxClient(userID, c)
	t.Cleanup(func() { clientManager.DeleteMaxClient(userID) })
	return c
}

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

// uri is the fake server's WebSocket endpoint.
func (f *fakeMAX) uri() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

// total returns how many frames the server received.
func (f *fakeMAX) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
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
	c.URI = f.uri()
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
	ctx := context.WithValue(req.Context(), "userinfo", Values{map[string]string{"Id": userID, "Token": tokenOf(userID)}})
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
			status, body := call(t, newTestServer(t).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)
			wantError(t, status, body, http.StatusServiceUnavailable, "not_connected")
		})
	}
}

// Without a target maxapi used to send to chatId 0, the account's own
// "Favorites" chat.
func TestSendTextWithoutTargetIsInvalidTarget(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)

	status, body := call(t, newTestServer(t).SendMessage(), userID, `{"text": "hello"}`)

	wantError(t, status, body, http.StatusBadRequest, "invalid_target")
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d send frames, want 0", n)
	}
}

// emptyReply is a MAX answer without a contact or an error.
func emptyReply(fakeFrame) map[string]interface{} { return map[string]interface{}{} }

// notFoundReply is MAX's answer to the lookup of a number without a MAX
// account, as seen in production on 2026-10-27.
func notFoundReply(fakeFrame) map[string]interface{} {
	return map[string]interface{}{"error": "not.found", "title": "Не найдено", "message": "Cannot found contact by phone"}
}

func TestSendTextUnknownPhoneIsAddressNotFound(t *testing.T) {
	cases := []struct {
		name, phone string
		reply       func(fakeFrame) map[string]interface{}
		lookups     int
		upstream    interface{} // upstreamCode in the answer, nil for none
	}{
		{"MAX answers without a contact", testPhone, emptyReply, 1, nil}, // ErrUserNotFound
		{"MAX answers not.found", testPhone, notFoundReply, 1, "not.found"},
		{"malformed number", "12-34", emptyReply, 0, nil}, // ErrInvalidPhone, rejected before asking MAX
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMAX(t, tc.reply)
			s := newTestServer(t)
			userID := t.Name()
			sessionUser(t, s, f, userID)

			status, body := call(t, s.SendMessage(), userID, `{"phone": "`+tc.phone+`", "text": "hello"}`)

			wantError(t, status, body, http.StatusNotFound, "address_not_found")
			if body["upstreamCode"] != tc.upstream {
				t.Fatalf("upstreamCode = %v, want %v", body["upstreamCode"], tc.upstream)
			}
			if n := f.count(maxclient.OpContactInfoByPhone); n != tc.lookups {
				t.Fatalf("MAX received %d lookups, want %d", n, tc.lookups)
			}
			if n := f.count(maxclient.OpMsgSend); n != 0 {
				t.Fatalf("MAX received %d send frames, want 0", n)
			}
			if body["paused"] != nil || s.pauseOf(testSelfID) != nil {
				t.Fatalf("an unknown number paused the account: %v", body)
			}
		})
	}
}

// A MAX error on the lookup other than not.found says nothing about the
// number: lookup_failed with MAX's code, and the number is not cached.
func TestSendTextLookupErrorIsLookupFailed(t *testing.T) {
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "service.unavailable", "message": "Try later"}
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	for i := 1; i <= 2; i++ {
		status, body := call(t, s.SendMessage(), userID, `{"phone": "`+testPhone+`", "text": "hello"}`)

		wantError(t, status, body, http.StatusServiceUnavailable, "lookup_failed")
		if body["upstreamCode"] != "service.unavailable" || body["paused"] != nil {
			t.Fatalf("send %d: %v, want upstreamCode service.unavailable and no pause", i, body)
		}
		if n := f.count(maxclient.OpContactInfoByPhone); n != i {
			t.Fatalf("send %d: MAX received %d lookups, want %d", i, n, i)
		}
	}
}

func TestSendTextLookupTimeoutIsLookupFailed(t *testing.T) {
	t.Parallel()
	const limit = 4 * time.Second
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return nil })
	userID, _ := connectedUser(t, f)

	start := time.Now()
	status, body := call(t, newTestServer(t).SendMessage(), userID, `{"phone": "`+testPhone+`", "text": "hello"}`)
	elapsed := time.Since(start)

	wantError(t, status, body, http.StatusServiceUnavailable, "lookup_failed")
	if elapsed > limit+500*time.Millisecond {
		t.Fatalf("answered after %v, want within the %v lookup limit", elapsed, limit)
	}
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d send frames, want 0", n)
	}
}

// MAX's limit and restriction codes also pause the account: pauses_test.go.
func TestSendTextRejectedByMAXIsSendRejected(t *testing.T) {
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{
			"error": "chat.denied", "message": "Chat denied", "title": "Denied",
		}
	})
	userID, _ := connectedUser(t, f)

	status, body := call(t, newTestServer(t).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)

	wantError(t, status, body, http.StatusUnprocessableEntity, "send_rejected")
	if body["upstreamCode"] != "chat.denied" {
		t.Fatalf("upstreamCode = %v, want MAX's code chat.denied", body["upstreamCode"])
	}
	if body["paused"] != nil {
		t.Fatalf("an ordinary refusal paused the account: %v", body)
	}
}

func TestSendTextReplyTimeoutIsSendFailed(t *testing.T) {
	t.Parallel()
	const limit = 5 * time.Second
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} { return nil })
	userID, _ := connectedUser(t, f)

	start := time.Now()
	status, body := call(t, newTestServer(t).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)
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

	status, body := call(t, newTestServer(t).SendMessage(), userID, `{"chatId": 5, "text": "hello"}`)

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

			status, body := call(t, newTestServer(t).SendMessage(), userID, tc.body)

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
		case "79000000004":
			return notFoundReply(fr)
		}
		return map[string]interface{}{}
	})
	userID, _ := connectedUser(t, f)

	status, body := call(t, (&server{}).CheckUser(), userID,
		`{"phone": ["79000000001", "79000000002", "79000000003", "12-34", "79000000004"]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d %v, want 200", status, body)
	}
	users, _ := body["users"].([]interface{})
	if len(users) != 5 {
		t.Fatalf("users = %v, want 5 entries", body["users"])
	}
	item := func(i int) map[string]interface{} { return users[i].(map[string]interface{}) }

	if found := item(0); found["exists"] != true || found["maxUserId"] != float64(testContactID) || found["error"] != nil {
		t.Fatalf("found number: %v", found)
	}
	for _, i := range []int{1, 4} {
		if missing := item(i); missing["exists"] != false || missing["error"] != nil {
			t.Fatalf("number MAX does not know: %v, want exists:false without error", missing)
		}
	}
	for _, i := range []int{2, 3} {
		failed := item(i)
		if msg, _ := failed["error"].(string); failed["exists"] != false || msg == "" {
			t.Fatalf("lookup error for %v: %v, want exists:false with error", failed["phone"], failed)
		}
	}
}
