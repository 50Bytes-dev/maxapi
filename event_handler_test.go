package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"

	"github.com/go-resty/resty/v2"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog"
)

// webhookSink receives a user's webhooks in either format callHook sends
// (WEBHOOK_FORMAT) and hands over the fields of each event.
type webhookSink struct {
	srv    *httptest.Server
	events chan map[string]string
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	k := &webhookSink{events: make(chan map[string]string, 64)}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := map[string]string{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for key, v := range body {
				fields[key] = fmt.Sprint(v)
			}
		} else {
			_ = r.ParseForm()
			for key := range r.PostForm {
				fields[key] = r.PostForm.Get(key)
			}
		}
		select {
		case k.events <- fields:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// waitFor returns the first delivered event of type typ.
func (k *webhookSink) waitFor(t *testing.T, typ string) map[string]string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-k.events:
			if ev["type"] == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s webhook arrived", typ)
		}
	}
}

// delivered returns the types of the events delivered so far.
func (k *webhookSink) delivered() []string {
	var types []string
	for {
		select {
		case ev := <-k.events:
			types = append(types, ev["type"])
		default:
			return types
		}
	}
}

// liveSession installs what initClient sets up for user userID: the
// MaxClient, its MyClient, the HTTP client for its webhooks, a kill channel,
// and the cached user info that points the webhooks at sink.
func liveSession(t *testing.T, s *server, userID string, c *maxclient.Client, sink *webhookSink) *MyClient {
	t.Helper()
	token := tokenOf(userID)
	userinfocache.Set(token, Values{map[string]string{
		"Id": userID, "Name": userID, "Token": token, "Webhook": sink.srv.URL, "Events": "All",
	}}, cache.NoExpiration)
	t.Cleanup(func() { userinfocache.Delete(token) })

	mycli := &MyClient{MaxClient: c, userID: userID, token: token, db: s.db, s: s}
	clientManager.SetMaxClient(userID, c)
	clientManager.SetMyClient(userID, mycli)
	clientManager.SetHTTPClient(userID, resty.New())
	clientManager.NewKillChannel(userID)
	t.Cleanup(func() { cleanupClient(userID) })
	return mycli
}

// shortBackoff turns the reconnect waits from seconds into milliseconds.
func shortBackoff(t *testing.T) {
	t.Helper()
	old := reconnectDelayUnit
	reconnectDelayUnit = time.Millisecond
	t.Cleanup(func() { reconnectDelayUnit = old })
}

// reconnectLoops are the two loops that keep a session up: runClientLoop
// after a connect, maintainConnection after /session/sync.
var reconnectLoops = map[string]func(s *server, userID string, c *maxclient.Client, mycli *MyClient){
	"runClientLoop": func(s *server, userID string, c *maxclient.Client, mycli *MyClient) {
		s.runClientLoop(userID, "stored-auth-token", c, mycli)
	},
	"maintainConnection": func(s *server, userID string, c *maxclient.Client, mycli *MyClient) {
		s.maintainConnection(userID, "stored-auth-token", "test-device", mycli.token, mycli)
	},
}

// startLoop runs loop in the background; the channel closes when it returns.
func startLoop(s *server, userID string, c *maxclient.Client, mycli *MyClient,
	loop func(*server, string, *maxclient.Client, *MyClient)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop(s, userID, c, mycli)
	}()
	return done
}

func waitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the reconnect loop did not stop")
	}
}

func storedAuthToken(t *testing.T, s *server, userID string) string {
	t.Helper()
	var token sql.NullString
	if err := s.db.Get(&token, "SELECT auth_token FROM users WHERE id=$1", userID); err != nil {
		t.Fatalf("read the stored token: %v", err)
	}
	return token.String
}

func storedConnected(t *testing.T, s *server, userID string) int {
	t.Helper()
	var connected int
	if err := s.db.Get(&connected, "SELECT connected FROM users WHERE id=$1", userID); err != nil {
		t.Fatalf("read the connected flag: %v", err)
	}
	return connected
}

// loginRig is a user whose reconnect loop logs in to a fake MAX that answers
// LOGIN with login (nil: no answer) and everything else with an empty
// payload.
type loginRig struct {
	f      *fakeMAX
	s      *server
	userID string
	sink   *webhookSink
	c      *maxclient.Client
	mycli  *MyClient
}

func newLoginRig(t *testing.T, login func(fakeFrame) map[string]interface{}) *loginRig {
	t.Helper()
	r := &loginRig{userID: t.Name()}
	r.f = newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if fr.Opcode == int(maxclient.OpLogin) {
			return login(fr)
		}
		return map[string]interface{}{}
	})
	r.s = newTestServer(t)
	addUser(t, r.s, r.userID)
	r.sink = newWebhookSink(t)
	r.c = maxclient.NewClient("test-device", zerolog.Nop())
	r.c.URI = r.f.uri()
	r.c.LoginTimeout = 50 * time.Millisecond
	r.mycli = liveSession(t, r.s, r.userID, r.c, r.sink)
	return r
}

