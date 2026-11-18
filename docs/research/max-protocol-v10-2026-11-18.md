> Research note of 2026-11-18 for maxapi 3.0.1: MAX switched JSON v11 off,
> the gateway moved to the binary protocol v10.

# MAX: the binary protocol v10

## 1. What happened

- 2026-11-18 14:20:53 MSK: MAX closed every WebSocket of the production
  gateway at once (`close 1006`). From 14:21 `wss://ws-api.oneme.ru/websocket`
  (155.212.204.70 and .133, port 443) timed out on connect (`dial tcp … i/o
  timeout`), from another machine too; `api.oneme.ru` and `web.max.ru`
  answered. Not an IP ban: the JSON endpoint is gone.
- All 79 production accounts lost their session; no message went out from
  14:21; QR logins failed; `POST /media/photo` and `POST /chat/send/text`
  answered `503`.
- Probed without an account: SESSION_INIT (opcode 6) with the old payload
  in JSON v11 on `api.oneme.ru` is closed at once with 1000; in v10 MAX
  answers `cmd 1` with an LZ4 body.

## 2. Sources

- MAX's web client, `web.max.ru`, `appVersion` 26.11.9 (2026-11-18):
  `wss://api.oneme.ru/websocket`, `binaryType = arraybuffer`, MessagePack
  with extension type 1 (`useBigInt64`).
- [MaxApiTeam/PyMax](https://github.com/MaxApiTeam/PyMax) ≥ 2.5.0 (MIT):
  `src/pymax/protocol/tcp/{framing,payload,compression}.py`. Sends plain
  bodies (its compression is commented out) and ids as plain integers.
- [WireCatLabs/max-cli](https://github.com/WireCatLabs/max-cli)
  (Apache-2.0): `src/protocol/frame.ts`, `lz4.ts`, `connection.ts`, and a
  capture of web.max.ru of 2026-09-25
  (`src/testing/fixtures/web-capture-2026-09-25.json`). Read, not copied:
  maxapi's codec is written anew in Go.

## 3. The frame

| Bytes | Field |
|---|---|
| 0 | `ver` = 10 |
| 1 | `cmd`: 0 a request or a push, 1 an answer, 3 an error answer (max-cli also names 2, an event, never seen) |
| 2–3 | `seq`, big-endian u16 (MAX parses the client's as int16: the gateway keeps it ≤ 32767) |
| 4–5 | opcode, u16 |
| 6 | compression: 0 none; 1..0x7f an LZ4 block, the value being how many times the body grew, rounded up; 0xff zstd (never seen) |
| 7–9 | body length, u24 |

The body is one MessagePack value, the payload; an empty body is no
payload (the web client's ping answer). MAX wraps 64-bit ids in extension
type 1 whose data is a MessagePack integer; map keys may be integers (ids).
The web client compresses bodies of 32 bytes and more; MAX takes plain ones
(PyMax). A time must be an integer: a float time fails LOGIN with
`proto.payload` (max-cli, 2026-09-25).

MAX pushes its own ping (opcode 1, `cmd 0`); the web client answers it
(`cmd 1`, same `seq`, empty body), and answers a new message (128) with
`{chatId, messageId}`.

## 4. How the gateway does it (`maxclient/codec.go`)

The rest of the client keeps speaking JSON. A request's payload is
marshalled to JSON and transcoded to MessagePack (key order kept; whole
numbers as the smallest integer, a time past 32 bits as uint64; other
numbers float64; `{}` as an empty body); the frame goes out plain, as one
binary WebSocket frame. An incoming body is inflated (LZ4, at most 32 MiB)
and transcoded to JSON: every integer digit kept, extension 1 unwrapped,
integer keys as strings, bin as base64. Unreadable answers fail their
request at once. Ids go out as plain integers, as PyMax sends them (the web
client wraps `chatId` in extension 1); message ids the client keeps as
strings (reactions) go as numbers.

**Integers past 2^53 read as JSON strings.** MAX wraps every 64-bit value
in extension 1, times and small ids too (live: chat `id`, `owner`, `cid`,
`created`, `lastEventTime`, `photoId`, profile `id`, `phone`, LOGIN `time`
and `chatMarker`). Of them only message ids (`lastMessage.id`,
`prevMessageId`, ~1.18e17) pass 2^53; JSON v11 sent those as strings. The
first build read them as numbers: the chat list failed ("cannot unmarshal
number into … Chat.lastMessage.id of type string", `502 upstream_error`),
and the raw payloads of webhooks would have carried message ids as rounded
floats. Writing integers beyond ±2^53 as strings gives the gateway, and
webhooks, what v11 gave.

## 5. Live checks

Run of 2026-11-18, 19:24–19:44 (UTC+4), test account 559563326 ("Max"), a
local gateway built from this tree (`127.0.0.1:5578`, its own SQLite).

| Check | Result |
|---|---|
| SESSION_INIT (6), no account | answered; MAX's answer LZ4-compressed |
| QR start (288), status (289) | `ttl` 119995, a new QR every 2 min, `track.not.found` after it as before |
| Scan → 291 | `payloadKeys ["passwordChallenge","tokenAttrs"]` (the account has a cloud password), `QRPasswordRequired` |
| Cloud password (115) | `200 accepted`; LOGIN (19) at once: profile 559563326, 6 chats; `QRAuthorized`, `Sync` |
| Saved token | the gateway restarted, logged in with the stored token and device id (19) without a QR; also from a second process |
| Chat list (53) | after the fix: 5 chats, `lastMessageId` strings with every digit |
| Send text (64) | `messageId` "117638205992225319" in "Maxapi group" |
| Photo (80 + upload) | 128×128 PNG uploaded, sent with text in one message ("117638206401950520") |
| History (49), delete for everyone (66) | the two messages read back with string ids, the text one deleted (`deleted`) |
| Logout (20) | the session ended |
