package maxclient

import (
	"encoding/json"
	"testing"
)

// MAX v10 sends every 64-bit value in extension 1, message ids among them;
// JSON v11 sent message ids as strings. An integer beyond ±2^53 reads as a
// JSON string, so Message.ID decodes and the raw payload webhooks pass on
// keeps every digit (live 2026-11-18: the chat list failed to decode, "cannot
// unmarshal number into Chat.lastMessage.id of type string").
func TestBigIntegersReadAsStrings(t *testing.T) {
	in := `{"chatId":80897482330686,"message":{"id":117638182879114852,"time":1795016218248},"neg":-9007199254740993,"safe":9007199254740991}`
	packed, err := jsonToMsgpack([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := msgpackToJSON(packed)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"chatId":80897482330686,"message":{"id":"117638182879114852","time":1795016218248},"neg":"-9007199254740993","safe":9007199254740991}`
	if string(raw) != want {
		t.Fatalf("json\n got %s\nwant %s", raw, want)
	}

	// Wrapped in extension 1, as MAX sends it.
	wrapped := mustHex(t, "81 a2 6964 c7 09 01 cf 01a1ef4dd6883664")
	raw, err = msgpackToJSON(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("Message from %s: %v", raw, err)
	}
	if msg.ID != "117638182879114852" {
		t.Fatalf("Message.ID %q", msg.ID)
	}

	// A raw payload keeps the id exact.
	var payload map[string]interface{}
	_ = json.Unmarshal(raw, &payload)
	out, _ := json.Marshal(payload)
	if string(out) != `{"id":"117638182879114852"}` {
		t.Fatalf("re-encoded %s", out)
	}
}

// A message id the client holds as a string goes to MAX as a number.
func TestReactionIDsGoAsNumbers(t *testing.T) {
	payload, _ := json.Marshal(map[string]interface{}{"messageId": wireMessageID("117638182879114852"), "other": wireMessageID("abc")})
	if string(payload) != `{"messageId":117638182879114852,"other":"abc"}` {
		t.Fatalf("payload %s", payload)
	}
	packed, err := jsonToMsgpack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustHex(t, "82 a9 6d65737361676549 64 cf 01a1ef4dd6883664 a5 6f74686572 a3 616263"); string(packed) != string(want) {
		t.Fatalf("msgpack % x\n   want % x", packed, want)
	}
}
