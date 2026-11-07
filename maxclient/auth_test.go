package maxclient

import (
	"bytes"
	"encoding/json"
	"errors"
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

// scriptedServer is a stand-in for MAX: answer returns the frames to write
// for each request (nil: none). It records each request and counts open
// connections.
type scriptedServer struct {
	uri    string
	answer func(req Response, raw json.RawMessage) []map[string]interface{}

	mu       sync.Mutex
	requests []Response
	raws     []json.RawMessage
	open     atomic.Int32
}

func newScriptedServer(t *testing.T, answer func(req Response, raw json.RawMessage) []map[string]interface{}) *scriptedServer {
	t.Helper()
	s := &scriptedServer{answer: answer}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.open.Add(1)
		defer func() { s.open.Add(-1); conn.Close() }()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req Response
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			var raw struct {
				Payload json.RawMessage `json:"payload"`
			}
			_ = json.Unmarshal(data, &raw)
			s.mu.Lock()
			s.requests = append(s.requests, req)
			s.raws = append(s.raws, raw.Payload)
			s.mu.Unlock()
			for _, frame := range s.answer(req, raw.Payload) {
				out, _ := json.Marshal(frame)
				if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	s.uri = "ws" + strings.TrimPrefix(srv.URL, "http")
	return s
}

func (s *scriptedServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// answerWith answers every request with payload, cmd 1.
func answerWith(payload map[string]interface{}) func(Response, json.RawMessage) []map[string]interface{} {
	return func(req Response, _ json.RawMessage) []map[string]interface{} {
		return []map[string]interface{}{{"ver": req.Ver, "cmd": 1, "seq": req.Seq, "opcode": req.Opcode, "payload": payload}}
	}
}

func connected(t *testing.T, uri string, logs *bytes.Buffer) *Client {
	t.Helper()
	logger := zerolog.Nop()
	if logs != nil {
		logger = zerolog.New(logs)
	}
	c := NewClient("test-device", logger)
	c.URI = uri
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Shutdown)
	return c
}

func TestConfirmQRLoginSteps(t *testing.T) {
	cases := []struct {
		name        string
		answer      map[string]interface{}
		token       string
		passwordFor string
		unsupported string
	}{
		{
			name: "token",
			answer: map[string]interface{}{
				"tokenAttrs": map[string]interface{}{"LOGIN": map[string]interface{}{"token": "T1"}},
				"profile":    map[string]interface{}{"contact": map[string]interface{}{"id": 42}},
			},
			token: "T1",
		},
		{
			// The answer of an account with a cloud password, as
			// max-account-api records it: tokenAttrs empty.
			name: "cloud password",
			answer: map[string]interface{}{
				"tokenAttrs": map[string]interface{}{},
				"passwordChallenge": map[string]interface{}{
					"trackId": "pw-1", "hint": "cat", "email": "o***@mail.ru",
					"config": map[string]interface{}{"passMinLen": 6, "passMaxLen": 64, "hintMaxLen": 30},
				},
			},
			passwordFor: "pw-1",
		},
		{
			name:        "another token kind",
			answer:      map[string]interface{}{"tokenAttrs": map[string]interface{}{"REGISTER": map[string]interface{}{"token": "R"}}},
			unsupported: "REGISTER",
		},
		{
			name:        "no tokenAttrs",
			answer:      map[string]interface{}{"something": true},
			unsupported: "something",
		},
		{
			name:        "empty",
			answer:      map[string]interface{}{},
			unsupported: "empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := connected(t, newScriptedServer(t, answerWith(tc.answer)).uri, &logs)
			step, err := c.ConfirmQRLogin("track")
			if err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if step.Token != tc.token || step.Unsupported != tc.unsupported {
				t.Fatalf("step = %+v", step)
			}
			if tc.passwordFor != "" {
				if step.Password == nil || step.Password.TrackID != tc.passwordFor || step.Password.Hint != "cat" || step.Password.Email != "o***@mail.ru" {
					t.Fatalf("password challenge = %+v", step.Password)
				}
			} else if step.Password != nil {
				t.Fatalf("unexpected password challenge %+v", step.Password)
			}
			if tc.token != "" && c.MaxUserID != 42 {
				t.Fatalf("MaxUserID = %d, want the profile's 42", c.MaxUserID)
			}
			if strings.Contains(logs.String(), `"token":"`) || strings.Contains(logs.String(), "T1") {
				t.Fatalf("the log carries a token: %s", logs.String())
			}
			if !strings.Contains(logs.String(), "payloadKeys") {
				t.Fatalf("the keys of the answer are not logged: %s", logs.String())
			}
		})
	}
}

func TestSubmitPassword(t *testing.T) {
	srv := newScriptedServer(t, answerWith(map[string]interface{}{
		"tokenAttrs": map[string]interface{}{"LOGIN": map[string]interface{}{"token": "T2"}},
	}))
	c := connected(t, srv.uri, nil)
	step, err := c.SubmitPassword("pw-1", "secret")
	if err != nil || step.Token != "T2" {
		t.Fatalf("SubmitPassword = %+v, %v", step, err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	last := srv.requests[len(srv.requests)-1]
	if last.Opcode != int(OpAuthLoginCheckPassword) || last.Payload["trackId"] != "pw-1" || last.Payload["password"] != "secret" {
		t.Fatalf("sent opcode %d %v", last.Opcode, last.Payload)
	}
}

func TestPasswordRefusal(t *testing.T) {
	for code, want := range map[string]string{
		"password2fa.wrong":                 PasswordWrong,
		"error.password.invalid":            PasswordWrong,
		"second.factor.disabled.update.app": PasswordUnsupported,
		"track.not.found":                   PasswordExpired,
		"password.attempts.exceeded":        PasswordExpired,
		"time_limit_expired_logout":         PasswordExpired,
		"login.flood":                       PasswordExpired,
		"service.unavailable":               PasswordOther,
	} {
		if got := PasswordRefusal(code); got != want {
			t.Errorf("PasswordRefusal(%q) = %q, want %q", code, got, want)
		}
	}
}

// MAX numbers the frames it pushes by a counter of its own: a push may
// carry the seq of a request that waits. It is a push, not the answer.
func TestPushWithAPendingSeqIsNoAnswer(t *testing.T) {
	srv := newScriptedServer(t, func(req Response, _ json.RawMessage) []map[string]interface{} {
		return []map[string]interface{}{
			{"ver": 11, "cmd": 0, "seq": req.Seq, "opcode": int(OpNotifMessage), "payload": map[string]interface{}{"chatId": 1}},
			{"ver": 11, "cmd": 1, "seq": req.Seq, "opcode": int(OpAuthQRConfirm), "payload": map[string]interface{}{"unrelated": true}},
			{"ver": 11, "cmd": 1, "seq": req.Seq, "opcode": req.Opcode, "payload": map[string]interface{}{
				"tokenAttrs": map[string]interface{}{"LOGIN": map[string]interface{}{"token": "T3"}},
			}},
		}
	})
	c := NewClient("test-device", zerolog.Nop())
	c.URI = srv.uri
	pushes := make(chan Event, 4)
	c.SetEventHandler(func(e Event) { pushes <- e })
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Shutdown)

	resp, err := c.sendAndWait(OpAuthQRConfirm+1000, map[string]interface{}{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, ok := resp.Payload["tokenAttrs"]; !ok {
		t.Fatalf("the request got %v, not its answer", resp.Payload)
	}
	select {
	case e := <-pushes:
		if e.Opcode != OpNotifMessage {
			t.Fatalf("push = %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the push did not reach the event handler")
	}
}

// After Shutdown nothing reaches MAX: the connection is closed, a request
// waiting for its answer fails at once, and the client never connects
// again.
func TestShutdownEndsTheClientForGood(t *testing.T) {
	srv := newScriptedServer(t, func(req Response, _ json.RawMessage) []map[string]interface{} {
		if Opcode(req.Opcode) == OpAuthQRStatus {
			return nil // never answered
		}
		return answerWith(map[string]interface{}{})(req, nil)
	})
	SetPingInterval(10 * time.Millisecond)
	t.Cleanup(func() { SetPingInterval(PingInterval) })
	c := connected(t, srv.uri, nil)
	c.StartPingLoop()

	waiting := make(chan error, 1)
	go func() {
		_, err := c.PollQRStatus("track")
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.Shutdown()

	select {
	case err := <-waiting:
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("the waiting request = %v, want ErrNotConnected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the waiting request did not fail at Shutdown")
	}
	deadline := time.Now().Add(2 * time.Second)
	for srv.open.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection stayed open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := srv.count()
	time.Sleep(100 * time.Millisecond)
	if err := c.Connect(); !errors.Is(err, ErrClientShutDown) {
		t.Fatalf("Connect after Shutdown = %v", err)
	}
	if _, err := c.sendAndWait(OpPing, map[string]interface{}{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a request after Shutdown = %v", err)
	}
	if after := srv.count(); after != before {
		t.Fatalf("MAX received %d frames after Shutdown", after-before)
	}
	c.Shutdown() // twice is fine
}

// An event handler may shut its own client down: Close would wait for the
// receive goroutine it runs on.
func TestShutdownFromTheEventHandler(t *testing.T) {
	srv := newScriptedServer(t, func(req Response, _ json.RawMessage) []map[string]interface{} {
		return []map[string]interface{}{{"ver": 11, "cmd": 0, "seq": 1, "opcode": int(OpLogout), "payload": map[string]interface{}{}}}
	})
	c := NewClient("test-device", zerolog.Nop())
	c.URI = srv.uri
	done := make(chan struct{})
	c.SetEventHandler(func(Event) { c.Shutdown(); close(done) })
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	go func() { _, _ = c.sendAndWait(OpPing, map[string]interface{}{}) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown on the receive goroutine did not return")
	}
}
