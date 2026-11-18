package maxclient

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/pierrec/lz4/v4"
)

// The binary protocol v10, the one MAX's web client speaks on
// wss://api.oneme.ru/websocket since JSON v11 went dark on 2026-11-18
// (14:20:53 MSK: every socket dropped, ws-api.oneme.ru stopped answering).
//
// A frame is a 10-byte big-endian header and a body:
//
//	ver u8 (10) | cmd u8 | seq u16 | opcode u16 | flags u8 | length u24
//
// flags 0 is a plain body; 1..0x7f an LZ4 block, flags being how many times
// the body grew when compressed, rounded up; 0xff zstd (never seen). The body
// is one MessagePack value, the payload. MAX wraps 64-bit ids in extension
// type 1, whose data is itself a MessagePack value.
//
// The rest of the client speaks JSON: an outgoing payload is marshalled to
// JSON and transcoded to MessagePack, an incoming body is transcoded to JSON
// and decoded as before. MAX sends every 64-bit value in extension 1 (ids
// and times alike); of them only message ids (~1.2e17) pass 2^53, and JSON
// v11 sent those as strings ("id": "117638182879114852"). The transcoder
// writes every integer beyond ±2^53 as a JSON string, so Message.ID and the
// payloads webhooks pass on read as in v11, every digit kept (live,
// 2026-11-18: LOGIN and the chat list). Sources: PyMax 2.5.0
// (src/pymax/protocol/tcp), WireCatLabs/max-cli (src/protocol/frame.ts, a
// capture of web.max.ru of 2026-09-25).

// FrameHeaderSize is the size of a v10 frame header.
const FrameHeaderSize = 10

const (
	cmdAnswer = 1

	flagsZstd        = 0xff
	maxLZ4Flags      = 0x7f
	maxBodyLength    = 1<<24 - 1
	maxDecompressed  = 32 << 20 // what one incoming body may grow to
	maxMsgpackDepth  = 256
	extWrappedNumber = 1

	// maxSafeInteger is 2^53 - 1, the largest integer a float64 (and a
	// JavaScript number) holds exactly.
	maxSafeInteger = 1<<53 - 1
)

// ErrFrame is the error of a frame the client cannot read.
var ErrFrame = errors.New("malformed MAX frame")

// EncodeFrame builds a v10 frame of cmd, seq and opcode with a JSON payload.
// An empty payload (none, null or {}) goes out as an empty body, as the web
// client sends its pings. The body is not compressed: MAX takes plain bodies
// (PyMax sends them so).
func EncodeFrame(cmd, seq, opcode int, payload []byte) ([]byte, error) {
	body, err := jsonToMsgpack(payload)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyLength {
		return nil, ErrFrameTooLarge
	}
	frame := make([]byte, FrameHeaderSize+len(body))
	frame[0] = ProtocolVersion
	frame[1] = byte(cmd)
	binary.BigEndian.PutUint16(frame[2:], uint16(seq))
	binary.BigEndian.PutUint16(frame[4:], uint16(opcode))
	binary.BigEndian.PutUint32(frame[6:], uint32(len(body))) // flags 0
	copy(frame[FrameHeaderSize:], body)
	return frame, nil
}

// DecodeFrame reads a v10 frame. Payload and RawPayload hold the body as JSON;
// an empty body is a null payload. A header whose body cannot be read still
// comes back, with the error.
func DecodeFrame(data []byte) (*Response, error) {
	if len(data) < FrameHeaderSize {
		return nil, fmt.Errorf("%w: %d bytes, shorter than the header", ErrFrame, len(data))
	}
	packed := binary.BigEndian.Uint32(data[6:])
	flags, length := int(packed>>24), int(packed&0xffffff)
	resp := &Response{
		Ver:        int(data[0]),
		Cmd:        int(data[1]),
		Seq:        int(binary.BigEndian.Uint16(data[2:])),
		Opcode:     int(binary.BigEndian.Uint16(data[4:])),
		RawPayload: json.RawMessage("null"),
	}
	if FrameHeaderSize+length > len(data) {
		return resp, fmt.Errorf("%w: the header claims %d bytes, the frame carries %d", ErrFrame, length, len(data)-FrameHeaderSize)
	}
	body := data[FrameHeaderSize : FrameHeaderSize+length]
	switch {
	case flags == flagsZstd:
		return resp, fmt.Errorf("%w: a zstd body", ErrFrame)
	case flags > maxLZ4Flags:
		return resp, fmt.Errorf("%w: compression flags %d", ErrFrame, flags)
	case flags > 0:
		var err error
		if body, err = decompressLZ4(body, flags); err != nil {
			return resp, err
		}
	}
	if len(body) == 0 {
		return resp, nil
	}
	raw, err := msgpackToJSON(body)
	if err != nil {
		return resp, err
	}
	resp.RawPayload = raw
	// A payload that is not a map (never seen) stays in RawPayload alone.
	var payload map[string]interface{}
	if json.Unmarshal(raw, &payload) == nil {
		resp.Payload = payload
	}
	return resp, nil
}

