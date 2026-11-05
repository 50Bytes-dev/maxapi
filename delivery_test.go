package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maxapi/maxclient"
)

// sentAttaches decodes the attaches of the last send frame.
func sentMessage(t *testing.T, f *fakeMAX) (msg struct {
	Text     string                 `json:"text"`
	CID      int64                  `json:"cid"`
	Attaches []maxclient.Attachment `json:"attaches"`
}) {
	t.Helper()
	var frame struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(f.lastRaw(maxclient.OpMsgSend), &frame); err != nil {
		t.Fatalf("decode the send frame: %v", err)
	}
	if err := json.Unmarshal(frame.Message, &msg); err != nil {
		t.Fatalf("decode the message: %v", err)
	}
	return msg
}

// Photos uploaded once go into a message as its attaches, in order; a
// message with attachments may have no text; the answer's messageId is a
// string; a chat's negative id goes to MAX as it is (C4).
func TestSendWithAttachments(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	status, body := call(t, s.SendMessage(), userID,
		`{"chatId": -70000000000000, "text": "", "attachments": ["T1", "T2", "T3", "T4", "T5"]}`)

	if status != http.StatusOK {
		t.Fatalf("send: %d %v", status, body)
	}
	if _, ok := body["messageId"].(string); !ok || body["chatId"] != float64(-70000000000000) {
		t.Fatalf("answer %v, want a string messageId and the chat", body)
	}
	if !strings.Contains(string(f.lastRaw(maxclient.OpMsgSend)), `"chatId":-70000000000000`) {
		t.Fatalf("send frame %s, want the negative chat id", f.lastRaw(maxclient.OpMsgSend))
	}
	msg := sentMessage(t, f)
	if len(msg.Attaches) != 5 {
		t.Fatalf("attaches %v, want 5", msg.Attaches)
	}
	for i, a := range msg.Attaches {
		if a.Type != maxclient.AttachTypePhoto || a.PhotoToken != []string{"T1", "T2", "T3", "T4", "T5"}[i] {
			t.Fatalf("attach %d = %+v", i, a)
		}
	}
}

// Twelve attachments, MAX's most (live 2026-11-05), go in one message.
func TestSendTwelveAttachments(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	tokens := strings.Split("a,b,c,d,e,f,g,h,i,j,k,l", ",")
	status, body := call(t, s.SendMessage(), userID, mustJSON(t, map[string]interface{}{"chatId": 5, "attachments": tokens}))
	if status != http.StatusOK {
		t.Fatalf("12 attachments: %d %v", status, body)
	}
	if n := len(sentMessage(t, f).Attaches); n != 12 {
		t.Fatalf("MAX got %d attachments, want 12", n)
	}
}

// A message MAX would refuse as it is never reaches MAX: no text and no
// attachment, a text over 4000 UTF-16 units (an emoji counts 2), more than
// 12 attachments. A ref over 128 bytes is invalid input.
func TestSendRefusesBadMessagesWithoutMAX(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty", `{"chatId": 5, "text": ""}`, 422, CodeMessageRejected},
		{"4001 units", mustJSON(t, map[string]interface{}{"chatId": 5, "text": strings.Repeat("я", 4001)}), 422, CodeMessageRejected},
		{"2001 emoji", mustJSON(t, map[string]interface{}{"chatId": 5, "text": strings.Repeat("😀", 2001)}), 422, CodeMessageRejected},
		{"13 attachments", mustJSON(t, map[string]interface{}{"chatId": 5, "attachments": strings.Split("a,b,c,d,e,f,g,h,i,j,k,l,m", ",")}), 422, CodeMessageRejected},
		{"long ref", mustJSON(t, map[string]interface{}{"chatId": 5, "text": "x", "ref": strings.Repeat("r", 129)}), 400, ErrInvalidInput},
		{"no target", `{"chatId": 0, "text": "x"}`, 400, CodeInvalidTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := call(t, s.SendMessage(), userID, tc.body)
			wantError(t, status, body, tc.status, tc.code)
		})
	}
	if n := f.count(maxclient.OpMsgSend); n != 0 {
		t.Fatalf("MAX received %d sends, want 0", n)
	}
	status, body := call(t, s.SendMessage(), userID, mustJSON(t, map[string]interface{}{"chatId": 5, "text": strings.Repeat("😀", 2000)}))
	if status != http.StatusOK {
		t.Fatalf("2000 emoji (4000 units): %d %v, want sent", status, body)
	}
}

