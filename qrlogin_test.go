package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// qrMAX is a fake MAX for QR logins: it issues numbered QRs (track-1,
// track-2, ...), reports a scan once scanned is set, and answers the
// confirmation and the cloud password with confirm and password.
type qrMAX struct {
	*fakeMAX
	scanned  atomic.Bool
	qrs      atomic.Int32
	confirm  func(fakeFrame) map[string]interface{}
	password func(fakeFrame) map[string]interface{}
	qrStart  func(fakeFrame) map[string]interface{} // nil: issue a QR
}

// lockedBuffer is a log sink goroutines may write to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the gateway's log to a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	old := log.Logger
	log.Logger = zerolog.New(logs)
	t.Cleanup(func() { log.Logger = old })
	return logs
}

// loginToken is the LOGIN token the fake hands out.
const loginToken = "fake-login-token"

func loginAnswer(token string) map[string]interface{} {
	return map[string]interface{}{
		"tokenAttrs": map[string]interface{}{"LOGIN": map[string]interface{}{"token": token}},
		"profile": map[string]interface{}{"contact": map[string]interface{}{
			"id": testSelfID, "names": []interface{}{map[string]interface{}{"name": "Shop"}},
		}},
	}
}

func newQRMAX(t *testing.T) *qrMAX {
	t.Helper()
	m := &qrMAX{confirm: func(fakeFrame) map[string]interface{} { return loginAnswer(loginToken) }}
	m.fakeMAX = newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		switch maxclient.Opcode(fr.Opcode) {
		case maxclient.OpAuthQRStart:
			if m.qrStart != nil {
				return m.qrStart(fr)
			}
			n := m.qrs.Add(1)
			return map[string]interface{}{
				"qrLink":    fmt.Sprintf("https://max.ru/:auth/%d", n),
				"trackId":   fmt.Sprintf("track-%d", n),
				"ttl":       60000,
				"expiresAt": time.Now().Add(time.Minute).UnixMilli(),
			}
		case maxclient.OpAuthQRStatus:
			return map[string]interface{}{"status": map[string]interface{}{"loginAvailable": m.scanned.Load()}}
		case maxclient.OpAuthQRConfirm:
			return m.confirm(fr)
		case maxclient.OpAuthLoginCheckPassword:
			return m.password(fr)
		case maxclient.OpLogin:
			return loginAnswer("")
		}
		return map[string]interface{}{}
	})
	// Every login and session of the test dials the fake; polls, reconnects
	// and pings take milliseconds.
	restore := func(set func()) { t.Cleanup(set) }
	oldURI, oldPoll, oldDelay := maxWSURI, qrPollDefault, qrReconnectDelay
	restore(func() {
		maxWSURI, qrPollDefault, qrReconnectDelay = oldURI, oldPoll, oldDelay
		maxclient.SetPingInterval(maxclient.PingInterval)
	})
	maxWSURI = m.uri()
	qrPollDefault = 10 * time.Millisecond
	qrReconnectDelay = 10 * time.Millisecond
	maxclient.SetPingInterval(20 * time.Millisecond)
	return m
}

// waitEvents returns the first event of each of types, delivered in any
// order: deliveries run concurrently, so QRAuthorized and Sync may swap.
func (k *webhookSink) waitEvents(t *testing.T, types ...string) map[string]map[string]string {
	t.Helper()
	got := map[string]map[string]string{}
	deadline := time.After(5 * time.Second)
	for len(got) < len(types) {
		select {
		case ev := <-k.events:
			for _, typ := range types {
				if ev["type"] == typ && got[typ] == nil {
					got[typ] = ev
				}
			}
		case <-deadline:
			t.Fatalf("webhooks %v arrived, want %v", keysOf(got), types)
		}
	}
	return got
}

