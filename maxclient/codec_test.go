package maxclient

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pierrec/lz4/v4"
)

// testFrame encodes a frame a fake MAX sends, given as {cmd, seq, opcode,
// payload}.
func testFrame(m map[string]interface{}) []byte {
	num := func(k string) int {
		switch v := m[k].(type) {
		case int:
			return v
		case Opcode:
			return int(v)
		case float64:
			return int(v)
		}
		return 0
	}
	body, _ := json.Marshal(m["payload"])
	out, err := EncodeFrame(num("cmd"), num("seq"), num("opcode"), body)
	if err != nil {
		panic(err)
	}
	return out
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The header is max-cli's and PyMax's: ver, cmd, seq u16, opcode u16, then
// flags in the top byte and the length in the low 24 bits, big-endian.
func TestEncodeFrameHeader(t *testing.T) {
	frame, err := EncodeFrame(0, 7, int(OpSessionInit), []byte(`{"deviceId":"d"}`))
	if err != nil {
		t.Fatal(err)
	}
	// {"deviceId": "d"}: fixmap 1, fixstr 8 "deviceId", fixstr 1 "d"
	body := mustHex(t, "81 a8 6465766963654964 a1 64")
	want := append(mustHex(t, "0a 00 0007 0006 00 00000c"), body...)
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame\n got % x\nwant % x", frame, want)
	}
}

// An empty payload is an empty body, as the web client sends its ping.
func TestEncodeEmptyPayload(t *testing.T) {
	for _, p := range []string{"", "null", "{}", " {} "} {
		frame, err := EncodeFrame(0, 1, int(OpPing), []byte(p))
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) != FrameHeaderSize {
			t.Fatalf("%q: % x, want a bare header", p, frame)
		}
	}
}

// Numbers go out as integers of the smallest width; a time past 32 bits as
// uint64 (MAX refuses a time sent as a float), and key order is kept.
func TestEncodeNumbersAndOrder(t *testing.T) {
	body, err := jsonToMsgpack([]byte(`{"b":1,"a":-1,"t":1790328205681,"n":-200,"f":1.5,"big":18446744073709551615,"s":"ж","l":[true,false,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := mustHex(t, "88"+
		"a162 01"+
		"a161 ff"+
		"a174 cf000001a0d7e07171"+
		"a16e d1ff38"+
		"a166 cb3ff8000000000000"+
		"a3626967 cfffffffffffffffff"+
		"a173 a2d0b6"+
		"a16c 93c3c2c0")
	if !bytes.Equal(body, want) {
		t.Fatalf("body\n got % x\nwant % x", body, want)
	}
}

// Every integer width reads back with every digit (past 2^53 as a string);
// ids in extension type 1 are unwrapped; integer map keys become strings; bin is base64.
func TestDecodeMsgpack(t *testing.T) {
	body := mustHex(t, "87"+
		"a2 6964 c7 09 01 cf 019ee04a2689fda8"+ // "id": ext1(uint64 116777249436990888)
		"a1 6e d3 fffffffffffffffe"+ // "n": int64 -2
		"a1 66 ca 3fc00000"+ // "f": float32 1.5
		"a1 6d 81 cd 3039 a1 78"+ // "m": {12345: "x"}
		"a1 62 c4 02 0102"+ // "b": bin
		"a1 61 92 e0 cc ff"+ // "a": [-32, 255]
		"a1 7a c7 02 05 0000") // "z": ext5 → null
	raw, err := msgpackToJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"116777249436990888","n":-2,"f":1.5,"m":{"12345":"x"},"b":"AQI=","a":[-32,255],"z":null}`
	if string(raw) != want {
		t.Fatalf("json\n got %s\nwant %s", raw, want)
	}
}

// What jsonToMsgpack writes, msgpackToJSON reads back the same.
func TestCodecRoundTrip(t *testing.T) {
	in := `{"chatId":-68123456789012,"message":{"text":"` + strings.Repeat("привет ", 50) + `","cid":1790328205681,"attaches":[{"_type":"PHOTO","photoToken":"tok"}]},"notify":true,"x":[],"y":{},"e":1e+21}`
	body, err := jsonToMsgpack([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := msgpackToJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round trip\n got %s\nwant %s", out, in)
	}
}

// A body MAX compressed with LZ4 (flags = how many times it grew) reads as
// the plain one, ids above 2^53 included.
func TestDecodeCompressedFrame(t *testing.T) {
	plain, _ := jsonToMsgpack([]byte(`{"messages":[` + strings.Repeat(`{"id":116777249436990888,"text":"hello hello hello"},`, 40) + `{"id":1}]}`))
	compressed := make([]byte, lz4.CompressBlockBound(len(plain)))
	n, err := lz4.CompressBlock(plain, compressed, nil)
	if err != nil || n == 0 {
		t.Fatalf("compress: %d %v", n, err)
	}
	compressed = compressed[:n]
	flags := (len(plain) + n - 1) / n
	frame := append(mustHex(t, "0a 01 7fff 0031"), byte(flags), byte(n>>16), byte(n>>8), byte(n))
	frame = append(frame, compressed...)

	resp, err := DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ver != 10 || resp.Cmd != 1 || resp.Seq != 32767 || resp.Opcode != int(OpChatHistory) {
		t.Fatalf("header %+v", resp)
	}
	var got struct {
		Messages []struct {
			ID json.Number `json:"id"`
		} `json:"messages"`
	}
	if err := resp.decodePayload(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 41 || got.Messages[0].ID != "116777249436990888" {
		t.Fatalf("payload %+v", got.Messages[:1])
	}
	if _, ok := resp.Payload["messages"].([]interface{}); !ok {
		t.Fatalf("Payload %v", resp.Payload)
	}

	// The flags under-reporting the growth still reads.
	frame[6] = 1
	if _, err := DecodeFrame(frame); err != nil {
		t.Fatalf("flags 1: %v", err)
	}
}

// Broken frames are errors, never panics; the header still comes back.
func TestDecodeBrokenFrames(t *testing.T) {
	cases := map[string][]byte{
		"short":         mustHex(t, "0a 01 0001"),
		"length":        mustHex(t, "0a 01 0001 0006 00 000010 81"),
		"truncated str": mustHex(t, "0a 01 0001 0006 00 000002 a5 61"),
		"huge map":      mustHex(t, "0a 01 0001 0006 00 000005 df ffffffff"),
		"bad lz4":       mustHex(t, "0a 01 0001 0006 02 000003 f0 ff ff"),
		"zstd":          mustHex(t, "0a 01 0001 0006 ff 000001 00"),
		"type c1":       mustHex(t, "0a 01 0001 0006 00 000001 c1"),
		"map key":       mustHex(t, "0a 01 0001 0006 00 000004 81 90 a0 00"),
		"deep":          append(mustHex(t, "0a 01 0001 0006 00 000200"), bytes.Repeat([]byte{0x91}, 0x200)...),
	}
	for name, frame := range cases {
		resp, err := DecodeFrame(frame)
		if !errors.Is(err, ErrFrame) {
			t.Errorf("%s: err %v, want ErrFrame", name, err)
		}
		if name != "short" && (resp == nil || resp.Seq != 1 || resp.Opcode != 6) {
			t.Errorf("%s: header %+v", name, resp)
		}
	}
}

// An answer whose body cannot be read fails its request at once.
func TestUnreadableAnswerFailsTheRequest(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		req, _ := DecodeFrame(data)
		bad := []byte{10, 1, byte(req.Seq >> 8), byte(req.Seq), 0, byte(req.Opcode), 0, 0, 0, 1, 0xc1}
		_ = conn.WriteMessage(websocket.BinaryMessage, bad)
		time.Sleep(time.Second)
	}))
	defer srv.Close()
	c := newTestClient()
	c.URI = "ws" + strings.TrimPrefix(srv.URL, "http")
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, err := c.sendAndWaitWithTimeout(OpSessionInit, map[string]interface{}{"deviceId": "d"}, 5*time.Second)
	if !errors.Is(err, ErrFrame) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v, want ErrFrame at once", err, time.Since(start))
	}
}