// decompressLZ4 inflates an LZ4 block. flags × length bounds what the web
// client compresses; the buffer grows past it should MAX round otherwise, up
// to maxDecompressed.
func decompressLZ4(src []byte, flags int) ([]byte, error) {
	size := flags * len(src)
	if size < 64 {
		size = 64
	}
	for {
		if size > maxDecompressed {
			size = maxDecompressed
		}
		dst := make([]byte, size)
		n, err := lz4.UncompressBlock(src, dst)
		if err == nil {
			return dst[:n], nil
		}
		if size == maxDecompressed {
			return nil, fmt.Errorf("%w: an LZ4 body: %v", ErrFrame, err)
		}
		size *= 2
	}
}

// --- MessagePack → JSON ---

func msgpackToJSON(data []byte) (json.RawMessage, error) {
	r := &mpReader{b: data}
	var out bytes.Buffer
	if err := r.value(&out, 0); err != nil {
		return nil, err
	}
	// Bytes after the value (PyMax met some) are left unread, as PyMax does.
	return out.Bytes(), nil
}

type mpReader struct {
	b []byte
	p int
}

func (r *mpReader) errf(format string, a ...interface{}) error {
	return fmt.Errorf("%w: MessagePack at byte %d: %s", ErrFrame, r.p, fmt.Sprintf(format, a...))
}

func (r *mpReader) take(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.p {
		return nil, r.errf("%d bytes past the end", n)
	}
	s := r.b[r.p : r.p+n]
	r.p += n
	return s, nil
}

