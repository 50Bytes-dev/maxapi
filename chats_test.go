package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"
)

// callURL runs a handler as the given user with method and target (path
// and query) and decodes the JSON body.
func callURL(t *testing.T, h http.HandlerFunc, userID, method, target, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), "userinfo", Values{map[string]string{"Id": userID, "Token": tokenOf(userID)}})
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

const (
	testPartnerID int64 = 3003 // the person of the dialog in testChats
	testOtherID   int64 = 4004
	testChatID    int64 = -70000000000000
	testChannelID int64 = -70000000000001
)

func me(id int64) string { return strconv.FormatInt(id, 10) }

// testChats is a page of the account's chats as MAX sends it, one of each
// case of the write rule.
func testChats() []interface{} {
	last := map[string]interface{}{"id": "116448890093701000", "sender": float64(testPartnerID), "text": "SECRET-TEXT", "time": float64(1776869050502), "type": "USER"}
	return []interface{}{
		map[string]interface{}{"id": 0, "type": "DIALOG", "status": "ACTIVE", "participants": map[string]interface{}{me(testSelfID): 1}},
		map[string]interface{}{"id": testSelfID ^ testPartnerID, "type": "DIALOG", "status": "ACTIVE", "owner": testSelfID,
			"participants": map[string]interface{}{me(testSelfID): 1, me(testPartnerID): 2}, "lastMessage": last, "newMessages": 3, "lastEventTime": 1776869050502},
		map[string]interface{}{"id": testChatID, "type": "CHAT", "status": "ACTIVE", "title": "Покупатели", "owner": testOtherID,
			"participantsCount": 17, "baseIconUrl": "https://i.example/c.jpg",
			"participants": map[string]interface{}{me(testSelfID): 1, me(testOtherID): 2}, "lastMessage": last, "lastEventTime": 1776860000000},
		map[string]interface{}{"id": testChannelID, "type": "CHANNEL", "status": "ACTIVE", "title": "Канал", "owner": testOtherID,
			"participants":      map[string]interface{}{me(testSelfID): 1, me(testOtherID): 2},
			"adminParticipants": map[string]interface{}{me(testSelfID): map[string]interface{}{"id": testSelfID, "permissions": 32}}, "lastEventTime": 1776850000000},
		map[string]interface{}{"id": testSelfID ^ 5005, "type": "DIALOG", "status": "ACTIVE", "hasBots": true, "restrictions": 8128,
			"options": map[string]interface{}{"SERVICE_CHAT": true}, "participants": map[string]interface{}{me(testSelfID): 1, "5005": 2}, "lastEventTime": 1776840000000},
		map[string]interface{}{"id": int64(-70000000000002), "type": "CHAT", "status": "LEFT", "title": "Старая", "participants": map[string]interface{}{}, "lastEventTime": 1776830000000},
		map[string]interface{}{"id": int64(-70000000000003), "type": "CHAT", "status": "ACTIVE", "title": "Чужая", "participants": map[string]interface{}{me(testOtherID): 1}, "lastEventTime": 1776820000000},
	}
}

// chatsMAX answers the chat list with testChats and marker 1776820000000,
// the contacts with the dialog's person, and records the rest.
func chatsMAX(t *testing.T) *fakeMAX {
	return newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		switch maxclient.Opcode(fr.Opcode) {
		case maxclient.OpChatsList:
			return map[string]interface{}{"chats": testChats(), "marker": 1776820000000}
		case maxclient.OpContactInfo:
			return map[string]interface{}{"contacts": []interface{}{
				map[string]interface{}{"id": testPartnerID, "accountStatus": 0, "names": []interface{}{map[string]interface{}{"name": "Иван", "type": "ONEME"}}, "baseUrl": "https://i.example/u.jpg", "phone": 79001234567},
				map[string]interface{}{"id": 5005, "accountStatus": 0, "names": []interface{}{map[string]interface{}{"name": "MAX", "type": "ONEME"}}},
			}}
		}
		return map[string]interface{}{}
	})
}

func rowsByID(t *testing.T, body map[string]interface{}) map[int64]map[string]interface{} {
	t.Helper()
	items, ok := body["items"].([]interface{})
	if !ok {
		t.Fatalf("no items in %v", body)
	}
	out := map[int64]map[string]interface{}{}
	for _, it := range items {
		row := it.(map[string]interface{})
		out[int64(row["chatId"].(float64))] = row
	}
	return out
}

