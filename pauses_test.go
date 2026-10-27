package main

import (
	"database/sql"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"maxapi/maxclient"
)

const sendByChat = `{"chatId": 5, "text": "hello"}`

// refusingMAX answers frames with opcode op with MAX's error code while
// refusing is set, and everything else as a healthy MAX.
func refusingMAX(op maxclient.Opcode, code string, refusing *atomic.Bool) func(fakeFrame) map[string]interface{} {
	return func(fr fakeFrame) map[string]interface{} {
		switch {
		case fr.Opcode == int(op) && refusing.Load():
			return map[string]interface{}{"error": code, "message": "Refused", "title": "Refused"}
		case fr.Opcode == int(maxclient.OpContactInfoByPhone):
			return contactReply(fr)
		}
		return sentReply(fr)
	}
}

func refusing() *atomic.Bool {
	b := new(atomic.Bool)
	b.Store(true)
	return b
}

// assertPaused fails unless the answer reports a spam pause with MAX code
// upstream (nil: no upstreamCode).
func assertPaused(t *testing.T, body map[string]interface{}, upstream interface{}) {
	t.Helper()
	if body["paused"] != true || body["pauseReason"] != "spam" || body["upstreamCode"] != upstream {
		t.Fatalf("answer %v, want paused for spam with upstreamCode %v", body, upstream)
	}
}

// storedPause returns the MAX code of the stored pause of MAX account
// maxUserID.
func storedPause(t *testing.T, s *server, maxUserID int64) (string, bool) {
	t.Helper()
	var code string
	err := s.db.Get(&code, "SELECT upstream_code FROM account_pauses WHERE max_user_id=$1", maxUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the stored pause: %v", err)
	}
	return code, true
}

// pauseUser has MAX refuse a send of userID with a restriction.
func pauseUser(t *testing.T, s *server, userID string) {
	t.Helper()
	status, body := call(t, s.SendMessage(), userID, sendByChat)
	wantError(t, status, body, http.StatusUnprocessableEntity, "send_rejected")
	assertPaused(t, body, "error.user.restricted.send")
}

// assertRefusedPaused sends as userID and fails unless the send is refused
// for the pause without a MAX call.
func assertRefusedPaused(t *testing.T, s *server, f *fakeMAX, userID, body string) {
	t.Helper()
	frames := f.total()
	status, got := call(t, s.SendMessage(), userID, body)
	wantError(t, status, got, http.StatusUnprocessableEntity, "send_rejected")
	assertPaused(t, got, nil)
	if n := f.total() - frames; n != 0 {
		t.Fatalf("a paused account sent %d frames to MAX", n)
	}
}

func connectAs(t *testing.T, s *server, userID string) (int, map[string]interface{}) {
	t.Helper()
	t.Cleanup(func() { userinfocache.Delete(tokenOf(userID)) })
	return call(t, s.Connect(), userID, `{"subscribe": ["Sync"], "immediate": true}`)
}

// A MAX error naming a limit or a restriction, on the lookup or on the send,
// pauses the account: the answer keeps its stage's code and adds the pause,
// the pause is stored by the MAX account, and the account sends nothing more.
func TestLimitErrorPausesTheAccount(t *testing.T) {
	byPhone := `{"phone": "` + testPhone + `", "text": "hello"}`
	cases := []struct {
		name, code string
		op         maxclient.Opcode
		body       string
		status     int
		outcome    string
	}{
		{"lookup limit", "error.limit.violate", maxclient.OpContactInfoByPhone, byPhone, http.StatusServiceUnavailable, "lookup_failed"},
		{"lookup flood, any case", "Error.FLOOD", maxclient.OpContactInfoByPhone, byPhone, http.StatusServiceUnavailable, "lookup_failed"},
		{"send restricted", "error.user.restricted.send", maxclient.OpMsgSend, sendByChat, http.StatusUnprocessableEntity, "send_rejected"},
		{"send too many", "too.many.requests", maxclient.OpMsgSend, sendByChat, http.StatusUnprocessableEntity, "send_rejected"},
		{"send spam", "error.spam.detected", maxclient.OpMsgSend, byPhone, http.StatusUnprocessableEntity, "send_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMAX(t, refusingMAX(tc.op, tc.code, refusing()))
			s := newTestServer(t)
			userID := t.Name()
			sessionUser(t, s, f, userID)

			status, body := call(t, s.SendMessage(), userID, tc.body)

			wantError(t, status, body, tc.status, tc.outcome)
			assertPaused(t, body, tc.code)
			if code, ok := storedPause(t, s, testSelfID); !ok || code != tc.code {
				t.Fatalf("stored pause = %q (%v), want %q", code, ok, tc.code)
			}
			assertRefusedPaused(t, s, f, userID, sendByChat)
			assertRefusedPaused(t, s, f, userID, byPhone)
		})
	}
}