// MAX refusing an attachment of a message answers attachment_rejected;
// refusing the text, message_rejected (C4).
func TestSendRefusalsOfAttachmentAndMessage(t *testing.T) {
	cases := []struct{ code, body, want string }{
		{"photo.token.expired", `{"chatId": 5, "attachments": ["T"]}`, CodeAttachmentRejected},
		{"error.message.too.long", `{"chatId": 5, "text": "x"}`, CodeMessageRejected},
		{"error.unknown", `{"chatId": 5, "attachments": ["T"]}`, CodeSendRejected},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			f := newFakeMAX(t, func(fakeFrame) map[string]interface{} {
				return map[string]interface{}{"error": tc.code, "message": "No"}
			})
			userID, _ := connectedUser(t, f)
			status, body := call(t, newTestServer(t).SendMessage(), userID, tc.body)
			wantError(t, status, body, http.StatusUnprocessableEntity, tc.want)
			if body["upstreamCode"] != tc.code {
				t.Fatalf("upstreamCode %v, want %s", body["upstreamCode"], tc.code)
			}
		})
	}
}

// A ref gives the message a cid of its own, the same for every send with that
// ref; a send without a ref has a cid of the current time.
func TestRefGivesDeterministicCID(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	call(t, s.SendMessage(), userID, `{"chatId": 5, "text": "a", "ref": "order-1"}`)
	first := sentMessage(t, f).CID
	call(t, s.SendMessage(), userID, `{"chatId": 5, "text": "a", "ref": "order-1"}`)
	again := sentMessage(t, f).CID
	call(t, s.SendMessage(), userID, `{"chatId": 5, "text": "a", "ref": "order-2"}`)
	other := sentMessage(t, f).CID
	before := time.Now().UnixMilli()
	call(t, s.SendMessage(), userID, `{"chatId": 5, "text": "a"}`)
	plain := sentMessage(t, f).CID

	if first != refCID("order-1") || again != first || other == first {
		t.Fatalf("cids %d, %d, %d: want one cid per ref", first, again, other)
	}
	if first < refCIDBase || first >= refCIDBase+refCIDSpan {
		t.Fatalf("cid %d outside the unix-ms range of the web client's cids", first)
	}
	if plain < before {
		t.Fatalf("a send without a ref has cid %d, want the current time", plain)
	}
}

// A media send answers with the delivery codes too: MAX refusing the upload
// is attachment_rejected, and a paused account's send never reaches MAX
// (M5, M9).
func TestMediaSendSharesTheDeliveryRules(t *testing.T) {
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		switch maxclient.Opcode(fr.Opcode) {
		case maxclient.OpContactInfoByPhone:
			return contactReply(fr)
		case maxclient.OpFileUpload:
			return map[string]interface{}{"error": "error.unknown", "message": "no uploads here"}
		}
		return sentReply(fr)
	})
	s := newTestServer(t)
	userID := t.Name()
	sessionUser(t, s, f, userID)

	doc := mustJSON(t, map[string]interface{}{"phone": testPhone, "document": "aGVsbG8=", "fileName": "a.txt"})
	status, body := call(t, s.SendDocument(), userID, doc)
	wantError(t, status, body, http.StatusUnprocessableEntity, CodeAttachmentRejected)

	s.pauseAccount(userID, testSelfID, "error.user.restricted.send")
	before := f.total()
	status, body = call(t, s.SendDocument(), userID, doc)
	wantError(t, status, body, http.StatusUnprocessableEntity, CodeSendRejected)
	if body["paused"] != true || f.total() != before {
		t.Fatalf("paused account: %v after %d MAX calls, want the pause and no call", body, f.total()-before)
	}
}