func (r *mpReader) uint(n int) (uint64, error) {
	s, err := r.take(n)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range s {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// value transcodes one MessagePack value to JSON.
func (r *mpReader) value(out *bytes.Buffer, depth int) error {
	if depth > maxMsgpackDepth {
		return r.errf("nested deeper than %d", maxMsgpackDepth)
	}
	tb, err := r.take(1)
	if err != nil {
		return err
	}
	t := tb[0]
	switch {
	case t <= 0x7f:
		out.WriteString(strconv.Itoa(int(t)))
		return nil
	case t >= 0xe0:
		out.WriteString(strconv.Itoa(int(int8(t))))
		return nil
	case t >= 0x80 && t <= 0x8f:
		return r.mapBody(out, int(t&0x0f), depth)
	case t >= 0x90 && t <= 0x9f:
		return r.arrayBody(out, int(t&0x0f), depth)
	case t >= 0xa0 && t <= 0xbf:
		return r.str(out, int(t&0x1f))
	}
	switch t {
	case 0xc0:
		out.WriteString("null")
	case 0xc2:
		out.WriteString("false")
	case 0xc3:
		out.WriteString("true")
	case 0xc4, 0xc5, 0xc6: // bin: as base64, the way encoding/json writes []byte
		n, err := r.uint(1 << (t - 0xc4))
		if err != nil {
			return err
		}
		s, err := r.take(int(n))
		if err != nil {
			return err
		}
		writeJSONString(out, base64.StdEncoding.EncodeToString(s))
	case 0xc7, 0xc8, 0xc9:
		n, err := r.uint(1 << (t - 0xc7))
		if err != nil {
			return err
		}
		return r.ext(out, int(n), depth)
	case 0xca:
		v, err := r.uint(4)
		if err != nil {
			return err
		}
		writeJSONFloat(out, float64(math.Float32frombits(uint32(v))))
	case 0xcb:
		v, err := r.uint(8)
		if err != nil {
			return err
		}
		writeJSONFloat(out, math.Float64frombits(v))
	case 0xcc, 0xcd, 0xce, 0xcf:
		v, err := r.uint(1 << (t - 0xcc))
		if err != nil {
			return err
		}
		if v > maxSafeInteger {
			writeJSONString(out, strconv.FormatUint(v, 10))
		} else {
			out.WriteString(strconv.FormatUint(v, 10))
		}
	case 0xd0, 0xd1, 0xd2, 0xd3:
		size := 1 << (t - 0xd0)
		v, err := r.uint(size)
		if err != nil {
			return err
		}
		shift := 64 - 8*size
		i := int64(v<<shift) >> shift
		if i > maxSafeInteger || i < -maxSafeInteger {
			writeJSONString(out, strconv.FormatInt(i, 10))
		} else {
			out.WriteString(strconv.FormatInt(i, 10))
		}
	case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8:
		return r.ext(out, 1<<(t-0xd4), depth)
	case 0xd9, 0xda, 0xdb:
		n, err := r.uint(1 << (t - 0xd9))
		if err != nil {
			return err
		}
		return r.str(out, int(n))
	case 0xdc, 0xdd:
		n, err := r.uint(2 << (t - 0xdc))
		if err != nil {
			return err
		}
		return r.arrayBody(out, int(n), depth)
	case 0xde, 0xdf:
		n, err := r.uint(2 << (t - 0xde))
		if err != nil {
			return err
		}
		return r.mapBody(out, int(n), depth)
	default:
		return r.errf("type byte 0x%02x", t)
	}
	return nil
}

func (r *mpReader) str(out *bytes.Buffer, n int) error {
	s, err := r.take(n)
	if err != nil {
		return err
	}
	writeJSONString(out, string(s))
	return nil
}

// ext reads an extension of n data bytes. Type 1 wraps a value (a 64-bit id);
// any other type, never seen from MAX, reads as null.
func (r *mpReader) ext(out *bytes.Buffer, n, depth int) error {
	tb, err := r.take(1)
	if err != nil {
		return err
	}
	data, err := r.take(n)
	if err != nil {
		return err
	}
	if int8(tb[0]) != extWrappedNumber {
		out.WriteString("null")
		return nil
	}
	inner := &mpReader{b: data}
	return inner.value(out, depth+1)
}

func (r *mpReader) arrayBody(out *bytes.Buffer, n, depth int) error {
	if n > len(r.b)-r.p {
		return r.errf("an array of %d items in %d bytes", n, len(r.b)-r.p)
	}
	out.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		if err := r.value(out, depth+1); err != nil {
			return err
		}
	}
	out.WriteByte(']')
	return nil
}

// mapBody reads n pairs. A key is a string in JSON: MAX keys maps by chat and
// message ids as often as by names.
func (r *mpReader) mapBody(out *bytes.Buffer, n, depth int) error {
	if n > len(r.b)-r.p {
		return r.errf("a map of %d pairs in %d bytes", n, len(r.b)-r.p)
	}
	out.WriteByte('{')
	var key bytes.Buffer
	for i := 0; i < n; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		key.Reset()
		if err := r.value(&key, depth+1); err != nil {
			return err
		}
		k := key.Bytes()
		switch {
		case len(k) > 0 && k[0] == '"':
			out.Write(k)
		case len(k) > 0 && (k[0] == '{' || k[0] == '['):
			return r.errf("a map key that is a map or an array")
		default: // a number, true, false or null
			writeJSONString(out, string(k))
		}
		out.WriteByte(':')
		if err := r.value(out, depth+1); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

func writeJSONString(out *bytes.Buffer, s string) {
	if !utf8.ValidString(s) {
		s = string(bytes.ToValidUTF8([]byte(s), []byte("�")))
	}
	b, _ := json.Marshal(s)
	out.Write(b)
}

func writeJSONFloat(out *bytes.Buffer, f float64) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		out.WriteString("null")
		return
	}
	out.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
}

// --- JSON → MessagePack ---

// jsonToMsgpack transcodes a JSON value, keeping the order of object keys.
// A whole number goes out as the smallest integer that holds it (a time or
// an id past 32 bits as uint64, as the web client sends "from"; MAX refuses
// a time sent as a float), any other number as float64. An empty payload
// (none, null, {}) is an empty body.
func jsonToMsgpack(payload []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var out bytes.Buffer
	if err := jsonValue(dec, &out, 0); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("maxclient: a payload of more than one JSON value")
	}
	return out.Bytes(), nil
}

