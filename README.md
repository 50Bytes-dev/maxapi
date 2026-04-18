# MaxAPI

MaxAPI is a multi-tenant REST API gateway for MAX Messenger, providing a simple HTTP interface to interact with the MAX protocol. It is based on the architecture of [WuzAPI](https://github.com/asternic/wuzapi).

## Features

- **Multi-tenant architecture**: Support multiple MAX accounts on a single server
- **QR Authentication**: Scan a QR code in the MAX mobile app to log in
- **Real-time webhooks**: Receive events via webhooks or RabbitMQ — with automatic retry on 5xx/network errors
- **Durable RabbitMQ delivery**: Persistent messages, transparent reconnect, optional topic exchange with event-type routing keys
- **Media handling**: Upload/download photos, videos, audio, and documents — per-user concurrency limit protects noisy users
- **Group management**: Create, manage, and interact with groups and channels
- **S3 Integration**: Optional media storage in S3-compatible storage
- **SSRF-safe outbound HTTP**: Loopback / RFC1918 / link-local destinations refused by default
- **SQLite WAL by default**: Readers don't block writers; no external DB required for small deployments
- **Health endpoint**: `GET /health` reports DB, RabbitMQ, connected users, memory, uptime

## Key Differences from WhatsApp (WuzAPI)

| Feature | WhatsApp (WuzAPI) | MAX (MaxAPI) |
|---------|-------------------|--------------|
| Authentication | QR Code | QR Code |
| User ID | JID (`phone@s.whatsapp.net`) | Numeric `int64` |
| Dialog ID | Automatic | `user1_id XOR user2_id` |
| Group Creation | Separate API | MSG_SEND with CONTROL attachment |
| Avatar | Separate API | Direct URL in User object |
 
## Prerequisites

- Go 1.21 or later
- PostgreSQL (optional, SQLite by default)
- Docker (optional, for containerization)

## Building

```bash
go build .
```

## Docker Building

```bash
docker build --platform linux/amd64 -t maxapi .
```

## Running

```bash
./maxapi -address=0.0.0.0 -port=5555
```

### Command Line Options

| Option | Description | Default |
|--------|-------------|---------|
| `-address` | IP address to bind | `0.0.0.0` |
| `-port` | Port number | `5555` |
| `-logtype` | Log format (`console` or `json`) | `console` |
| `-color` | Enable colored console output | `false` |
| `-skipmedia` | Skip media download in messages | `false` |
| `-admintoken` | Admin authentication token | (generated) |
| `-globalwebhook` | Global webhook URL | (none) |
| `-sslcertificate` | SSL certificate file | (none) |
| `-sslprivatekey` | SSL private key file | (none) |
| `-webhookretry` | Enable webhook retries with exponential backoff | `true` |
| `-webhookretrycount` | Max webhook delivery attempts per event | `5` |
| `-webhookretrydelay` | Base delay (s) between webhook retries (doubled per attempt) | `30` |
| `-rabbitretrycount` | Max RabbitMQ reconnect attempts | `10` |
| `-rabbitretrydelay` | Base delay (s) between RabbitMQ reconnect attempts | `5` |
| `-maxconnectdelay` | Base delay (s) between MAX WS reconnect attempts | `5` |
| `-maxconnectcap` | Max backoff (s) per MAX reconnect attempt | `300` |
| `-maxconnectretries` | Max MAX reconnect attempts (0 = infinite) | `0` |
| `-userconcurrency` | Max concurrent heavy ops per user (media, webhook-with-file) | `4` |

All flags can be set via environment variables — see `.env.example`.

## Configuration

MaxAPI reads a `.env` file at startup. See [`.env.example`](.env.example) for
the full list — the essentials are:

```bash
# Required
MAXAPI_ADMIN_TOKEN=your_admin_token_here

# Optional - Database (PostgreSQL; omit all DB_* to use SQLite with WAL)
DB_USER=maxapi
DB_PASSWORD=maxapi
DB_NAME=maxapi
DB_HOST=localhost
DB_PORT=5432
DB_SSLMODE=disable

# Optional - RabbitMQ
RABBITMQ_URL=amqp://guest:guest@localhost:5672
RABBITMQ_QUEUE=max_events
# RABBITMQ_EXCHANGE=max.events     # enables topic exchange + event-type routing keys
RABBITMQ_RETRY_COUNT=10
RABBITMQ_RETRY_DELAY=5

# Webhook delivery
WEBHOOK_FORMAT=json                 # or "form" (default)
WEBHOOK_RETRY_ENABLED=true
WEBHOOK_RETRY_COUNT=5
WEBHOOK_RETRY_DELAY=30              # seconds, doubled each attempt

# MAX reconnect backoff
MAX_CONNECT_DELAY=5
MAX_CONNECT_CAP=300
MAX_CONNECT_RETRIES=0               # 0 = infinite, otherwise fires ReconnectExhausted

# Limits & security
MAXAPI_USER_CONCURRENCY=4
# ALLOW_PRIVATE_IPS=true            # only for dev/test; bypasses the SSRF guard

# Misc
TZ=Europe/Moscow
```

## Quick Start

### 1. Create a User (Admin)

```bash
curl -X POST http://localhost:5555/admin/users \
  -H "Authorization: YOUR_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "Test User", "webhook": "https://your-server.com/webhook"}'
```

This returns a user token.

### 2. Open a Session

```bash
curl -X POST http://localhost:5555/session/connect \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"subscribe": ["All"], "immediate": true}'
```

If the user has no stored auth token the proxy starts a QR auth session
server-side; the response is `{"success": true, "details": "Awaiting QR scan"}`.
If an auth token is already on file the user is reconnected in place and the
response is `{"success": true, "details": "Connected to MAX"}`.

### 3. Fetch the QR code

```bash
curl -H "token: USER_TOKEN" \
  http://localhost:5555/session/qr
```

Returns `{"success": true, "qrcode": "data:image/png;base64,..."}`. Render it
in a browser and scan with the MAX mobile app (Settings → Devices → Scan QR
code). The server refreshes the code automatically when the MAX TTL elapses
and fires a new `QRGenerated` webhook — poll this endpoint every few seconds
(or react to the webhook) to keep the displayed code current.

Webhook consumers can skip polling entirely: `QRGenerated` carries `qrLink` +
`trackId`, `QRAuthorized` signals success, `Sync` fires once the full MAX
session comes online (the proxy reconnects automatically after authorisation).

### 4. Send a Message

```bash
curl -X POST http://localhost:5555/chat/send/text \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"phone": "+79007654321", "text": "Hello from MaxAPI!"}'
```

## API Reference

See [API.md](API.md) for the complete API documentation.

### Available Endpoints

#### Session
- `POST /session/connect` - Open a session (starts QR flow if no auth token, else reconnects)
- `GET /session/qr` - Get current QR code (base64 PNG) for an in-progress auth session
- `POST /session/disconnect` - Disconnect / cancel in-progress QR session
- `POST /session/logout` - Logout and remove user
- `GET /session/status` - Connection / authentication status

#### Messages
- `POST /chat/send/text` - Send text
- `POST /chat/send/image` - Send image
- `POST /chat/send/video` - Send video
- `POST /chat/send/audio` - Send audio
- `POST /chat/send/document` - Send document
- `POST /chat/send/edit` - Edit message
- `POST /chat/delete` - Delete messages
- `POST /chat/markread` - Mark as read
- `POST /chat/history` - Get history
- `POST /chat/react` - Add/remove reaction

#### Media Download
- `POST /chat/downloadimage` - Download image
- `POST /chat/downloadvideo` - Download video
- `POST /chat/downloadaudio` - Download audio
- `POST /chat/downloaddocument` - Download document

#### Users
- `POST /user/check` - Check phone numbers
- `POST /user/info` - Get user info
- `POST /user/avatar` - Get avatar URL
- `POST /user/presence` - Send typing indicator

#### Groups
- `POST /group/create` - Create group
- `GET /group/list` - List groups
- `POST /group/info` - Get group info
- `POST /group/invitelink` - Get invite link
- `POST /group/join` - Join group
- `POST /group/leave` - Leave group
- `POST /group/name` - Set name
- `POST /group/topic` - Set topic
- `POST /group/updateparticipants` - Add/remove members

#### Webhooks
- `POST /webhook` - Set webhook
- `GET /webhook` - Get webhook
- `DELETE /webhook` - Delete webhook

#### Admin
- `GET /admin/users` - List users
- `GET /admin/users/{id}` - Get a single user (proxy password masked, secrets omitted)
- `POST /admin/users` - Create user
- `PUT /admin/users/{id}` - Edit user
- `DELETE /admin/users/{id}` - Delete user

#### Observability
- `GET /health` - Liveness + readiness (unauthenticated). Returns `200 ok` or `503 degraded`.

## Webhook Events

| Event | Description |
|-------|-------------|
| `Message` | New message received |
| `MessageEdit` | Message was edited |
| `MessageDelete` | Message was deleted |
| `ReadReceipt` | Messages were read |
| `Connected` | Connected to MAX |
| `Disconnected` | Disconnected |
| `Reconnecting` | Reconnect attempt in progress (emitted every 10th attempt) |
| `ReconnectExhausted` | Reconnect budget (`MAX_CONNECT_RETRIES`) exhausted; session stopped |
| `QRGenerated` | New (or refreshed) QR code — payload carries `qrCodeBase64`, `qrLink`, `trackId`, `expiresAt` |
| `QRScanned` | User scanned QR code in the mobile app |
| `QRAuthorized` | Auth token received — proxy auto-starts the MAX session (`Sync` follows) |
| `QRExpired` | QR refresh budget exhausted or session cancelled |
| `AuthExpired` | Stored auth token is no longer valid |
| `ChatUpdate` | Chat was updated |
| `Typing` | User is typing |
| `ReactionChange` | Reaction changed |
| `ContactUpdate` | Contact updated |
| `PresenceUpdate` | Presence changed |
| `FileReady` | File upload complete |
| `All` | All events |

### Webhook Payload

Every webhook delivery (HTTP + RabbitMQ) carries a common identity block in
addition to the event-specific fields:

| Field | Description |
|-------|-------------|
| `userID` / `instanceID` | Internal MaxAPI user ID — use this to correlate events with your records |
| `instanceName` | Human-readable name of the MaxAPI user |
| `eventTimestamp` | Unix seconds (int64) when the event was dispatched |
| `serverHost` | Hostname of the MaxAPI instance (RabbitMQ only) |

**Webhook payload no longer contains the user API `token`.** Authenticate the
receiving endpoint via a standard mechanism (shared secret in the `Authorization`
header, HMAC, mTLS, …) and identify the MaxAPI user by `instanceID`.

Retries use exponential backoff (`WEBHOOK_RETRY_DELAY × 2^(attempt-1)`) and fire
only for network errors, HTTP 5xx, 408, and 429. 4xx responses are considered a
client contract failure and are delivered exactly once.

## Health Check

```bash
curl http://localhost:5555/health
```

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

Returns `503 degraded` when the DB ping fails.

## Project Structure

```
maxapi/
├── main.go           # Entry point
├── handlers.go       # HTTP handlers
├── routes.go         # Route definitions
├── clients.go        # Client manager
├── event_handler.go  # Event handling and webhooks
├── constants.go      # Event types
├── helpers.go        # Utility functions
├── db.go             # Database initialization
├── migrations.go     # Schema migrations
├── rabbitmq.go       # RabbitMQ integration
├── s3manager.go      # S3 integration
└── maxclient/        # MAX API client package
    ├── client.go     # Main client
    ├── auth.go       # Authentication
    ├── messages.go   # Messaging
    ├── files.go      # File operations
    ├── chats.go      # Chat operations
    ├── users.go      # User operations
    ├── events.go     # Event handling
    ├── types.go      # Data structures
    ├── opcodes.go    # Protocol opcodes
    └── errors.go     # Error types
```

## Scaling Roadmap

План улучшений для горизонтального масштабирования:

### Phase 1: Stateless Foundation
- [ ] **Redis Session Store** — вынести `ClientManager` в Redis (pub/sub + hash)
- [ ] **Connection Registry** — хранить mapping `user_id → instance_id` в Redis
- [ ] **Distributed Cache** — заменить `go-cache` на Redis с TTL

### Phase 2: Message Routing
- [ ] **Request Router** — направлять запросы на инстанс с активным WebSocket
- [ ] **Redis Pub/Sub** — межинстансная коммуникация для событий
- [ ] **Consistent Hashing** — распределение пользователей по инстансам

### Phase 3: Resilience
- [x] **Health Checks** — `GET /health` с метриками активных соединений, памяти, uptime
- [x] **Graceful Shutdown** — корректное закрытие RabbitMQ и HTTP при SIGTERM
- [x] **Webhook Retry** — exponential backoff с разграничением retriable/non-retriable
- [x] **RabbitMQ Reconnect** — прозрачное переподключение, durable delivery
- [x] **Per-user Concurrency Limits** — семафоры защищают от noisy neighbours
- [x] **SSRF Guard** — outbound HTTP client отказывает в приватных IP
- [ ] **Circuit Breaker** — защита от каскадных отказов подписчиков
- [ ] **Rate Limiting** — per-user лимиты через Redis (sliding window)

### Phase 4: Observability
- [ ] **Prometheus Metrics** — connections, latency, errors per instance
- [ ] **Distributed Tracing** — OpenTelemetry для запросов между сервисами
- [ ] **Structured Logging** — correlation ID для трассировки

### Architecture Pattern: Sticky Sessions + Shared State

```
┌─────────────┐     ┌─────────────┐
│ Load Balancer│────▶│   Redis     │◀── session registry
│ (sticky)    │     │ (pub/sub)   │◀── distributed cache
└──────┬──────┘     └─────────────┘◀── rate limits
       │
┌──────┼──────┬─────────────┐
▼      ▼      ▼             ▼
┌────┐ ┌────┐ ┌────┐    ┌────────┐
│API1│ │API2│ │API3│    │PostgreSQL│
└────┘ └────┘ └────┘    └────────┘
```

### Key Patterns
- **Sidecar** — Redis connection per instance
- **Competing Consumers** — RabbitMQ для webhook delivery
- **Leader Election** — один инстанс для cron-задач (reconnect sweep)

### Recommended Stack
| Component | Purpose |
|-----------|---------|
| Redis Cluster | State, pub/sub, rate limits |
| PostgreSQL + PgBouncer | Connection pooling |
| Traefik/HAProxy | Sticky sessions, health checks |
| Prometheus + Grafana | Monitoring |

---

## Based On

This project is based on:
- [WuzAPI](https://github.com/asternic/wuzapi) - WhatsApp REST API Gateway
- [pymax](https://github.com/sobytes/pymax) - Python MAX API client

## License

MIT License - See [LICENSE](LICENSE) for details.