func keysOf(m map[string]map[string]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// qrUser stores a gateway user without a MAX login whose webhooks go to sink,
// and deletes it (and so whatever runs for it) when the test ends.
func qrUser(t *testing.T, s *server, sink *webhookSink) string {
	t.Helper()
	userID := strings.ReplaceAll(t.Name(), "/", "-")
	if _, err := s.db.Exec(
		`INSERT INTO users (id, name, token, webhook, events, connected) VALUES ($1, $1, $2, $3, 'All', 0)`,
		userID, tokenOf(userID), sink.srv.URL); err != nil {
		t.Fatalf("add user %s: %v", userID, err)
	}
	t.Cleanup(func() { _ = s.removeUser(userID) })
	return userID
}

// request sends a request as the user (token) or the admin through s's
// router and decodes the JSON body.
func request(t *testing.T, s *server, method, path, userID, body string) (int, map[string]interface{}) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if strings.HasPrefix(path, "/admin/") {
		r.Header.Set("Authorization", adminTestToken)
	} else {
		r.Header.Set("token", tokenOf(userID))
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	var got map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

func connectQR(t *testing.T, s *server, userID string) {
	t.Helper()
	status, body := request(t, s, "POST", "/session/connect", userID, `{"subscribe":["All"],"immediate":true}`)
	if status != http.StatusOK || body["details"] != "Awaiting QR scan" {
		t.Fatalf("connect = %d %v, want 200 Awaiting QR scan", status, body)
	}
}

// eventually waits up to 5 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertSilent fails if the fake receives a frame, or has a WebSocket open,
// after everything of the user was to stop: pings and polls run every few
// milliseconds in these tests, so 300 ms of silence means none is left.
func assertSilent(t *testing.T, m *qrMAX) {
	t.Helper()
	eventually(t, "every WebSocket to MAX closed", func() bool { return m.open() == 0 })
	before := m.total()
	time.Sleep(300 * time.Millisecond)
	if after := m.total(); after != before {
		t.Fatalf("MAX received %d frames after the stop", after-before)
	}
	if m.open() != 0 {
		t.Fatalf("%d WebSockets to MAX opened after the stop", m.open())
	}
}

func TestQRLoginWithoutPassword(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)

	connectQR(t, s, userID)
	if ev := sink.waitFor(t, "QRGenerated"); ev["trackId"] != "track-1" || !strings.HasPrefix(ev["qrCodeBase64"], "data:image/png;base64,") {
		t.Fatalf("QRGenerated = %v", ev)
	}
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	if status != http.StatusOK || body["scanned"] != false || !strings.HasPrefix(fmt.Sprint(body["qrcode"]), "data:image/png") {
		t.Fatalf("GET /session/qr = %d %v, want 200 with the QR", status, body)
	}
	if status, body := request(t, s, "GET", "/session/status", userID, ""); body["connected"] != false {
		t.Fatalf("status during the QR login = %d %v, want connected false", status, body)
	}

	m.scanned.Store(true)
	evs := sink.waitEvents(t, "QRScanned", "QRAuthorized", "Sync")
	if evs["QRAuthorized"]["authToken"] != loginToken {
		t.Fatalf("QRAuthorized = %v", evs["QRAuthorized"])
	}
	if got := storedAuthToken(t, s, userID); got != loginToken {
		t.Fatalf("stored login = %q", got)
	}
	status, body = request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusConflict, CodeAlreadyLoggedIn)
	eventually(t, "the session up", func() bool { return clientManager.IsConnected(userID) })
	// The login's own WebSocket is closed; only the session's stays.
	eventually(t, "one WebSocket to MAX", func() bool { return m.open() == 1 })
}

func TestQRLoginWithCloudPassword(t *testing.T) {
	logs := captureLog(t)

	m := newQRMAX(t)
	m.confirm = func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{
			"tokenAttrs":        map[string]interface{}{},
			"passwordChallenge": map[string]interface{}{"trackId": "pw-track", "hint": "the cat", "email": "o***@mail.ru"},
		}
	}
	m.password = func(fr fakeFrame) map[string]interface{} {
		if fr.Payload["trackId"] != "pw-track" {
			return map[string]interface{}{"error": "track.not.found", "message": "no track"}
		}
		if fr.Payload["password"] == "right-secret" {
			return loginAnswer("pw-login-token")
		}
		return map[string]interface{}{"error": "error.password.invalid", "message": "wrong password"}
	}
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)

	status, body := request(t, s, "POST", "/session/password", userID, `{"password":"too-early"}`)
	wantError(t, status, body, http.StatusGone, LoginExpired)

	connectQR(t, s, userID)
	sink.waitFor(t, "QRGenerated")
	status, body = request(t, s, "POST", "/session/password", userID, `{"password":"too-early"}`)
	wantError(t, status, body, http.StatusConflict, CodePasswordNotRequired)

	m.scanned.Store(true)
	ev := sink.waitFor(t, "QRPasswordRequired")
	if ev["hint"] != "the cat" || ev["email"] != "o***@mail.ru" {
		t.Fatalf("QRPasswordRequired = %v", ev)
	}
	status, body = request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusConflict, CodePasswordRequired)

	status, body = request(t, s, "POST", "/session/password", userID, `{"password":"wrong-secret"}`)
	wantError(t, status, body, http.StatusUnprocessableEntity, CodeInvalidPassword)
	if body["upstreamCode"] != "error.password.invalid" {
		t.Fatalf("upstreamCode = %v, want MAX's code", body["upstreamCode"])
	}
	status, body = request(t, s, "POST", "/session/password", userID, `{"password":""}`)
	wantError(t, status, body, http.StatusBadRequest, ErrInvalidInput)

	status, body = request(t, s, "POST", "/session/password", userID, `{"password":"right-secret"}`)
	if status != http.StatusOK || body["accepted"] != true {
		t.Fatalf("right password = %d %v, want 200 accepted", status, body)
	}
	if evs := sink.waitEvents(t, "QRAuthorized", "Sync"); evs["QRAuthorized"]["authToken"] != "pw-login-token" {
		t.Fatalf("QRAuthorized = %v", evs["QRAuthorized"])
	}
	if got := storedAuthToken(t, s, userID); got != "pw-login-token" {
		t.Fatalf("stored login = %q", got)
	}
	status, body = request(t, s, "POST", "/session/password", userID, `{"password":"right-secret"}`)
	wantError(t, status, body, http.StatusConflict, CodeAlreadyLoggedIn)

	for _, secret := range []string{"right-secret", "wrong-secret", "pw-login-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log carries %q", secret)
		}
	}
	// The keys of MAX's answer are logged, for when MAX changes it again.
	if !strings.Contains(logs.String(), `"payloadKeys":["passwordChallenge","tokenAttrs"]`) {
		t.Fatalf("the keys of the confirmation answer are not in the log:\n%s", logs.String())
	}
}