// GET /chat/list gives each chat as a row: its kind, the dialog's person,
// the title, whether the account may write there and why not, by the MAX web
// client's rules. No message's text comes without include=lastMessage; the
// account's own Favorites (chat 0) is no address and is left out.
func TestChatListRows(t *testing.T) {
	f := chatsMAX(t)
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	before := time.Now()
	status, body := callURL(t, s.ListChats(), userID, "GET", "/chat/list", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	if strings.Contains(mustJSON(t, body), "SECRET-TEXT") {
		t.Fatalf("a message's text is in the list without include=lastMessage: %v", body)
	}
	if body["nextMarker"] != "1776820000000" {
		t.Fatalf("nextMarker = %v, want MAX's marker as a string", body["nextMarker"])
	}
	var asked struct {
		Marker int64 `json:"marker"`
	}
	_ = json.Unmarshal(f.lastRaw(maxclient.OpChatsList), &asked)
	if asked.Marker <= before.UnixMilli() {
		t.Fatalf("first page asked from marker %d, want one after now (the newest chat first)", asked.Marker)
	}

	rows := rowsByID(t, body)
	if _, ok := rows[0]; ok {
		t.Fatalf("the Favorites chat 0 is listed")
	}
	dialog := rows[testSelfID^testPartnerID]
	if dialog["type"] != "user" || dialog["userId"] != float64(testPartnerID) || dialog["title"] != "Иван" ||
		dialog["photo"] != "https://i.example/u.jpg" || dialog["canWrite"] != true || dialog["unreadCount"] != float64(3) ||
		dialog["lastMessageId"] != "116448890093701000" || dialog["lastMessageDate"] != float64(1776869050) {
		t.Fatalf("dialog row: %v", dialog)
	}
	chat := rows[testChatID]
	if chat["type"] != "chat" || chat["title"] != "Покупатели" || chat["membersCount"] != float64(17) || chat["canWrite"] != true {
		t.Fatalf("chat row: %v", chat)
	}
	want := map[int64]string{
		testChannelID:          CantWriteNotAdmin,
		testSelfID ^ 5005:      CantWriteRestricted,
		int64(-70000000000002): CantWriteChatClosed,
		int64(-70000000000003): CantWriteNotMember,
	}
	for id, reason := range want {
		if row := rows[id]; row["canWrite"] != false || row["cantWriteReason"] != reason {
			t.Errorf("chat %d: %v, want canWrite false with %s", id, row, reason)
		}
	}
	if rows[testChannelID]["type"] != "channel" {
		t.Errorf("channel row type: %v", rows[testChannelID])
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// include=lastMessage and include=participants add what was asked for; a
// marker goes to MAX as it is; type keeps the rows of the kinds asked for.
func TestChatListIncludesTypeAndMarker(t *testing.T) {
	f := chatsMAX(t)
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	status, body := callURL(t, s.ListChats(), userID, "GET",
		"/chat/list?include=lastMessage,participants&type=chat&type=channel&marker=1776869050502", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	if !strings.Contains(string(f.lastRaw(maxclient.OpChatsList)), `"marker":1776869050502`) {
		t.Fatalf("MAX got %s, want the caller's marker", f.lastRaw(maxclient.OpChatsList))
	}
	rows := rowsByID(t, body)
	for id, row := range rows {
		if row["type"] != "chat" && row["type"] != "channel" {
			t.Fatalf("chat %d of type %v passed type=chat,channel", id, row["type"])
		}
	}
	chat := rows[testChatID]
	last, _ := chat["lastMessage"].(map[string]interface{})
	if last["text"] != "SECRET-TEXT" || last["id"] != "116448890093701000" {
		t.Fatalf("lastMessage = %v, want MAX's message", chat["lastMessage"])
	}
	if mustJSON(t, chat["participants"]) != "[1000,4004]" {
		t.Fatalf("participants = %v", chat["participants"])
	}
	if status, body := callURL(t, s.ListChats(), userID, "GET", "/chat/list?type=group", ""); status != http.StatusBadRequest {
		t.Fatalf("type=group: %d %v, want 400", status, body)
	}
}

// q keeps the rows of the page whose title holds it, in any letter case, a
// dialog's title being the person's name; nextMarker stays, so the caller
// pages on for the rest. MAX's search (opcode 68) is not asked.
func TestChatListSearch(t *testing.T) {
	f := chatsMAX(t)
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	// "пОк": Покупатели, the group; "ива": Иван, the dialog's person.
	for q, want := range map[string]int64{"%D0%BF%D0%9E%D0%BA": testChatID, "%D0%B8%D0%B2%D0%B0": testSelfID ^ testPartnerID} {
		status, body := callURL(t, s.ListChats(), userID, "GET", "/chat/list?q="+q, "")
		if status != http.StatusOK {
			t.Fatalf("q=%s: %d %v", q, status, body)
		}
		rows := rowsByID(t, body)
		if len(rows) != 1 || rows[want] == nil {
			t.Fatalf("q=%s: rows %v, want only chat %d", q, rows, want)
		}
		if body["nextMarker"] != "1776820000000" {
			t.Fatalf("q=%s: nextMarker = %v, want the page's marker", q, body["nextMarker"])
		}
	}
	if n := f.count(maxclient.OpChatSearch); n != 0 {
		t.Fatalf("MAX's search was asked %d times", n)
	}
}

// GET /chat/info gives the rows in the order asked; what MAX does not show
// is in missing.
func TestChatInfo(t *testing.T) {
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) == maxclient.OpChatInfo {
			chats := testChats()
			return map[string]interface{}{"chats": []interface{}{chats[3], chats[2]}}
		}
		return map[string]interface{}{}
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	status, body := callURL(t, s.ChatInfo(), userID, "GET",
		"/chat/info?chatId=-70000000000000&chatId=-5,-70000000000001", "")
	if status != http.StatusOK {
		t.Fatalf("info: %d %v", status, body)
	}
	items := body["items"].([]interface{})
	if len(items) != 2 || items[0].(map[string]interface{})["chatId"] != float64(testChatID) ||
		items[1].(map[string]interface{})["chatId"] != float64(testChannelID) {
		t.Fatalf("items = %v, want the chat then the channel, as asked", items)
	}
	if mustJSON(t, body["missing"]) != "[-5]" {
		t.Fatalf("missing = %v, want [-5]", body["missing"])
	}
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = strconv.Itoa(-(i + 1))
	}
	if status, _ := callURL(t, s.ChatInfo(), userID, "GET", "/chat/info?chatId="+strings.Join(ids, ","), ""); status != http.StatusBadRequest {
		t.Fatalf("101 chats: %d, want 400", status)
	}
}

// A stop signal on the list pauses the account (409 page_paused with MAX's
// code); any other MAX error is 502 upstream_error with MAX's code.
func TestChatListPauseAndErrors(t *testing.T) {
	var refusal atomic.Value
	refusal.Store("too.many.requests")
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) == maxclient.OpChatsList {
			return map[string]interface{}{"error": refusal.Load().(string), "message": "Refused"}
		}
		return map[string]interface{}{}
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	status, body := callURL(t, s.ListChats(), userID, "GET", "/chat/list", "")
	wantError(t, status, body, http.StatusConflict, CodePagePaused)
	if body["upstreamCode"] != "too.many.requests" || body["paused"] != true {
		t.Fatalf("stop signal: %v, want upstreamCode and the pause", body)
	}

	if err := s.resumeAccount(userID, testSelfID); err != nil {
		t.Fatal(err)
	}
	refusal.Store("error.unknown")
	status, body = callURL(t, s.ListChats(), userID, "GET", "/chat/list", "")
	wantError(t, status, body, http.StatusBadGateway, CodeUpstreamError)
	if body["upstreamCode"] != "error.unknown" || body["paused"] != nil {
		t.Fatalf("other error: %v", body)
	}
}

// The pause holds what the account does in MAX, not what it reads (owner's
// decision of 2026-11-05): while it is paused the chat list, chat info,
// history, mark read, contacts and user info go to MAX and answer, while an
// edit answers 409 page_paused without a MAX call.
func TestPauseLetsReadsThrough(t *testing.T) {
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		switch maxclient.Opcode(fr.Opcode) {
		case maxclient.OpChatsList, maxclient.OpChatInfo:
			return map[string]interface{}{"chats": []interface{}{}, "marker": 0}
		case maxclient.OpChatHistory:
			return map[string]interface{}{"messages": []interface{}{}}
		case maxclient.OpContactInfo:
			return map[string]interface{}{"contacts": []interface{}{}}
		}
		return map[string]interface{}{}
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)
	s.pauseAccount(userID, testSelfID, "error.user.restricted.send")

	reads := []struct {
		name string
		op   maxclient.Opcode
		do   func() (int, map[string]interface{})
	}{
		{"list", maxclient.OpChatsList, func() (int, map[string]interface{}) {
			return callURL(t, s.ListChats(), userID, "GET", "/chat/list", "")
		}},
		{"info", maxclient.OpChatInfo, func() (int, map[string]interface{}) {
			return callURL(t, s.ChatInfo(), userID, "GET", "/chat/info?chatId=-5", "")
		}},
		{"history", maxclient.OpChatHistory, func() (int, map[string]interface{}) {
			return call(t, s.GetChatHistory(), userID, `{"chatId":-5,"count":10}`)
		}},
		{"markread", maxclient.OpChatMark, func() (int, map[string]interface{}) {
			return call(t, s.MarkRead(), userID, `{"chatId":-5,"messageId":"7"}`)
		}},
		{"user info", maxclient.OpContactInfo, func() (int, map[string]interface{}) {
			return call(t, s.GetUser(), userID, `{"userIds":[42]}`)
		}},
	}
	for _, rd := range reads {
		before := f.count(rd.op)
		if status, body := rd.do(); status != http.StatusOK {
			t.Fatalf("%s while paused: %d %v, want 200", rd.name, status, body)
		}
		if f.count(rd.op) != before+1 {
			t.Fatalf("%s while paused made no MAX call", rd.name)
		}
	}

	before := f.total()
	status, body := call(t, s.SendEditMessage(), userID, `{"chatId":-5,"messageId":"7","text":"x"}`)
	wantError(t, status, body, http.StatusConflict, CodePagePaused)
	if body["paused"] != true || f.total() != before {
		t.Fatalf("edit while paused: %v after %d MAX calls, want the pause and no call", body, f.total()-before)
	}
}