// MAX's ping is answered with an empty answer of its seq, a new message with
// its chatId and messageId, as the web client does; the ping is no event.
func TestPushesAreAnswered(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	answers := make(chan *Response, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.BinaryMessage, testFrame(map[string]interface{}{"cmd": 0, "seq": 40000, "opcode": OpPing, "payload": nil}))
		msg, _ := EncodeFrame(0, 40001, int(OpNotifMessage), []byte(`{"chatId":-70000000000001,"message":{"id":116777249436990888,"text":"hi"}}`))
		_ = conn.WriteMessage(websocket.BinaryMessage, msg)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage {
				t.Errorf("an answer of kind %d", kind)
			}
			resp, err := DecodeFrame(data)
			if err != nil {
				t.Errorf("answer: %v", err)
				return
			}
			answers <- resp
		}
	}))
	defer srv.Close()
	events := make(chan Event, 4)
	c := newTestClient()
	c.URI = "ws" + strings.TrimPrefix(srv.URL, "http")
	c.SetEventHandler(func(e Event) { events <- e })
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i, want := range []struct {
		seq, opcode int
		payload     string
	}{
		{40000, int(OpPing), "null"},
		// The id goes out a number; the fake server's decoder reads it back
		// as a string, being past 2^53.
		{40001, int(OpNotifMessage), `{"chatId":-70000000000001,"messageId":"116777249436990888"}`},
	} {
		select {
		case a := <-answers:
			if a.Cmd != 1 || a.Seq != want.seq || a.Opcode != want.opcode || string(a.RawPayload) != want.payload {
				t.Fatalf("answer %d: cmd %d seq %d opcode %d payload %s", i, a.Cmd, a.Seq, a.Opcode, a.RawPayload)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("answer %d: none", i)
		}
	}
	select {
	case e := <-events:
		if e.Opcode != OpNotifMessage {
			t.Fatalf("event %v, want the message alone", e.Opcode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message event")
	}
	select {
	case e := <-events:
		t.Fatalf("a second event %v", e.Opcode)
	case <-time.After(200 * time.Millisecond):
	}
}