func TestQRLoginEndsAfterTooManyWrongPasswords(t *testing.T) {
	old := maxPasswordAttempts
	maxPasswordAttempts = 2
	t.Cleanup(func() { maxPasswordAttempts = old })

	m := newQRMAX(t)
	m.confirm = func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"passwordChallenge": map[string]interface{}{"trackId": "pw-track"}}
	}
	m.password = func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "error.password.invalid"}
	}
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	m.scanned.Store(true)
	sink.waitFor(t, "QRPasswordRequired")

	status, body := request(t, s, "POST", "/session/password", userID, `{"password":"a"}`)
	wantError(t, status, body, http.StatusUnprocessableEntity, CodeInvalidPassword)
	status, body = request(t, s, "POST", "/session/password", userID, `{"password":"b"}`)
	wantError(t, status, body, http.StatusGone, LoginExpired)
	if ev := sink.waitFor(t, "QRExpired"); ev["reason"] != LoginExpired {
		t.Fatalf("QRExpired = %v", ev)
	}
	status, body = request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusGone, LoginExpired)
	assertSilent(t, m)
}

// A confirmation MAX answers with no LOGIN token and no step the gateway
// knows ended as "No LOGIN token in response" (no_login_token), 90 of 97
// times on 2026-11-07: now the login ends with login_unsupported naming
// what MAX asked for, and the keys go to the log, never the values.
func TestQRLoginUnsupportedStep(t *testing.T) {
	logs := captureLog(t)

	m := newQRMAX(t)
	m.confirm = func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"tokenAttrs": map[string]interface{}{"REGISTER": map[string]interface{}{"token": "reg-secret"}}}
	}
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	m.scanned.Store(true)

	ev := sink.waitFor(t, "QRExpired")
	if ev["reason"] != LoginUnsupported || ev["step"] != "REGISTER" {
		t.Fatalf("QRExpired = %v, want login_unsupported with step REGISTER", ev)
	}
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusGone, LoginUnsupported)
	if strings.Contains(logs.String(), "reg-secret") {
		t.Fatal("the log carries the token MAX handed out")
	}
	if !strings.Contains(logs.String(), `"tokenAttrsKeys":["REGISTER"]`) {
		t.Fatalf("the tokenAttrs keys are not in the log:\n%s", logs.String())
	}
	assertSilent(t, m)
}

func TestQRWithoutLoginIsGoneOrConflict(t *testing.T) {
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusGone, LoginExpired)

	addUser(t, s, "with-login")
	t.Cleanup(func() { _ = s.removeUser("with-login") })
	status, body = request(t, s, "GET", "/session/qr", "with-login", "")
	wantError(t, status, body, http.StatusConflict, CodeAlreadyLoggedIn)
}

// DELETE /admin/users answered 200 while the user's QR WebSocket lived on,
// pinging every 30 s and polling the QR every 5 s: ~20 such orphans in 5
// hours on 2026-11-07.
func TestDeleteStopsTheQRLogin(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	sink.waitFor(t, "QRGenerated")
	eventually(t, "the QR polled", func() bool { return m.count(maxclient.OpAuthQRStatus) > 0 })

	if status, _ := request(t, s, "DELETE", "/admin/users/"+userID, "", ""); status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
	assertSilent(t, m)
	if qrLogins.running(userID) != nil {
		t.Fatal("the QR login is still registered")
	}
	for _, typ := range sink.delivered() {
		if typ == "QRExpired" {
			t.Fatal("a delete sent QRExpired")
		}
	}
}