// The gateway does not pace sends: the projects using it do (owner's
// decision of 2026-11-05). Sends of one account right after each other, and
// one while another waits for MAX's answer, all go to MAX.
func TestSendsAreNotPaced(t *testing.T) {
	// MAX never acknowledges the first send: it stays in flight for
	// sendTextReplyTimeout while the next ones are made.
	var sends atomic.Int32
	f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) == maxclient.OpMsgSend && sends.Add(1) == 1 {
			return nil
		}
		return sentReply(fr)
	})
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		call(t, s.SendMessage(), userID, sendByChat)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.count(maxclient.OpMsgSend) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		if status, body := call(t, s.SendMessage(), userID, sendByChat); status != http.StatusOK {
			t.Fatalf("send %d while the first one waits: %d %v", i+2, status, body)
		}
	}
	if n := f.count(maxclient.OpMsgSend); n != 3 {
		t.Fatalf("MAX received %d sends, want 3", n)
	}
	wg.Wait()
}

// deleteMAX answers MSG_DELETE with handle and records the ids it got.
func deleteMAX(t *testing.T, handle func(ids []int64) map[string]interface{}) *fakeMAX {
	return newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) != maxclient.OpMsgDelete {
			return map[string]interface{}{}
		}
		var p struct {
			MessageIDs []int64 `json:"messageIds"`
		}
		_ = json.Unmarshal(fr.Raw, &p)
		return handle(p.MessageIDs)
	})
}

// POST /chat/delete deletes for everyone by MAX's exact ids (above 2^53) and
// answers per message: MAX's answer lists what it deleted (C5).
func TestDeleteMessages(t *testing.T) {
	f := deleteMAX(t, func(ids []int64) map[string]interface{} {
		return map[string]interface{}{"messageIds": []interface{}{"116448890093701001"}}
	})
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	status, body := call(t, s.DeleteMessage(), userID,
		`{"chatId": -70000000000000, "messageIds": ["116448890093701001", 116448890093701002]}`)

	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, body)
	}
	raw := string(f.lastRaw(maxclient.OpMsgDelete))
	if !strings.Contains(raw, "116448890093701001") || !strings.Contains(raw, "116448890093701002") || !strings.Contains(raw, `"forMe":false`) {
		t.Fatalf("delete frame %s, want both exact ids, for everyone", raw)
	}
	if got := mustJSON(t, body["results"]); got != `[{"messageId":"116448890093701001","status":"deleted"},{"messageId":"116448890093701002","status":"not_found"}]` {
		t.Fatalf("results = %s", got)
	}
}

// A refusal of MAX for the whole delete: a per-message code gives every id
// its status with upstreamCode; a stop signal pauses the account; no answer
// is send_failed; a paused account makes no call.
func TestDeleteRefusals(t *testing.T) {
	cases := []struct {
		code   string
		status int
		want   string
	}{
		{"not.found", http.StatusOK, DeleteStatusNotFound},
		{"error.message.delete.time.expired", http.StatusOK, DeleteStatusTooOld},
		{"chat.denied", http.StatusOK, DeleteStatusForbidden},
		{"error.unknown", http.StatusUnprocessableEntity, CodeSendRejected},
		{"too.many.requests", http.StatusUnprocessableEntity, CodeSendRejected},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			f := deleteMAX(t, func([]int64) map[string]interface{} {
				return map[string]interface{}{"error": tc.code, "message": "No"}
			})
			userID, _ := connectedUser(t, f)
			status, body := call(t, newTestServer(t).DeleteMessage(), userID, `{"chatId": 5, "messageIds": ["1", "2"]}`)
			if status != tc.status {
				t.Fatalf("got %d %v, want %d", status, body, tc.status)
			}
			if status == http.StatusOK {
				for _, r := range body["results"].([]interface{}) {
					row := r.(map[string]interface{})
					if row["status"] != tc.want || row["upstreamCode"] != tc.code {
						t.Fatalf("result %v, want %s with upstreamCode", row, tc.want)
					}
				}
				return
			}
			wantError(t, status, body, tc.status, tc.want)
			if tc.code == "too.many.requests" && body["paused"] != true {
				t.Fatalf("a stop signal on delete did not pause: %v", body)
			}
		})
	}

	f := deleteMAX(t, func([]int64) map[string]interface{} { return nil })
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)
	if testing.Short() {
		return
	}
	status, body := call(t, s.DeleteMessage(), userID, `{"chatId": 5, "messageIds": ["1"]}`)
	wantError(t, status, body, http.StatusInternalServerError, CodeSendFailed)

	ids := make([]string, 101)
	for i := range ids {
		ids[i] = `"1"`
	}
	status, body = call(t, s.DeleteMessage(), userID, `{"chatId": 5, "messageIds": [`+strings.Join(ids, ",")+`]}`)
	wantError(t, status, body, http.StatusBadRequest, ErrInvalidInput)
	status, body = call(t, s.DeleteMessage(), userID, `{"chatId": 0, "messageIds": ["1"]}`)
	wantError(t, status, body, http.StatusBadRequest, CodeInvalidTarget)
}

