package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"
)

// lookupThenSend answers lookups with lookup and sends as a healthy MAX.
func lookupThenSend(lookup func(fakeFrame) map[string]interface{}) func(fakeFrame) map[string]interface{} {
	return func(fr fakeFrame) map[string]interface{} {
		if fr.Opcode == int(maxclient.OpContactInfoByPhone) {
			return lookup(fr)
		}
		return sentReply(fr)
	}
}

func sendByPhone(t *testing.T, s *server, userID, phone string) (int, map[string]interface{}) {
	t.Helper()
	return call(t, s.SendMessage(), userID, `{"phone": "`+phone+`", "text": "hello"}`)
}

// ageLookups moves the cached lookups of userID back by d, as if MAX had
// answered them d earlier.
func ageLookups(t *testing.T, s *server, userID string, d time.Duration) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE phone_lookups SET looked_up_at = looked_up_at - $1 WHERE user_id=$2",
		int64(d/time.Second), userID); err != nil {
		t.Fatalf("age lookups: %v", err)
	}
}

func cachedLookups(t *testing.T, s *server, userID string) int {
	t.Helper()
	var n int
	if err := s.db.Get(&n, "SELECT COUNT(*) FROM phone_lookups WHERE user_id=$1", userID); err != nil {
		t.Fatalf("count cached lookups: %v", err)
	}
	return n
}

// A found contact is looked up once and trusted for 90 days: numbers are
// reissued, so after that it is looked up again.
func TestSendByPhoneLooksUpFoundContactFor90Days(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(contactReply))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)
	wantChat := float64(maxclient.GetDialogID(testSelfID, testContactID))

	steps := []struct {
		age     time.Duration // added to the cached answer's age before the send
		lookups int           // lookups MAX has received after the send
	}{
		{0, 1},
		{0, 1},
		{89 * 24 * time.Hour, 1},
		{2 * 24 * time.Hour, 2}, // 91 days old
		{0, 2},                  // the new answer is cached again
	}
	for i, step := range steps {
		ageLookups(t, s, userID, step.age)
		status, body := sendByPhone(t, s, userID, testPhone)
		if status != http.StatusOK || body["chatId"] != wantChat {
			t.Fatalf("step %d: %d %v, want 200 to chatId %v", i, status, body, wantChat)
		}
		if n := f.count(maxclient.OpContactInfoByPhone); n != step.lookups {
			t.Fatalf("step %d: MAX received %d lookups, want %d", i, n, step.lookups)
		}
	}
	if n := f.count(maxclient.OpMsgSend); n != len(steps) {
		t.Fatalf("MAX received %d sends, want %d", n, len(steps))
	}
}

// A number MAX answered not.found for answers 404 from the cache, without
// asking MAX again: retail looked such numbers up again and again
// (2026-10-27).
func TestSendByPhoneRemembersUnknownNumber(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(notFoundReply))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	for i := 1; i <= 2; i++ {
		status, body := sendByPhone(t, s, userID, testPhone)
		wantError(t, status, body, http.StatusNotFound, "address_not_found")
	}

	if n := f.count(maxclient.OpContactInfoByPhone); n != 1 {
		t.Fatalf("MAX received %d lookups, want 1", n)
	}
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d sends, want 0", n)
	}
}

// Only MAX's explicit not.found is cached. An answer without a usable
// contact still answers 404, but the next send asks MAX again.
func TestAnswerWithoutContactIsNotCached(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(emptyReply))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	for i := 1; i <= 2; i++ {
		status, body := sendByPhone(t, s, userID, testPhone)
		wantError(t, status, body, http.StatusNotFound, "address_not_found")
		if n := f.count(maxclient.OpContactInfoByPhone); n != i {
			t.Fatalf("send %d: MAX received %d lookups, want %d", i, n, i)
		}
	}
	if n := cachedLookups(t, s, userID); n != 0 {
		t.Fatalf("%d lookups cached, want none", n)
	}
}