func TestDeleteStopsTheSession(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	m.scanned.Store(true)
	sink.waitFor(t, "Sync")
	eventually(t, "the session pinged", func() bool { return m.count(maxclient.OpPing) > 3 })

	if status, _ := request(t, s, "DELETE", "/admin/users/"+userID, "", ""); status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
	assertSilent(t, m)
	if clientManager.GetMaxClient(userID) != nil {
		t.Fatal("the session is still registered")
	}
}

// POST /session/disconnect left the QR WebSocket open (it closed only a
// client that was not connected) and the watcher polling on it.
func TestDisconnectStopsTheQRLogin(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	sink.waitFor(t, "QRGenerated")

	if status, _ := request(t, s, "POST", "/session/disconnect", userID, ""); status != http.StatusOK {
		t.Fatalf("disconnect = %d", status)
	}
	assertSilent(t, m)
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusGone, LoginExpired)
}

// Each connect replaces the running login: its WebSocket closes, and only
// the new one stays open.
func TestRepeatedConnectsLeaveOneLogin(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	for i := 0; i < 3; i++ {
		connectQR(t, s, userID)
	}
	eventually(t, "one WebSocket to MAX", func() bool { return m.open() == 1 })
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	if status != http.StatusOK {
		t.Fatalf("GET /session/qr = %d %v", status, body)
	}
}

// A WebSocket that dropped during the QR login (close 1006) was polled
// forever: "QR poll failed: WebSocket is not connected". Now the login
// connects again, shows a new QR, and goes on.
func TestQRLoginReconnectsAfterADrop(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	sink.waitFor(t, "QRGenerated")
	eventually(t, "the QR polled", func() bool { return m.count(maxclient.OpAuthQRStatus) > 0 })

	m.drop()
	if ev := sink.waitFor(t, "QRGenerated"); ev["trackId"] != "track-2" {
		t.Fatalf("QRGenerated after the drop = %v, want the new QR track-2", ev)
	}
	m.scanned.Store(true)
	sink.waitFor(t, "QRAuthorized")
	if got := m.lastPayload(maxclient.OpAuthQRConfirm)["trackId"]; got != "track-2" {
		t.Fatalf("confirmed %v, want track-2", got)
	}
}

func TestQRLoginGivesUpWhenMAXCannotBeReached(t *testing.T) {
	old := maxQRReconnects
	maxQRReconnects = 2
	t.Cleanup(func() { maxQRReconnects = old })

	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	sink.waitFor(t, "QRGenerated")

	m.refuse.Store(true)
	m.drop()
	if ev := sink.waitFor(t, "QRExpired"); ev["reason"] != LoginFailed {
		t.Fatalf("QRExpired = %v, want login_failed", ev)
	}
	status, body := request(t, s, "GET", "/session/qr", userID, "")
	wantError(t, status, body, http.StatusGone, LoginFailed)
	assertSilent(t, m)

	// The outcome stays until the next connect.
	m.refuse.Store(false)
	connectQR(t, s, userID)
	if status, _ := request(t, s, "GET", "/session/qr", userID, ""); status != http.StatusOK {
		t.Fatalf("GET /session/qr after a new connect = %d", status)
	}
}

func TestConnectWhenMAXRefusesTheQR(t *testing.T) {
	m := newQRMAX(t)
	m.qrStart = func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "service.unavailable", "message": "try later"}
	}
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	status, body := request(t, s, "POST", "/session/connect", userID, `{"subscribe":["All"]}`)
	wantError(t, status, body, http.StatusServiceUnavailable, CodeLoginUnavailable)
	if body["upstreamCode"] != "service.unavailable" {
		t.Fatalf("upstreamCode = %v, want MAX's code", body["upstreamCode"])
	}
	assertSilent(t, m)
}

// MAX ending a session (opcode 20) erases the login to NULL; POST
// /session/connect then answered 500 "DB error: ... converting NULL to
// string" (live, 2026-11-07), so the user could never log in again.
func TestConnectAfterMAXEndedTheSession(t *testing.T) {
	m := newQRMAX(t)
	s := routedServer(t)
	sink := newWebhookSink(t)
	userID := qrUser(t, s, sink)
	connectQR(t, s, userID)
	m.scanned.Store(true)
	sink.waitEvents(t, "QRAuthorized", "Sync")
	eventually(t, "the session up", func() bool { return clientManager.IsConnected(userID) })

	s.endLogin(userID) // what MAX's LoggedOut push does
	if got := storedAuthToken(t, s, userID); got != "" {
		t.Fatalf("stored login = %q, want it erased", got)
	}
	eventually(t, "the session's socket closed", func() bool { return m.open() == 0 })

	m.scanned.Store(false)
	connectQR(t, s, userID)
	if status, body := request(t, s, "GET", "/session/qr", userID, ""); status != http.StatusOK {
		t.Fatalf("GET /session/qr = %d %v", status, body)
	}
}
