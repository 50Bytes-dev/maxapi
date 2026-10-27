# MaxAPI - REST API for MAX Messenger

MaxAPI is a multi-tenant REST API gateway for MAX Messenger, providing a simple HTTP interface to interact with the MAX protocol.

## Authentication

### Admin Token
Used for user management operations. Set via `--admintoken` flag or `MAXAPI_ADMIN_TOKEN` environment variable.

```
Authorization: <admin_token>
```

### User Token
Used for all other operations. Each user has a unique token.

```
Header: token: <user_token>
```

---

## Session Endpoints

### Connect
Open a MAX session. Behaviour depends on whether the user already has a stored
auth token:

- **No auth token** → the proxy starts a QR auth session server-side and
  responds with `{"success": true, "details": "Awaiting QR scan"}`. Fetch the
  rendered code via `GET /session/qr`, scan it in the MAX mobile app, then
  consume the `QRAuthorized` + `Sync` webhooks (the proxy reconnects
  automatically after authorisation).
- **Auth token present** → the proxy reconnects with the saved credentials and
  responds with `{"success": true, "details": "Connected to MAX"}`.
- **Already connected** → idempotent; subscriptions are still refreshed and the
  response carries `"alreadyConnected": true` plus the
  [account pause](#account-pause): `paused` always, `pauseReason` and
  `upstreamCode` while paused.

```json
{
    "success": true,
    "details": "Already connected to MAX",
    "events": "Sync,AuthExpired,LoggedOut",
    "alreadyConnected": true,
    "paused": true,
    "pauseReason": "spam",
    "upstreamCode": "error.user.restricted.send"
}
```

```http
POST /session/connect
Content-Type: application/json

{
    "subscribe": ["All"],
    "immediate": true
}
```

### Get QR Code
Returns the QR code for an in-progress auth session. The proxy refreshes the
code automatically when MAX's TTL elapses — poll this endpoint every few
seconds (or subscribe to the `QRGenerated` webhook) to keep the displayed
image current.

```http
GET /session/qr
```

Response:
```json
{ "success": true, "qrcode": "data:image/png;base64,..." }
```

Errors: `500` `"no session"` when no QR session is active, `500` `"already
logged in"` once the user is fully authorised.

### Disconnect
Closes an active MAX connection or cancels an in-progress QR auth session.
Keeps the stored auth token.

```http
POST /session/disconnect
```

### Logout
Logout and remove the user entirely: the MAX session is ended (opcode 20),
the reconnect loop stops, and the user's cached phone lookups are deleted
with it. A [pause](#account-pause) of its MAX account stays. No webhook is
sent for a logout requested here.

```http
POST /session/logout
```

### Get Status
Connection / authentication status.

```http
GET /session/status
```

Response:
```json
{
    "success": true,
    "connected": true,
    "authenticated": true,
    "loggedIn": true,
    "maxUserID": 123456789,
    "paused": true,
    "pauseReason": "spam",
    "upstreamCode": "error.user.restricted.send"
}
```

| Field | Present | Meaning |
|-------|---------|---------|
| `connected` | always | The MAX WebSocket of this user is up. A paused account keeps its session and stays `connected: true`. |
| `authenticated` | always | An auth token is stored. |
| `loggedIn` | always | `connected && authenticated`. |
| `maxUserID` | always | The MAX id of the logged-in account; `0` while the session is down. |
| `paused` | always | Whether the MAX account this user is logged into is [paused](#account-pause): the live session's account, or without a session the one stored at the last login. **Read it before `connected`.** |
| `pauseReason` | only while paused | Always `spam`. |
| `upstreamCode` | only while paused | The MAX error code that paused the account. |

### Resume
Lifts the [pause](#account-pause) of the MAX account this user is logged
into (the live session's, or the one stored at the last login). The body is
optional: no body, `{}` and `{"captchaKey": "..."}` all work; `captchaKey`
belongs to the VK gateway's contract and is ignored (a MAX pause has no
captcha).

```http
POST /session/resume
Content-Type: application/json

{}
```

Response `200`:
```json
{ "success": true, "maxUserID": 123456789 }
```

- The stored pause is deleted first, then the one in memory: if the delete
  fails, the account stays paused and the answer is `500` `INTERNAL_ERROR`.
- The pause is the MAX account's, so it is lifted for every gateway user
  logged into it. `maxUserID` names that account, so that the caller resumes
  all its connections of it.
- An account that is not paused answers `200` as well; nothing changes. So
  does a user whose MAX account is unknown (`maxUserID: 0`).
- No MAX call is made. The session state does not matter.

Errors: `400` `INVALID_INPUT` (the body is not JSON), `500` `INTERNAL_ERROR`
(database error; the account stays paused), `401` (unknown token).

### Reconnect
A user with a stored auth token is kept connected by a reconnect loop (after
`POST /session/connect`, after a QR login, at startup, after
`POST /session/sync`). A dropped session is logged into again after a backoff
that doubles from `MAX_CONNECT_DELAY` up to `MAX_CONNECT_CAP` seconds, spread
by ±20% so that accounts dropped together do not log in at the same moment.

- **Network failures on the way to the login** (no connection, a failed
  handshake, SessionInit unanswered) are retried with that backoff, up to
  `MAX_CONNECT_RETRIES` (`0` = forever), then `ReconnectExhausted`.
- **An auth error** (`login.token`, `login.expired`, `auth.invalid`,
  `auth.expired`) stops the loop at once and **erases the stored auth
  token**: the account needs a new QR login.
- **A refusal 3 times in a row** stops the loop too: logging in again and
  again after MAX ended a session is what gets an account blocked. A refusal
  is any other error code of MAX's own (on SessionInit or on the login), or
  the login (opcode 19) left unanswered or losing its socket once the
  connection and SessionInit went through, as MAX may refuse that way too.
  Network failures in between neither count nor reset the count; a
  successful login resets it. **The stored token is kept**: a partial MAX
  failure would otherwise erase every account's token within minutes and
  send every owner through a new QR login, itself a risk signal. The user is
  marked disconnected, and the next attempt waits for
  `POST /session/connect`, that is for the owner.

Both stops close the session and send `AuthExpired` with the reason in
`reason`: the error text for an auth error; for refusals MAX's error code,
`"login timed out"` or `"login connection lost"`.

---

## Message Endpoints

### Send Text Message

```http
POST /chat/send/text
Content-Type: application/json

{
    "chatId": 123456789,  // or use "phone"
    "phone": "+79001234567",  // alternative to chatId
    "text": "Hello, World!",
    "replyTo": 987654321,  // optional, message ID to reply to
    "notify": true
}
```

Response: 
```json
{
    "success": true,
    "messageId": 111222333,
    "chatId": 123456789
}
```

One of `chatId` or `phone` is required. When both are set, `chatId` wins.
`chatId: 0` is not a recipient: a request without one is rejected with
`invalid_target` instead of landing in the account's own "Favorites" chat.

The endpoint answers within ~9.5 s: the phone lookup waits at most 4 s for
MAX, the send at most 5 s for MAX to acknowledge it, and each database call
of the lookup cache and the pause at most 200 ms. A caller with a 10 s read
timeout therefore always gets an outcome code.

Errors use the [error envelope](#error-responses) with a delivery outcome
`code` that says whether the text may have reached the recipient:

| HTTP | `code` | Meaning | Sent? |
|------|--------|---------|-------|
| `503` | `not_connected` | No live MAX session for this user | No |
| `400` | `invalid_target` | Neither `chatId` nor `phone` given | No |
| `400` | `INVALID_INPUT` | Body is not valid JSON | No |
| `422` | `send_rejected` | The MAX account is [paused](#account-pause): no MAX call was made. `paused: true`, `pauseReason: "spam"`, no `upstreamCode` | No |
| `404` | `address_not_found` | `phone` is malformed, or MAX has no user with it: MAX answered the lookup without a usable contact or with its `not.found` error (`upstreamCode: "not.found"`), or the [lookup cache](#phone-lookup-cache) holds a `not.found` younger than 30 days (no MAX call, no `upstreamCode`) | No |
| `503` | `lookup_failed` | Phone lookup timed out, the connection failed, or MAX answered it with an error other than `not.found`. A limit or restriction error also pauses the account | No |
| `422` | `send_rejected` | MAX answered the send with an error. A limit or restriction error also pauses the account; any other error on a send to a cached contact drops it from the cache | No |
| `500` | `send_failed` | No answer to the send in time, or the session was lost after the frame went out | Unknown — do not resend automatically |

When MAX itself answered with an error (`send_rejected`, `lookup_failed`
caused by a MAX error, and `address_not_found` for `not.found`), the body also
carries MAX's code as `upstreamCode`:

```json
{
    "success": false,
    "error": "send rejected: Refused: Refused (chat.denied)",
    "code": "send_rejected",
    "upstreamCode": "chat.denied"
}
```

The checks run in this order: session, body, target, pause, lookup (cache,
then MAX), send.

#### Phone lookup cache

A send by `phone` needs the recipient's MAX id, which MAX gives out for the
phone (opcode 46). Repeated lookups, above all of numbers MAX does not know,
are what MAX limits and then restricts accounts for, so the gateway keeps
MAX's answer in its database, per gateway user:

- **Found**: the contact's MAX id, for 90 days (phone numbers are reissued).
  Sends to the number within that time make no lookup. If MAX refuses a send
  to a cached id with an error of its own (other than a
  [pause](#account-pause) code), the entry is dropped and the next send looks
  the number up again.
- **Not found**: only MAX's explicit `not.found`, for 30 days. A send to the
  number answers `404` `address_not_found` without asking MAX. After 30 days
  the number is looked up again.
- Nothing else is cached: an answer without a usable contact (still `404`), a
  malformed number, a timeout, a lost connection or any other MAX error
  leaves the next send to look the number up again.

The key is the phone exactly as sent in `phone` (`+79001234567` and
`79001234567` are different keys). The cache is deleted with the user. Each
cache read or write waits at most 200 ms for the database, so the endpoint
still answers within ~9.5 s.

#### Account pause

MAX answers an account that ran into one of its limits, or that it
restricted, with an error whose code says so. Going on after that turns a
temporary restriction into a block, so the gateway stops sending from the
account until the owner resumes it. The contract is the one of the VK
gateway's page pause.

- **Trigger:** a MAX error on the phone lookup (opcode 46) or on the send
  (opcode 64) whose code contains, in any case, `restricted`, `limit`,
  `too.many`, `flood` or `spam` (for example `error.user.restricted.send`,
  `too.many.requests`, `error.limit.violate`).
- **The request MAX refused** answers by its stage, `503` `lookup_failed` or
  `422` `send_rejected`, plus `paused: true`, `pauseReason: "spam"` and MAX's
  code in `upstreamCode`:

  ```json
  {
      "success": false,
      "error": "send rejected: ... (error.user.restricted.send)",
      "code": "send_rejected",
      "upstreamCode": "error.user.restricted.send",
      "paused": true,
      "pauseReason": "spam"
  }
  ```

- **The pause belongs to the MAX account** (its MAX user id), not to the
  gateway user, like the VK gateway's page pause. Every gateway user logged
  into the account is paused, and the pause outlives them: it survives the
  deletion of the gateway user (`LoggedOut`, `/session/logout`,
  `DELETE /admin/users/{id}`), a new QR login into the same account through a
  new gateway user, and a restart (it is restored before any account
  reconnects). It is stored before the answer goes out. A further refusal of
  a paused account keeps the first pause.
- **While paused** `POST /chat/send/text` of any gateway user of the account
  makes no MAX call, neither lookup nor send, and answers `422`
  `send_rejected` with `paused: true` and `pauseReason: "spam"`, without
  `upstreamCode`.
- [`GET /session/status`](#get-status) and an already connected
  [`POST /session/connect`](#connect) report `paused`, and while paused
  `pauseReason` and `upstreamCode`. The account is the live session's, or
  without a session the one stored at the user's last login. The session
  itself stays up.
- Only [`POST /session/resume`](#resume) ends it, through any gateway user of
  the account.
- Only `POST /chat/send/text` is guarded; the media endpoints and
  `/user/check` are not.

The codes are lowercase because they follow a delivery contract shared with
other gateways. That contract also reserves `address_refused` (`403`, the
recipient's privacy settings or a block). MAX's error code for that case is not
known, so maxapi does not emit it yet; such refusals arrive as `send_rejected`
or `lookup_failed` with MAX's code in `upstreamCode`.

### Send Image

```http
POST /chat/send/image
Content-Type: application/json

{
    "chatId": 123456789,
    "image": "base64_encoded_image_or_url",
    "caption": "Image caption",
    "notify": true
}
```

### Send Document

```http
POST /chat/send/document
Content-Type: application/json

{
    "chatId": 123456789,
    "document": "base64_encoded_file_or_url",
    "fileName": "document.pdf",
    "caption": "Document caption",
    "notify": true
}
```

### Send Audio

```http
POST /chat/send/audio
Content-Type: application/json

{
    "chatId": 123456789,
    "audio": "base64_encoded_audio_or_url",
    "fileName": "audio.mp3",
    "notify": true
}
```

### Send Video

```http
POST /chat/send/video
Content-Type: application/json

{
    "chatId": 123456789,
    "video": "base64_encoded_video_or_url",
    "caption": "Video caption",
    "fileName": "video.mp4",
    "notify": true
}
```

### Edit Message

```http
POST /chat/send/edit
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": 111222333,
    "text": "Updated message text"
}
```

### Delete Messages

```http
POST /chat/delete
Content-Type: application/json

{
    "chatId": 123456789,
    "messageIds": [111222333, 111222334],
    "forMe": false  // if true, deletes only for you
}
```

### Mark as Read

```http
POST /chat/markread
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": 111222333
}
```

### Get Chat History

```http
POST /chat/history
Content-Type: application/json

{
    "chatId": 123456789,
    "count": 50,
    "fromTime": 1699999999999  // optional, milliseconds timestamp
}
```

Response:
```json
{
    "success": true,
    "messages": [
        {
            "id": 111222333,
            "chatId": 123456789,
            "sender": 987654321,
            "text": "Message text",
            "time": 1699999999999,
            "type": "TEXT"
        }
    ]
}
```

### Add Reaction

```http
POST /chat/react
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": "111222333",
    "reaction": "👍"  // empty string to remove reaction
}
```

---

## Media Download Endpoints

### Download Image

```http
POST /chat/downloadimage
Content-Type: application/json

{
    "url": "https://example.com/image.jpg"
}
```

### Download Video

```http
POST /chat/downloadvideo
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": 111222333,
    "videoId": 555666777
}
```

### Download Document

```http
POST /chat/downloaddocument
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": 111222333,
    "fileId": 555666777
}
```

### Download Audio

```http
POST /chat/downloadaudio
Content-Type: application/json

{
    "chatId": 123456789,
    "messageId": 111222333,
    "fileId": 555666777
}
```

---

## User Endpoints

### Check User by Phone

```http
POST /user/check
Content-Type: application/json

{
    "phone": ["+79001234567", "+79007654321"]
}
```

Response:
```json
{
    "success": true,
    "users": [
        {
            "phone": "+79001234567",
            "exists": true,
            "maxUserId": 123456789,
            "name": "John Doe"
        },
        {
            "phone": "+79007654321",
            "exists": false,
            "maxUserId": 0,
            "error": "Timeout Error: Request timed out (timeout)"
        }
    ]
}
```

`exists: false` without `error` means MAX answered and has no such user (no
contact, or its `not.found` error). With `error` the lookup itself failed
(malformed number, timeout, connection or another MAX error), so whether the
user exists is unknown. This endpoint always asks MAX: it neither uses nor
fills the [lookup cache](#phone-lookup-cache), and it does not pause the
account.

### Get User Info

```http
POST /user/info
Content-Type: application/json

{
    "userId": 123456789
}
```

### Get User Avatar

```http
POST /user/avatar
Content-Type: application/json

{
    "userId": 123456789
}
```

Response: 
```json
{
    "success": true,
    "avatarUrl": "https://..."
}
```

### Send Typing Indicator

```http
POST /user/presence
Content-Type: application/json

{
    "chatId": 123456789
}
```

---

## Group Endpoints

### Create Group

```http
POST /group/create
Content-Type: application/json

{
    "name": "My Group",
    "participants": [123456789, 987654321]
}
```

### List Groups

```http
GET /group/list
```

### Get Group Info

```http
POST /group/info
Content-Type: application/json

{
    "chatId": 123456789
}
```

### Get Invite Link

```http
POST /group/invitelink
Content-Type: application/json

{
    "chatId": 123456789
}
```

### Join Group

```http
POST /group/join
Content-Type: application/json

{
    "link": "https://max.ru/join/abc123"
}
```

### Leave Group

```http
POST /group/leave
Content-Type: application/json

{
    "chatId": 123456789
}
```

### Update Participants

```http
POST /group/updateparticipants
Content-Type: application/json

{
    "chatId": 123456789,
    "userIds": [111222333],
    "operation": "add"  // or "remove"
}
```

### Set Group Name

```http
POST /group/name
Content-Type: application/json

{
    "chatId": 123456789,
    "name": "New Group Name"
}
```

### Set Group Topic

```http
POST /group/topic
Content-Type: application/json

{
    "chatId": 123456789,
    "topic": "Group description"
}
```

---

## Webhook Endpoints

### Set Webhook

```http
POST /webhook
Content-Type: application/json

{
    "webhook": "https://your-server.com/webhook"
}
```

### Get Webhook

```http
GET /webhook
```

### Delete Webhook

```http
DELETE /webhook
```

---

## Admin Endpoints

All admin endpoints require the admin token in the `Authorization` header.

### List Users

```http
GET /admin/users
Authorization: <admin_token>
```

### Get User by ID

Returns full details for a single user. Secrets are never returned in cleartext:

- `auth_token` → exposed only as `auth_configured: bool`
- `s3_access_key` / `s3_secret_key` → omitted
- `proxy_url` → password component masked as `***`; `proxy_configured: bool`
  tells you whether any proxy is set

```http
GET /admin/users/{userid}
Authorization: <admin_token>
```

Response:
```json
{
    "success": true,
    "data": {
        "id": "a1b2c3d4-...",
        "name": "Acme Bot",
        "token": "<user_token>",
        "webhook": "https://example.com/webhook",
        "max_user_id": 123456789,
        "device_id": "uuid-...",
        "connected": true,
        "auth_configured": true,
        "events": ["Message", "ReadReceipt"],
        "proxy_configured": true,
        "proxy_url": "http://user:***@proxy.internal:3128",
        "history": 100,
        "s3_enabled": false,
        "s3_endpoint": "",
        "s3_region": "",
        "s3_bucket": "",
        "media_delivery": "base64"
    }
}
```

### Create User

```http
POST /admin/users
Authorization: <admin_token>
Content-Type: application/json

{
    "name": "User Name",
    "webhook": "https://...",
    "events": "Message,ReadReceipt"
}
```

### Edit User

```http
PUT /admin/users/{userid}
Authorization: <admin_token>
Content-Type: application/json

{
    "name": "New Name",
    "webhook": "https://...",
    "events": "Message,ReadReceipt,Connected"
}
```

### Delete User

```http
DELETE /admin/users/{userid}
Authorization: <admin_token>
```

---

## Webhook Events

Subscribe to these events via the `subscribe` array in `/session/connect`:

| Event | Description |
|-------|-------------|
| `Message` | New incoming message |
| `MessageEdit` | Message was edited |
| `MessageDelete` | Message was deleted |
| `ReadReceipt` | Messages were read |
| `Connected` | Successfully connected |
| `Disconnected` | Connection lost |
| `Reconnecting` | Reconnect attempt in progress (first + every 10th attempt) |
| `ReconnectExhausted` | Reconnect budget (`MAX_CONNECT_RETRIES`) exhausted; session stopped |
| `QRGenerated` | QR code available (initial or refreshed). Payload: `qrCodeBase64`, `qrLink`, `trackId`, `expiresAt`. `GET /session/qr` returns the same PNG for pull consumers. |
| `QRScanned` | User scanned QR in the mobile app |
| `QRAuthorized` | Auth token received — proxy auto-starts the MAX session (`Sync` follows) |
| `QRExpired` | QR refresh budget exhausted or session cancelled |
| `AuthExpired` | The reconnect loop stopped, see [Reconnect](#reconnect): MAX refused the token with an auth error (the token is erased, a new QR login is needed), or refused the reconnect login 3 times in a row (the token is kept, `POST /session/connect` tries again). `reason` carries the error text, MAX's code, `"login timed out"` or `"login connection lost"` |
| `LoggedOut` | MAX ended the session (opcode 20 pushed by the server): the user is deleted and its reconnect loop stops |
| `ChatUpdate` | Chat was updated |
| `Typing` | User is typing |
| `ReactionChange` | Reaction was changed |
| `ContactUpdate` | Contact was updated |
| `PresenceUpdate` | User presence changed |
| `FileReady` | File upload completed |
| `HistorySync` | History sync completed |
| `All` | All events |

### Webhook Payload Format

Every webhook body (HTTP JSON and RabbitMQ JSON) carries an identity block in
addition to the event-specific `event` payload:

```json
{
    "type": "Message",
    "opcode": 128,
    "event": {
        "chatId": 123456789,
        "message": {
            "id": 111222333,
            "sender": 987654321,
            "text": "Hello!",
            "time": 1699999999999,
            "type": "TEXT"
        }
    },
    "userID": "a1b2c3d4-...",
    "instanceID": "a1b2c3d4-...",
    "instanceName": "My Instance",
    "eventTimestamp": 1700000000
}
```

RabbitMQ payloads additionally include `"serverHost"` (the hostname of the
MaxAPI instance that emitted the event), and — when `RABBITMQ_EXCHANGE` is
configured — publish with routing key = event type, headers
`x-instance-id` and `x-event-type`, `ContentType: application/json`,
`DeliveryMode: persistent`, `MessageId: <uuid>`, `Timestamp: <now>`.

**The webhook payload never contains the user API `token`.** Identify the
MaxAPI user by `instanceID` and authenticate your receiver via your own
mechanism (shared secret in the `Authorization` header, HMAC over the body,
mTLS, …).

### Webhook Delivery Guarantees

The HTTP client retries delivery with exponential backoff:

- Triggers: network errors, HTTP `5xx`, `408`, `429`.
- `4xx` is treated as a client contract failure and is delivered exactly once.
- Backoff: `WEBHOOK_RETRY_DELAY × 2^(attempt-1)` seconds, up to
  `WEBHOOK_RETRY_COUNT` attempts. Defaults: 5 attempts starting at 30 s.

Multipart webhooks (file attachments) share the same policy and are additionally
guarded by a per-user concurrency semaphore (`MAXAPI_USER_CONCURRENCY`) so a
slow subscriber cannot consume unbounded file descriptors for a single user.

### RabbitMQ Delivery Guarantees

- Persistent messages (`DeliveryMode: persistent`) with durable queue.
- Transparent reconnect on broker restart (`RABBITMQ_RETRY_COUNT` /
  `RABBITMQ_RETRY_DELAY`). Publishes during reconnect are dropped at the
  publisher with a warning log.
- Optional topic exchange — set `RABBITMQ_EXCHANGE` to bind consumers to
  specific event types via routing key.

---

## Health

### Get Health

Unauthenticated liveness + readiness endpoint for orchestrators.

```http
GET /health
```

Response (`200 OK` when healthy, `503 Service Unavailable` when DB is down):

```json
{
    "success": true,
    "status": "ok",
    "version": "1.5.0",
    "uptime_seconds": 12345,
    "db_ok": true,
    "rabbitmq_ok": true,
    "connected_users": 42,
    "goroutines": 87,
    "mem_alloc_mb": 67,
    "timestamp": 1700000000
}
```

---

## Error Responses

All error responses follow this format:

```json
{
    "success": false,
    "error": "human-readable description",
    "code": "MACHINE_CODE"
}
```

`code` is a stable machine-readable constant that clients can branch on; the
`error` text is free-form and may evolve. `POST /chat/send/text` uses its own
lowercase delivery outcome codes, see [Send Text Message](#send-text-message).
Other known codes:

| Code | Meaning |
|------|---------|
| `INVALID_INPUT` | Malformed payload, missing required field, bad parameter |
| `UNAUTHORIZED` | Missing/invalid admin or user token |
| `FORBIDDEN` | Authenticated but not allowed |
| `NOT_FOUND` | Resource does not exist |
| `NOT_CONNECTED` | User has no active MAX session |
| `AUTH_EXPIRED` | Stored auth token is no longer valid — re-authenticate |
| `INTERNAL_ERROR` | Server-side failure |

Common HTTP status codes:
- `400` - Bad Request (invalid parameters)
- `401` - Unauthorized (invalid token)
- `404` - Not Found
- `409` - Conflict (e.g., already connected)
- `500` - Internal Server Error
- `503` - Service Unavailable (not connected)
