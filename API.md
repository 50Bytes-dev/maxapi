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

## Session / Auth Endpoints

### Start QR Auth Session
Open a WebSocket to MAX and request a QR auth session.

```http
POST /session/auth/qr/start
```

Response:
```json
{
    "success": true,
    "qrLink": "https://max.ru/:auth/6bc2adb2-6b23-4a7d-a96c-320bee4ed0d7",
    "qrCodeBase64": "data:image/png;base64,...",
    "trackId": "6bc2adb2-6b23-4a7d-a96c-320bee4ed0d7",
    "pollingInterval": 5000,
    "ttl": 120000,
    "expiresAt": 1776435251750
}
```

Render `qrCodeBase64` directly, or regenerate a QR image from `qrLink`. Scan it with the MAX mobile
app (Settings → Devices → Scan QR code).

### Poll QR Auth Status
Check whether the QR has been scanned and, once it has, receive the auth token.

```http
GET /session/auth/qr/status
```

While waiting:
```json
{ "success": true, "status": "pending" }
```

After the user confirms on their phone — the server also persists the token:
```json
{
    "success": true,
    "status": "authorized",
    "authToken": "permanent_auth_token"
}
```

If the session has expired without being scanned:
```json
{ "success": true, "status": "expired" }
```

Clients are expected to poll on the cadence indicated by `pollingInterval` from
`/session/auth/qr/start` (5 seconds by default).

### Cancel QR Auth Session
Close an in-progress QR session (e.g. the user navigated away from the login screen).

```http
POST /session/auth/qr/cancel
```

Response:
```json
{ "success": true, "message": "QR session cancelled" }
```

### Connect
Connect to MAX with saved auth token.

```http
POST /session/connect
Content-Type: application/json

{
    "subscribe": ["Message", "ReadReceipt", "Connected"],
    "immediate": false  // if true, returns immediately
}
```

Response:
```json
{ 
    "success": true,
    "message": "Connected to MAX"
}
```

### Disconnect
Disconnect from MAX (keeps auth token).

```http
POST /session/disconnect
```

### Logout
Logout and clear auth token.

```http
POST /session/logout
```

### Get Status
Get connection status.

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
| `QRGenerated` | QR auth session created |
| `QRScanned` | User scanned QR in the mobile app |
| `QRAuthorized` | Auth token received |
| `QRExpired` | QR session expired before scan |
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
  }
}
```

---

## Error Responses

All error responses follow this format:

```json
{
    "success": false,
    "error": "Error description"
}
```

Common HTTP status codes:
- `400` - Bad Request (invalid parameters)
- `401` - Unauthorized (invalid token)
- `404` - Not Found
- `409` - Conflict (e.g., already connected)
- `500` - Internal Server Error
- `503` - Service Unavailable (not connected)