// assertStoppedKeepingToken fails unless the loop stopped after
// maxLoginRefusals logins the way it does for refusals: AuthExpired with
// reason, the user disconnected and its clients dropped, the token kept.
func (r *loginRig) assertStoppedKeepingToken(t *testing.T, done <-chan struct{}, reason string) {
	t.Helper()
	waitStopped(t, done)
	if ev := r.sink.waitFor(t, "AuthExpired"); ev["reason"] != reason {
		t.Fatalf("AuthExpired reason = %q, want %q", ev["reason"], reason)
	}
	if n := r.f.count(maxclient.OpLogin); n != maxLoginRefusals {
		t.Fatalf("MAX received %d logins, want %d", n, maxLoginRefusals)
	}
	if token := storedAuthToken(t, r.s, r.userID); token != "stored-auth-token" {
		t.Fatalf("stored token = %q, want it kept for POST /session/connect", token)
	}
	if n := storedConnected(t, r.s, r.userID); n != 0 {
		t.Fatalf("connected = %d, want 0", n)
	}
	if r.c.IsConnected() {
		t.Fatalf("the client is still connected")
	}
	if clientManager.GetKillChannel(r.userID) != nil || clientManager.GetMaxClient(r.userID) != nil {
		t.Fatalf("the user's clients were not dropped")
	}
}

// MAX refusing the reconnect login with its own error, not an auth one,
// used to be retried forever; logging in again after MAX ended a session is
// what gets an account blocked. The third refusal in a row stops the loop
// with AuthExpired carrying MAX's code. The token stays: a partial MAX
// failure must not send every owner through a new QR login.
func TestReconnectStopsAfterThreeLoginRefusals(t *testing.T) {
	shortBackoff(t)
	for name, loop := range reconnectLoops {
		t.Run(name, func(t *testing.T) {
			r := newLoginRig(t, func(fakeFrame) map[string]interface{} {
				return map[string]interface{}{"error": "account.blocked", "message": "Account blocked"}
			})

			done := startLoop(r.s, r.userID, r.c, r.mycli, loop)

			r.assertStoppedKeepingToken(t, done, "account.blocked")
		})
	}
}

// MAX may refuse a login by never answering it, once the connection and
// SessionInit went through: three such logins in a row stop the loop too.
func TestReconnectStopsAfterThreeLoginTimeouts(t *testing.T) {
	shortBackoff(t)
	for name, loop := range reconnectLoops {
		t.Run(name, func(t *testing.T) {
			r := newLoginRig(t, func(fakeFrame) map[string]interface{} { return nil })

			done := startLoop(r.s, r.userID, r.c, r.mycli, loop)

			r.assertStoppedKeepingToken(t, done, "login timed out")
		})
	}
}

// An auth error says the token is dead: the loop stops at once and erases
// it, as before.
func TestReconnectAuthErrorErasesTheToken(t *testing.T) {
	shortBackoff(t)
	r := newLoginRig(t, func(fakeFrame) map[string]interface{} {
		return map[string]interface{}{"error": "login.token", "message": "Token is invalid"}
	})

	done := startLoop(r.s, r.userID, r.c, r.mycli, reconnectLoops["runClientLoop"])

	waitStopped(t, done)
	if ev := r.sink.waitFor(t, "AuthExpired"); !strings.Contains(ev["reason"], "login.token") {
		t.Fatalf("AuthExpired reason = %q, want the auth error", ev["reason"])
	}
	if n := r.f.count(maxclient.OpLogin); n != 1 {
		t.Fatalf("MAX received %d logins, want 1", n)
	}
	if token := storedAuthToken(t, r.s, r.userID); token != "" {
		t.Fatalf("the stored token survived an auth error: %q", token)
	}
}

// What counts as MAX refusing a login: its own error code at any stage, and
// LOGIN unanswered or losing its socket after SessionInit went through. A
// network failure before LOGIN does not count.
func TestLoginRefusalClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		refused bool
		reason  string
	}{
		{"MAX code on LOGIN", &maxclient.LoginError{Err: maxclient.ParseError(map[string]interface{}{"error": "account.blocked"})}, true, "account.blocked"},
		{"MAX code on SessionInit", maxclient.ParseError(map[string]interface{}{"error": "device.banned"}), true, "device.banned"},
		{"LOGIN unanswered", &maxclient.LoginError{Err: maxclient.ErrTimeout}, true, "login timed out"},
		{"LOGIN lost its socket", &maxclient.LoginError{Err: maxclient.ErrNotConnected}, true, "login connection lost"},
		{"SessionInit unanswered", maxclient.ErrTimeout, false, ""},
		{"not connected before LOGIN", maxclient.ErrNotConnected, false, ""},
		{"dial failed", fmt.Errorf("dial tcp: connection refused"), false, ""},
		{"LOGIN frame not written", &maxclient.LoginError{Err: fmt.Errorf("write: broken pipe")}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, refused := loginRefusal(tc.err)
			if refused != tc.refused || reason != tc.reason {
				t.Fatalf("loginRefusal = %q, %v; want %q, %v", reason, refused, tc.reason, tc.refused)
			}
		})
	}
}

