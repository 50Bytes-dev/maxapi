package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"

	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// storedUserWithWebhook stores gateway user userID, logged into MAX account
// testSelfID, posting events to sink under the subscription events, and
// caches it as a request would.
func storedUserWithWebhook(t *testing.T, s *server, userID, events string, sink *webhookSink) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO users (id, name, token, webhook, events, auth_token, max_user_id, connected) VALUES ($1, $1, $2, $3, $4, 'stored-auth-token', $5, 0)`,
		userID, tokenOf(userID), sink.srv.URL, events, testSelfID); err != nil {
		t.Fatalf("add user %s: %v", userID, err)
	}
}

// Paused and Resumed are the MAX account's: every gateway user of it that is
// subscribed gets them, with a session or without, whoever caused them; one
// not subscribed gets nothing (C6).
func TestPausedAndResumedReachEveryUserOfTheAccount(t *testing.T) {
	s := newTestServer(t)
	sinkA, sinkB, sinkC := newWebhookSink(t), newWebhookSink(t), newWebhookSink(t)
	storedUserWithWebhook(t, s, "shop-a", "Paused,Resumed", sinkA)
	storedUserWithWebhook(t, s, "shop-b", "All", sinkB)
	storedUserWithWebhook(t, s, "shop-c", "Sync", sinkC)

	s.pauseAccount("shop-a", testSelfID, "error.user.restricted.send")

	for name, sink := range map[string]*webhookSink{"shop-a": sinkA, "shop-b": sinkB} {
		ev := sink.waitFor(t, EventPaused)
		if ev["pauseReason"] != "spam" || ev["upstreamCode"] != "error.user.restricted.send" || ev["maxUserID"] != "1000" {
			t.Fatalf("%s got Paused %v", name, ev)
		}
	}

	if err := s.resumeAccount("shop-b", testSelfID); err != nil {
		t.Fatal(err)
	}
	for name, sink := range map[string]*webhookSink{"shop-a": sinkA, "shop-b": sinkB} {
		if ev := sink.waitFor(t, EventResumed); ev["maxUserID"] != "1000" {
			t.Fatalf("%s got Resumed %v", name, ev)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := sinkC.delivered(); len(got) != 0 {
		t.Fatalf("a user not subscribed got %v", got)
	}
}

// A message event of a user not subscribed to Message is dropped before its
// media is fetched; a subscriber's photo is fetched. Nothing of the
// message's text goes to the log (M11).
func TestMessageWorkOnlyForSubscribers(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_IPS", "true")
	logs := &lockedBuffer{}
	old := log.Logger
	log.Logger = zerolog.New(logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = old })

	photos, fetched := serving(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("jpeg")) })
	event := maxclient.Event{Type: maxclient.EventTypeMessage, Opcode: maxclient.OpNotifMessage, Payload: map[string]interface{}{
		"chatId": float64(5),
		"message": map[string]interface{}{"id": "1", "sender": float64(7), "text": "SECRET-TEXT", "type": "USER",
			"attaches": []interface{}{map[string]interface{}{"_type": "PHOTO", "baseUrl": photos.URL + "/p.jpg", "photoId": float64(9)}}},
	}}

	for _, tc := range []struct {
		events  string
		history string // the user's message history keeps texts
		fetches int32
	}{{"Sync,AuthExpired", "0", 0}, {"Sync", "10", 0}, {"Message", "0", 1}} {
		s := newTestServer(t)
		sink := newWebhookSink(t)
		userID := "sub-" + strings.ReplaceAll(tc.events, ",", "-") + "-" + tc.history
		token := tokenOf(userID)
		userinfocache.Set(token, Values{map[string]string{"Id": userID, "Token": token, "Webhook": sink.srv.URL, "Events": tc.events, "History": tc.history}}, cache.NoExpiration)
		t.Cleanup(func() { userinfocache.Delete(token) })
		mycli := &MyClient{MaxClient: maxclient.NewClient("d", zerolog.Nop()), userID: userID, token: token, db: s.db, s: s}

		before := fetched.Load()
		logs.Reset()
		mycli.handleEvent(event)
		if got := fetched.Load() - before; got != tc.fetches {
			t.Fatalf("subscribed to %s: the photo was fetched %d times, want %d", tc.events, got, tc.fetches)
		}
		parsed := strings.Contains(logs.String(), "Message received")
		if wantParsed := tc.events == "Message" || tc.history != "0"; parsed != wantParsed {
			t.Fatalf("subscribed to %s, history %s: parsed %v, want %v", tc.events, tc.history, parsed, wantParsed)
		}
		if strings.Contains(logs.String(), "SECRET-TEXT") {
			t.Fatalf("a message's text went to the log")
		}
	}
	if strings.Contains(logs.String(), "SECRET-TEXT") {
		t.Fatalf("a message's text went to the log")
	}
}

// ChatUpdate carries the chat without its last message's text (C10).
func TestChatUpdateDropsTheLastMessage(t *testing.T) {
	s := newTestServer(t)
	sink := newWebhookSink(t)
	token := tokenOf(t.Name())
	userinfocache.Set(token, Values{map[string]string{"Id": t.Name(), "Token": token, "Webhook": sink.srv.URL, "Events": "ChatUpdate"}}, cache.NoExpiration)
	t.Cleanup(func() { userinfocache.Delete(token) })
	mycli := &MyClient{userID: t.Name(), token: token, db: s.db, s: s}
	mycli.handleEvent(maxclient.Event{Type: maxclient.EventTypeChatUpdate, Opcode: maxclient.OpNotifChat, Payload: map[string]interface{}{
		"chat": map[string]interface{}{"id": float64(-5), "lastMessage": map[string]interface{}{"text": "SECRET-TEXT"}}}})
	if ev := sink.waitFor(t, "ChatUpdate"); strings.Contains(ev["event"], "SECRET-TEXT") || !strings.Contains(ev["event"], "-5") {
		t.Fatalf("ChatUpdate webhook %v", ev)
	}

	payload := map[string]interface{}{"chat": map[string]interface{}{"id": float64(-5), "title": "T",
		"lastMessage": map[string]interface{}{"text": "SECRET-TEXT"}}}
	out := withoutLastMessage(payload)
	if strings.Contains(mustJSON(t, out), "SECRET-TEXT") || out["chat"].(map[string]interface{})["title"] != "T" {
		t.Fatalf("ChatUpdate payload %v", out)
	}
	if _, kept := payload["chat"].(map[string]interface{})["lastMessage"]; !kept {
		t.Fatalf("the MAX payload itself was changed")
	}
}

// The login asks MAX for nobody's contacts, and Sync carries the account,
// its name and photo, never MAX's login answer with the chats (M11, C11).
func TestLoginAndSyncCarryNoChats(t *testing.T) {
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) == maxclient.OpLogin {
			return map[string]interface{}{
				"profile": map[string]interface{}{"contact": map[string]interface{}{"id": testSelfID, "baseUrl": "https://i.example/me.jpg",
					"names": []interface{}{map[string]interface{}{"name": "Магазин", "type": "ONEME"}}}},
				"chats": testChats(), "chatMarker": 1,
			}
		}
		return map[string]interface{}{}
	})
	c := maxclient.NewClient("d", zerolog.Nop())
	c.URI = f.uri()
	if _, err := c.ConnectAndLogin("stored-auth-token", nil); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if n := f.count(maxclient.OpContactInfo); n != 0 {
		t.Fatalf("the login asked MAX for contacts %d times", n)
	}

	sync := buildSyncPostmap(false, c)
	if sync["maxUserID"] != testSelfID || sync["name"] != "Магазин" || sync["photo"] != "https://i.example/me.jpg" {
		t.Fatalf("Sync %v", sync)
	}
	for _, key := range []string{"chats", "contacts", "profile", "chatMarker"} {
		if _, has := sync[key]; has {
			t.Fatalf("Sync carries %s", key)
		}
	}

	s := newTestServer(t)
	userID := t.Name()
	addUser(t, s, userID)
	clientManager.SetMaxClient(userID, c)
	t.Cleanup(func() { clientManager.DeleteMaxClient(userID) })
	s.storeLogin(userID, c)
	_, status := call(t, s.GetStatus(), userID, "")
	if status["name"] != "Магазин" || status["photo"] != "https://i.example/me.jpg" {
		t.Fatalf("status %v, want the account's name and photo", status)
	}
	clientManager.DeleteMaxClient(userID)
	_, status = call(t, s.GetStatus(), userID, "")
	if status["name"] != "Магазин" || status["connected"] != false {
		t.Fatalf("status without a session %v, want the name of the last login", status)
	}
}

// MAX's login.blocked erases the login with AuthExpired page_blocked;
// login.flood stops the loop at once and keeps it (C9).
func TestLoginRefusalReasons(t *testing.T) {
	shortBackoff(t)
	r := newLoginRig(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "login.blocked", "message": "Blocked"}
	})
	done := startLoop(r.s, r.userID, r.c, r.mycli, reconnectLoops["runClientLoop"])
	waitStopped(t, done)
	if ev := r.sink.waitFor(t, "AuthExpired"); ev["reason"] != ReasonPageBlocked {
		t.Fatalf("reason %q, want page_blocked", ev["reason"])
	}
	if token := storedAuthToken(t, r.s, r.userID); token != "" || r.f.count(maxclient.OpLogin) != 1 {
		t.Fatalf("token %q after %d logins, want erased after one", token, r.f.count(maxclient.OpLogin))
	}

	var logins atomic.Int32
	r = newLoginRig(t, func(fakeFrame) map[string]interface{} {
		logins.Add(1)
		return map[string]interface{}{"error": "login.flood", "message": "Too many logins"}
	})
	done = startLoop(r.s, r.userID, r.c, r.mycli, reconnectLoops["runClientLoop"])
	waitStopped(t, done)
	if ev := r.sink.waitFor(t, "AuthExpired"); ev["reason"] != ReasonRefreshFailed {
		t.Fatalf("reason %q, want refresh_failed", ev["reason"])
	}
	if logins.Load() != 1 || storedAuthToken(t, r.s, r.userID) != "stored-auth-token" {
		t.Fatalf("%d logins, token %q: want one and the token kept", logins.Load(), storedAuthToken(t, r.s, r.userID))
	}
}

// A limit on the request (a file's size, a text's length) is no stop signal;
// a limit on the account is (M9).
func TestPauseCodeMarkers(t *testing.T) {
	cases := map[string]bool{
		"error.user.restricted.send": true,
		"too.many.requests":          true,
		"error.limit.violate":        true,
		"error.spam.detected":        true,
		"file.size.limit":            false,
		"error.text.length.limit":    false,
		"chat.participants.limit":    false,
		"chat.denied":                false,
	}
	for code, want := range cases {
		_, got := pauseCode(maxclient.ParseError(map[string]interface{}{"error": code}))
		if got != want {
			t.Errorf("%s: pause %v, want %v", code, got, want)
		}
	}
}

// The logins at startup are spread: the n-th starts about n intervals in.
func TestStartupLoginsAreSpread(t *testing.T) {
	old := startupLoginInterval
	startupLoginInterval = time.Second
	t.Cleanup(func() { startupLoginInterval = old })
	for n := 0; n < 50; n++ {
		d := startupDelay(n)
		lo, hi := time.Duration(n)*time.Second-time.Second/2, time.Duration(n)*time.Second+time.Second/2
		if d < lo || d > hi {
			t.Fatalf("login %d starts at %v, want within %v..%v", n, d, lo, hi)
		}
	}
}