// MAX refusing a send to a cached id drops it: the id may be stale, and the
// next send looks the number up again. A refusal of a send to a freshly
// looked-up id keeps it, and so does a refusal that pauses the account.
func TestRejectedSendToCachedContactDropsIt(t *testing.T) {
	var refusal atomic.Value // MAX's error code for sends, "" to accept them
	refusal.Store("")
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		code := refusal.Load().(string)
		switch {
		case fr.Opcode == int(maxclient.OpContactInfoByPhone):
			return contactReply(fr)
		case code != "":
			return map[string]interface{}{"error": code, "message": "Refused"}
		}
		return sentReply(fr)
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	steps := []struct {
		refusal string
		status  int
		cached  int // cached lookups after the send
		lookups int // lookups MAX has received after the send
	}{
		{"chat.denied", http.StatusUnprocessableEntity, 1, 1}, // fresh lookup: kept
		{"chat.denied", http.StatusUnprocessableEntity, 0, 1}, // cached id: dropped
		{"", http.StatusOK, 1, 2},                             // looked up again
		{"too.many.requests", http.StatusUnprocessableEntity, 1, 2},
	}
	for i, step := range steps {
		refusal.Store(step.refusal)
		status, body := sendByPhone(t, s, userID, testPhone)
		if status != step.status {
			t.Fatalf("step %d: %d %v, want %d", i, status, body, step.status)
		}
		if n := cachedLookups(t, s, userID); n != step.cached {
			t.Fatalf("step %d: %d lookups cached, want %d", i, n, step.cached)
		}
		if n := f.count(maxclient.OpContactInfoByPhone); n != step.lookups {
			t.Fatalf("step %d: MAX received %d lookups, want %d", i, n, step.lookups)
		}
	}
}

// "Not found" is trusted for 30 days, then the number is looked up again.
func TestUnknownNumberIsLookedUpAgainAfter30Days(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(notFoundReply))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	steps := []struct {
		age     time.Duration // added to the cached answer's age before the send
		lookups int           // lookups MAX has received after the send
	}{
		{0, 1},
		{29 * 24 * time.Hour, 1},
		{2 * 24 * time.Hour, 2}, // 31 days old
		{0, 2},                  // the new answer is cached again
	}
	for i, step := range steps {
		ageLookups(t, s, userID, step.age)
		status, body := sendByPhone(t, s, userID, testPhone)
		wantError(t, status, body, http.StatusNotFound, "address_not_found")
		if n := f.count(maxclient.OpContactInfoByPhone); n != step.lookups {
			t.Fatalf("step %d: MAX received %d lookups, want %d", i, n, step.lookups)
		}
	}
}

// The cache belongs to the gateway user: another one asks MAX itself.
func TestLookupCacheIsPerAccount(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(contactReply))
	s := newTestServer(t)
	first, second := t.Name()+"-a", t.Name()+"-b"
	sessionUser(t, s, f, first)
	sessionUser(t, s, f, second)

	for _, userID := range []string{first, second} {
		if status, body := sendByPhone(t, s, userID, testPhone); status != http.StatusOK {
			t.Fatalf("send as %s: %d %v", userID, status, body)
		}
	}

	if n := f.count(maxclient.OpContactInfoByPhone); n != 2 {
		t.Fatalf("MAX received %d lookups, want one per account", n)
	}
}

// Only what MAX said about the number is cached: neither a malformed number,
// which never reaches MAX, nor a failed lookup.
func TestLookupCacheKeepsOnlyMAXAnswers(t *testing.T) {
	f := newFakeMAX(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "service.unavailable", "message": "Try later"}
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	status, body := sendByPhone(t, s, userID, "12-34")
	wantError(t, status, body, http.StatusNotFound, "address_not_found")
	status, body = sendByPhone(t, s, userID, testPhone)
	wantError(t, status, body, http.StatusServiceUnavailable, "lookup_failed")

	if n := cachedLookups(t, s, userID); n != 0 {
		t.Fatalf("%d lookups cached, want none", n)
	}
}

// The cached lookups go with the gateway user.
func TestLookupCacheGoesWithTheUser(t *testing.T) {
	f := newFakeMAX(t, lookupThenSend(func(fr fakeFrame) map[string]interface{} {
		if fr.Payload["phone"] == testPhone {
			return contactReply(fr)
		}
		return notFoundReply(fr)
	}))
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)
	sendByPhone(t, s, userID, testPhone)
	sendByPhone(t, s, userID, "79000000002")
	if n := cachedLookups(t, s, userID); n != 2 {
		t.Fatalf("%d lookups cached, want 2", n)
	}

	s.safeDeleteUser(userID, false)

	if n := cachedLookups(t, s, userID); n != 0 {
		t.Fatalf("%d lookups left after the user was deleted", n)
	}
}