// Other MAX refusals leave the account alone.
func TestOtherMAXErrorsDoNotPause(t *testing.T) {
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "chat.denied", refusing()))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	for i := 1; i <= 2; i++ {
		status, body := call(t, s.SendMessage(), userID, sendByChat)
		wantError(t, status, body, http.StatusUnprocessableEntity, "send_rejected")
		if body["paused"] != nil {
			t.Fatalf("send %d: %v, want no pause", i, body)
		}
	}
	if n := f.count(maxclient.OpMsgSend); n != 2 {
		t.Fatalf("MAX received %d sends, want 2", n)
	}
	if _, ok := storedPause(t, s, testSelfID); ok {
		t.Fatalf("an ordinary refusal stored a pause")
	}
}

// The status and the connect of a connected account report the pause, and
// the session stays connected.
func TestStatusAndConnectReportThePause(t *testing.T) {
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refusing()))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	status, body := call(t, s.GetStatus(), userID, "")
	if status != http.StatusOK || body["connected"] != true || body["paused"] != false || body["pauseReason"] != nil {
		t.Fatalf("status before the pause: %d %v, want connected and paused:false", status, body)
	}
	status, body = connectAs(t, s, userID)
	if status != http.StatusOK || body["alreadyConnected"] != true || body["paused"] != false {
		t.Fatalf("connect before the pause: %d %v, want alreadyConnected and paused:false", status, body)
	}

	pauseUser(t, s, userID)

	status, body = call(t, s.GetStatus(), userID, "")
	if status != http.StatusOK || body["connected"] != true || body["loggedIn"] != true {
		t.Fatalf("status of the paused account: %d %v, want connected", status, body)
	}
	assertPaused(t, body, "error.user.restricted.send")
	status, body = connectAs(t, s, userID)
	if status != http.StatusOK || body["alreadyConnected"] != true {
		t.Fatalf("connect of the paused account: %d %v, want alreadyConnected", status, body)
	}
	assertPaused(t, body, "error.user.restricted.send")
}

// The pause belongs to the MAX account: every gateway user logged into it is
// refused without a MAX call and reports it, a resume through any of them
// lifts it for all, and a user of another MAX account is not affected.
func TestPauseCoversEveryGatewayUserOfTheAccount(t *testing.T) {
	refuse := refusing()
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refuse))
	s := newTestServer(t)
	shopA, shopB, other := t.Name()+"-a", t.Name()+"-b", t.Name()+"-other"
	sessionUser(t, s, f, shopA)
	sessionUser(t, s, f, shopB)
	sessionUser(t, s, f, other).MaxUserID = testSelfID + 1
	if _, err := s.db.Exec("UPDATE users SET max_user_id=$1 WHERE id=$2", testSelfID+1, other); err != nil {
		t.Fatalf("move %s to another MAX account: %v", other, err)
	}

	pauseUser(t, s, shopA)
	refuse.Store(false)

	assertRefusedPaused(t, s, f, shopB, sendByChat)
	_, body := call(t, s.GetStatus(), shopB, "")
	assertPaused(t, body, "error.user.restricted.send")
	if status, body := call(t, s.SendMessage(), other, sendByChat); status != http.StatusOK {
		t.Fatalf("send from another MAX account: %d %v", status, body)
	}

	status, body := call(t, s.Resume(), shopB, `{}`)
	if status != http.StatusOK || body["maxUserID"] != float64(testSelfID) {
		t.Fatalf("resume: %d %v, want 200 with maxUserID %d", status, body, testSelfID)
	}
	for _, userID := range []string{shopA, shopB} {
		if status, body := call(t, s.SendMessage(), userID, sendByChat); status != http.StatusOK {
			t.Fatalf("send as %s after resume: %d %v", userID, status, body)
		}
	}
}