// Network failures say nothing about the token: the loop keeps retrying
// them, past the refusal limit, until it is told to stop.
func TestReconnectKeepsRetryingNetworkFailures(t *testing.T) {
	shortBackoff(t)
	for name, loop := range reconnectLoops {
		t.Run(name, func(t *testing.T) {
			var dials atomic.Int32
			down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dials.Add(1)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(down.Close)
			s := newTestServer(t)
			userID := t.Name()
			addUser(t, s, userID)
			sink := newWebhookSink(t)
			c := maxclient.NewClient("test-device", zerolog.Nop())
			c.URI = "ws" + strings.TrimPrefix(down.URL, "http")
			mycli := liveSession(t, s, userID, c, sink)

			done := startLoop(s, userID, c, mycli, loop)

			deadline := time.Now().Add(5 * time.Second)
			for dials.Load() < 2*maxLoginRefusals {
				if time.Now().After(deadline) {
					t.Fatalf("only %d reconnect attempts in 5 s", dials.Load())
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case <-done:
				t.Fatalf("the loop stopped on network failures")
			default:
			}
			if token := storedAuthToken(t, s, userID); token != "stored-auth-token" {
				t.Fatalf("stored token = %q after network failures, want it kept", token)
			}
			for _, typ := range sink.delivered() {
				if typ == "AuthExpired" {
					t.Fatalf("network failures sent AuthExpired")
				}
			}

			clientManager.SendKill(userID)
			waitStopped(t, done)
		})
	}
}

// The reconnect waits are spread by ±20% around the doubling backoff.
func TestReconnectBackoffIsSpread(t *testing.T) {
	base := time.Duration(*connectRetryDelay) * reconnectDelayUnit
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := reconnectBackoff(1)
		if d < base*8/10 || d > base*12/10 {
			t.Fatalf("backoff %v outside ±20%% of %v", d, base)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("backoff takes only %d values, want it spread", len(seen))
	}
}

// LoggedOut from MAX deletes the user. The kill signal used to go out after
// cleanupClient had dropped the channel, so the loop went on with the
// deleted user's token; and the LoggedOut webhook, sent right before the
// cleanup, found the HTTP client gone and was dropped.
func TestLoggedOutStopsTheLoopAndReachesTheWebhook(t *testing.T) {
	for name, loop := range reconnectLoops {
		t.Run(name, func(t *testing.T) {
			f := newFakeMAX(t, emptyReply)
			s := newTestServer(t)
			userID := t.Name()
			addUser(t, s, userID)
			sink := newWebhookSink(t)
			c := maxclient.NewClient("test-device", zerolog.Nop())
			c.URI = f.uri()
			if err := c.Connect(); err != nil {
				t.Fatalf("connect to fake MAX: %v", err)
			}
			t.Cleanup(func() { c.Close() })
			mycli := liveSession(t, s, userID, c, sink)
			done := startLoop(s, userID, c, mycli, loop)

			mycli.handleEvent(maxclient.Event{Type: "LoggedOut", Opcode: maxclient.OpLogout, Payload: map[string]interface{}{}})

			waitStopped(t, done)
			if c.IsConnected() {
				t.Fatalf("the MaxClient of the deleted user is still connected")
			}
			sink.waitFor(t, "LoggedOut")
			if _, found := userinfocache.Get(tokenOf(userID)); found {
				t.Fatalf("the deleted user is still in userinfocache")
			}
			var exists bool
			if err := s.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)", userID); err != nil || exists {
				t.Fatalf("user row exists = %v (%v), want deleted", exists, err)
			}
		})
	}
}

// POST /session/logout logs out of MAX and deletes the user; the reconnect
// loop must stop instead of logging in again.
func TestLogoutStopsTheLoop(t *testing.T) {
	f := newFakeMAX(t, emptyReply)
	s := newTestServer(t)
	userID := t.Name()
	addUser(t, s, userID)
	sink := newWebhookSink(t)
	c := maxclient.NewClient("test-device", zerolog.Nop())
	c.URI = f.uri()
	if err := c.Connect(); err != nil {
		t.Fatalf("connect to fake MAX: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	mycli := liveSession(t, s, userID, c, sink)
	done := startLoop(s, userID, c, mycli, reconnectLoops["runClientLoop"])

	if status, body := call(t, s.Logout(), userID, ""); status != http.StatusOK {
		t.Fatalf("logout: %d %v", status, body)
	}

	waitStopped(t, done)
	if n := f.count(maxclient.OpLogout); n != 1 {
		t.Fatalf("MAX received %d logouts, want 1", n)
	}
	if n := f.count(maxclient.OpLogin); n != 0 {
		t.Fatalf("the loop logged in %d times after the logout", n)
	}
}
