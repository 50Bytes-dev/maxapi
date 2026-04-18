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
  response carries `"alreadyConnected": true`.

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
Logout and remove the user entirely.

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
    "maxUserID": 123456789
}
```

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
        }
    ]
}
```

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
| `AuthExpired` | Stored auth token is no longer valid |
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
    "version": "2.0.0-max",
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
`error` text is free-form and may evolve. Known codes:

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
