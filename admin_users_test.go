package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// adminTestToken is the admin token of the routed test server.
const adminTestToken = "admin-test-token"

// routedServer returns a test server behind the gateway's real router, so a
// request goes through the token lookup and its cache.
func routedServer(t *testing.T) *server {
	t.Helper()
	s := newTestServer(t)
	s.router = mux.NewRouter()
	s.routes()
	old := *adminToken
	*adminToken = adminTestToken
	t.Cleanup(func() { *adminToken = old })
	return s
}

// serve sends one request through s's router and returns the status.
func serve(s *server, method, path, header, value, body string) int {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if header != "" {
		r.Header.Set(header, value)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	return w.Code
}

func TestDeletedUserTokenStopsWorkingAtOnce(t *testing.T) {
	s := routedServer(t)
	addUser(t, s, "deleted-user")
	token := tokenOf("deleted-user")

	// The first call puts the user's info into the token cache.
	if got := serve(s, "GET", "/session/status", "token", token, ""); got != http.StatusOK {
		t.Fatalf("status before the delete = %d, want 200", got)
	}
	if got := serve(s, "DELETE", "/admin/users/deleted-user", "Authorization", adminTestToken, ""); got != http.StatusOK {
		t.Fatalf("delete = %d, want 200", got)
	}
	if got := serve(s, "GET", "/session/status", "token", token, ""); got != http.StatusUnauthorized {
		t.Fatalf("status after the delete = %d, want 401: the deleted token still lets in", got)
	}
	if _, found := userinfocache.Get(token); found {
		t.Fatal("the deleted user's info is still cached")
	}
}

func TestDeletingUnknownUserSucceeds(t *testing.T) {
	s := routedServer(t)
	if got := serve(s, "DELETE", "/admin/users/nobody", "Authorization", adminTestToken, ""); got != http.StatusOK {
		t.Fatalf("delete of an unknown user = %d, want 200", got)
	}
}

func TestEditedUserTakesEffectWithoutRestart(t *testing.T) {
	s := routedServer(t)
	addUser(t, s, "edited-user")
	token := tokenOf("edited-user")
	t.Cleanup(func() { userinfocache.Delete(token) })

	if got := serve(s, "GET", "/session/status", "token", token, ""); got != http.StatusOK {
		t.Fatalf("status before the edit = %d, want 200", got)
	}
	body := `{"name":"renamed","webhook":"https://hooks.example/new","events":"Sync,QRAuthorized"}`
	if got := serve(s, "PUT", "/admin/users/edited-user", "Authorization", adminTestToken, body); got != http.StatusOK {
		t.Fatalf("edit = %d, want 200", got)
	}

	if got := getUserWebhookUrl(token); got != "https://hooks.example/new" {
		t.Fatalf("webhook events go to %q, want the edited webhook", got)
	}
	cached, found := userinfocache.Get(token)
	if !found {
		t.Fatal("the edited user's info left the cache: its events would find no webhook")
	}
	v := cached.(Values)
	if v.Get("Events") != "Sync,QRAuthorized" || v.Get("Name") != "renamed" {
		t.Fatalf("cached events %q, name %q: want the edited ones", v.Get("Events"), v.Get("Name"))
	}
	if v.Get("Id") != "edited-user" || v.Get("Token") != token {
		t.Fatalf("the edit lost the cached id or token: %v", v.m)
	}
}