func jsonValue(dec *json.Decoder, out *bytes.Buffer, depth int) error {
	if depth > maxMsgpackDepth {
		return errors.New("maxclient: a payload nested too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case nil:
		out.WriteByte(0xc0)
	case bool:
		if v {
			out.WriteByte(0xc3)
		} else {
			out.WriteByte(0xc2)
		}
	case string:
		writeMsgpackString(out, v)
	case json.Number:
		writeMsgpackNumber(out, v)
	case json.Delim:
		var body bytes.Buffer
		n := 0
		switch v {
		case '{':
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				writeMsgpackString(&body, k)
				if err := jsonValue(dec, &body, depth+1); err != nil {
					return err
				}
				n++
			}
			if _, err := dec.Token(); err != nil { // '}'
				return err
			}
			writeMsgpackLen(out, n, 0x80, 0xde)
		case '[':
			for dec.More() {
				if err := jsonValue(dec, &body, depth+1); err != nil {
					return err
				}
				n++
			}
			if _, err := dec.Token(); err != nil { // ']'
				return err
			}
			writeMsgpackLen(out, n, 0x90, 0xdc)
		default:
			return fmt.Errorf("maxclient: unexpected JSON %q", v)
		}
		out.Write(body.Bytes())
	}
	return nil
}

// writeMsgpackLen writes a map or array header: fix (n < 16), 16 or 32 bit.
func writeMsgpackLen(out *bytes.Buffer, n int, fix, b16 byte) {
	switch {
	case n < 16:
		out.WriteByte(fix | byte(n))
	case n <= math.MaxUint16:
		out.WriteByte(b16)
		_ = binary.Write(out, binary.BigEndian, uint16(n))
	default:
		out.WriteByte(b16 + 1)
		_ = binary.Write(out, binary.BigEndian, uint32(n))
	}
}

func writeMsgpackString(out *bytes.Buffer, s string) {
	n := len(s)
	switch {
	case n < 32:
		out.WriteByte(0xa0 | byte(n))
	case n <= math.MaxUint8:
		out.WriteByte(0xd9)
		out.WriteByte(byte(n))
	case n <= math.MaxUint16:
		out.WriteByte(0xda)
		_ = binary.Write(out, binary.BigEndian, uint16(n))
	default:
		out.WriteByte(0xdb)
		_ = binary.Write(out, binary.BigEndian, uint32(n))
	}
	out.WriteString(s)
}

func writeMsgpackNumber(out *bytes.Buffer, n json.Number) {
	s := string(n)
	if !bytes.ContainsAny([]byte(s), ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			writeMsgpackInt(out, i)
			return
		}
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			out.WriteByte(0xcf)
			_ = binary.Write(out, binary.BigEndian, u)
			return
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	out.WriteByte(0xcb)
	_ = binary.Write(out, binary.BigEndian, math.Float64bits(f))
}

func writeMsgpackInt(out *bytes.Buffer, i int64) {
	switch {
	case i >= 0 && i <= 0x7f:
		out.WriteByte(byte(i))
	case i >= 0 && i <= math.MaxUint8:
		out.WriteByte(0xcc)
		out.WriteByte(byte(i))
	case i >= 0 && i <= math.MaxUint16:
		out.WriteByte(0xcd)
		_ = binary.Write(out, binary.BigEndian, uint16(i))
	case i >= 0 && i <= math.MaxUint32:
		out.WriteByte(0xce)
		_ = binary.Write(out, binary.BigEndian, uint32(i))
	case i >= 0:
		out.WriteByte(0xcf)
		_ = binary.Write(out, binary.BigEndian, uint64(i))
	case i >= -32:
		out.WriteByte(byte(int8(i)))
	case i >= math.MinInt8:
		out.WriteByte(0xd0)
		out.WriteByte(byte(int8(i)))
	case i >= math.MinInt16:
		out.WriteByte(0xd1)
		_ = binary.Write(out, binary.BigEndian, int16(i))
	case i >= math.MinInt32:
		out.WriteByte(0xd2)
		_ = binary.Write(out, binary.BigEndian, int32(i))
	default:
		out.WriteByte(0xd3)
		_ = binary.Write(out, binary.BigEndian, i)
	}
}