// The pause outlives the gateway user: a LoggedOut deletes it, and the owner
// logs in again through a new gateway user into the same, still restricted
// MAX account. That one is paused from the start, without a session (the
// status reads the stored MAX account), after a restart, and on connect.
func TestPauseOutlivesTheUser(t *testing.T) {
	dir := t.TempDir()
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refusing()))
	before := newTestServerAt(t, dir)
	oldUser, newUser := t.Name()+"-old", t.Name()+"-new"
	sessionUser(t, before, f, oldUser)
	pauseUser(t, before, oldUser)

	before.safeDeleteUser(oldUser, false)

	if _, ok := storedPause(t, before, testSelfID); !ok {
		t.Fatalf("the stored pause went with the gateway user")
	}
	addUser(t, before, newUser)
	status, body := call(t, before.GetStatus(), newUser, "")
	if status != http.StatusOK || body["connected"] != false {
		t.Fatalf("status of the new user without a session: %d %v, want connected:false", status, body)
	}
	assertPaused(t, body, "error.user.restricted.send")

	after := newTestServerAt(t, dir)
	startSession(t, f, newUser)

	assertRefusedPaused(t, after, f, newUser, sendByChat)
	_, body = connectAs(t, after, newUser)
	assertPaused(t, body, "error.user.restricted.send")
}

// The pause survives a restart: the restarted gateway refuses the sends and
// reports it, and a resume there lifts the stored pause for good.
func TestPauseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refusing()))
	before := newTestServerAt(t, dir)
	userID := t.Name()
	sessionUser(t, before, f, userID)
	pauseUser(t, before, userID)

	after := newTestServerAt(t, dir)

	assertRefusedPaused(t, after, f, userID, sendByChat)
	_, body := call(t, after.GetStatus(), userID, "")
	assertPaused(t, body, "error.user.restricted.send")

	if status, body := call(t, after.Resume(), userID, `{}`); status != http.StatusOK {
		t.Fatalf("resume: %d %v", status, body)
	}
	if again := newTestServerAt(t, dir); again.pauseOf(testSelfID) != nil {
		t.Fatalf("the pause came back after a resume and another restart")
	}
}

// Resume lifts the pause: the stored one and the one in memory, and the
// account sends again.
func TestResumeLiftsThePause(t *testing.T) {
	refuse := refusing()
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refuse))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)
	pauseUser(t, s, userID)
	refuse.Store(false)

	status, body := call(t, s.Resume(), userID, "")

	if status != http.StatusOK || body["success"] != true || body["maxUserID"] != float64(testSelfID) {
		t.Fatalf("resume: %d %v, want 200 with maxUserID %d", status, body, testSelfID)
	}
	if _, ok := storedPause(t, s, testSelfID); ok {
		t.Fatalf("the stored pause outlived the resume")
	}
	if status, body := call(t, s.SendMessage(), userID, sendByChat); status != http.StatusOK {
		t.Fatalf("send after resume: %d %v", status, body)
	}
	_, body = call(t, s.GetStatus(), userID, "")
	if body["paused"] != false || body["pauseReason"] != nil || body["upstreamCode"] != nil {
		t.Fatalf("status after resume: %v, want paused:false without its fields", body)
	}
}

// Resuming an account that is not paused answers 200 and changes nothing,
// whatever the optional body; a body that is not JSON is refused.
func TestResumeWithoutPause(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(contactReply))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	for _, body := range []string{"", `{}`, `{"captchaKey": "x7kp2"}`} {
		status, got := call(t, s.Resume(), userID, body)
		if status != http.StatusOK || got["maxUserID"] != float64(testSelfID) {
			t.Fatalf("resume with %q: %d %v, want 200 with maxUserID", body, status, got)
		}
	}
	status, got := call(t, s.Resume(), userID, `{`)
	wantError(t, status, got, http.StatusBadRequest, "INVALID_INPUT")

	if status, got := call(t, s.SendMessage(), userID, sendByChat); status != http.StatusOK {
		t.Fatalf("send: %d %v", status, got)
	}
}

// The stored pause is deleted first: if that fails, the account stays paused.
func TestResumeKeepsThePauseWhenTheDeleteFails(t *testing.T) {
	f := newFakeMAX(t, refusingMAX(maxclient.OpMsgSend, "error.user.restricted.send", refusing()))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)
	pauseUser(t, s, userID)
	if _, err := s.db.Exec(`CREATE TRIGGER keep_pause BEFORE DELETE ON account_pauses
		BEGIN SELECT RAISE(FAIL, 'disk I/O error'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	status, body := call(t, s.Resume(), userID, `{}`)

	wantError(t, status, body, http.StatusInternalServerError, "INTERNAL_ERROR")
	assertRefusedPaused(t, s, f, userID, sendByChat)
	_, body = call(t, s.GetStatus(), userID, "")
	assertPaused(t, body, "error.user.restricted.send")
}