// A send answered send_failed with a ref is confirmed by MessageOutcome: the
// gateway looks for the send's cid in the chat's last messages (C8).
func TestMessageOutcome(t *testing.T) {
	old := outcomeDelays
	outcomeDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond}
	t.Cleanup(func() { outcomeDelays = old })

	for _, delivered := range []bool{true, false} {
		t.Run(map[bool]string{true: "delivered", false: "lost"}[delivered], func(t *testing.T) {
			ref := "order-" + t.Name()
			f := newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
				switch maxclient.Opcode(fr.Opcode) {
				case maxclient.OpMsgSend:
					return nil // no answer: send_failed
				case maxclient.OpChatHistory:
					msgs := []interface{}{map[string]interface{}{"id": "1", "cid": 7, "sender": testSelfID, "time": 1}}
					if delivered {
						msgs = append(msgs, map[string]interface{}{"id": "116448890093709999", "cid": refCID(ref), "sender": testSelfID, "time": time.Now().UnixMilli()})
					}
					return map[string]interface{}{"messages": msgs}
				}
				return map[string]interface{}{}
			})
			s := newTestServer(t)
			userID := t.Name()
			c := sessionUser(t, s, f, userID)
			sink := newWebhookSink(t)
			liveSession(t, s, userID, c, sink)

			status, body := call(t, s.SendMessage(), userID, mustJSON(t, map[string]interface{}{"chatId": 5, "text": "x", "ref": ref}))
			wantError(t, status, body, http.StatusInternalServerError, CodeSendFailed)

			ev := sink.waitFor(t, EventMessageOutcome)
			if ev["ref"] != ref || ev["chatId"] != "5" || ev["delivered"] != map[bool]string{true: "true", false: "false"}[delivered] {
				t.Fatalf("MessageOutcome %v", ev)
			}
			if delivered && ev["messageId"] != "116448890093709999" {
				t.Fatalf("messageId %q, want MAX's id as a string", ev["messageId"])
			}
			if !delivered && ev["messageId"] != "" {
				t.Fatalf("a lost message has messageId %q", ev["messageId"])
			}
			s.outcomes.wg.Wait()
			if n := f.count(maxclient.OpChatHistory); n != 1 {
				t.Fatalf("%d looks, want 1", n)
			}
		})
	}
}

// A send notifies unless the caller says notify false: MAX's web client sends
// every message with notify true, and MAX refuses a channel post without it.
func TestSendNotifiesByDefault(t *testing.T) {
	f := newFakeMAX(t, sentReply)
	userID, _ := connectedUser(t, f)
	s := newTestServer(t)

	for body, want := range map[string]bool{
		`{"chatId":-5,"text":"x"}`:                true,
		`{"chatId":-5,"text":"x","notify":true}`:  true,
		`{"chatId":-5,"text":"x","notify":false}`: false,
	} {
		if status, resp := call(t, s.SendMessage(), userID, body); status != http.StatusOK {
			t.Fatalf("%s: %d %v", body, status, resp)
		}
		if got := f.lastPayload(maxclient.OpMsgSend)["notify"]; got != want {
			t.Fatalf("%s: MAX got notify %v, want %v", body, got, want)
		}
	}
}
