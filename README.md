# MaxAPI

MaxAPI is a multi-tenant REST API gateway for MAX Messenger, providing a simple HTTP interface to interact with the MAX protocol. It is based on the architecture of [WuzAPI](https://github.com/asternic/wuzapi).

## Features

- **Multi-tenant architecture**: Support multiple MAX accounts on a single server
- **SMS Authentication**: Authenticate via phone number and SMS code
- **Real-time webhooks**: Receive events via webhooks or RabbitMQ
- **Media handling**: Upload/download photos, videos, audio, and documents
- **Group management**: Create, manage, and interact with groups and channels
- **S3 Integration**: Optional media storage in S3-compatible storage

## Key Differences from WhatsApp (WuzAPI)

| Feature | WhatsApp (WuzAPI) | MAX (MaxAPI) |
|---------|-------------------|--------------|
| Authentication | QR Code | SMS Code |
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

## Configuration

MaxAPI uses a `.env` file for configuration:

```bash
# Required
MAXAPI_ADMIN_TOKEN=your_admin_token_here

# Optional - Database (PostgreSQL)
DB_USER=maxapi
DB_PASSWORD=maxapi
DB_NAME=maxapi
DB_HOST=localhost
DB_PORT=5432
DB_SSLMODE=disable

# Optional - RabbitMQ
RABBITMQ_URL=amqp://guest:guest@localhost:5672
RABBITMQ_QUEUE=max_events

# Optional
TZ=Europe/Moscow
MAXAPI_WEBHOOK_FORMAT=json
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

### 2. Request SMS Code

```bash
curl -X POST http://localhost:5555/session/auth/request \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"phone": "+79001234567"}'
```

### 3. Confirm SMS Code

```bash
curl -X POST http://localhost:5555/session/auth/confirm \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"code": "123456"}'
```

### 4. Connect

```bash
curl -X POST http://localhost:5555/session/connect \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"subscribe": ["Message", "ReadReceipt", "Connected"]}'
```

### 5. Send a Message

```bash
curl -X POST http://localhost:5555/chat/send/text \
  -H "token: USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"phone": "+79007654321", "text": "Hello from MaxAPI!"}'
```

## API Reference

See [API.md](API.md) for the complete API documentation.

### Available Endpoints

#### Session/Auth
- `POST /session/auth/request` - Request SMS code
- `POST /session/auth/confirm` - Confirm SMS code
- `POST /session/auth/register` - Register new user
- `POST /session/connect` - Connect to MAX
- `POST /session/disconnect` - Disconnect
- `POST /session/logout` - Logout
- `GET /session/status` - Get status

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
- `POST /admin/users` - Create user
- `PUT /admin/users/{id}` - Edit user
- `DELETE /admin/users/{id}` - Delete user

## Webhook Events

| Event | Description |
|-------|-------------|
| `Message` | New message received |
| `MessageEdit` | Message was edited |
| `MessageDelete` | Message was deleted |
| `ReadReceipt` | Messages were read |
| `Connected` | Connected to MAX |
| `Disconnected` | Disconnected |
| `AuthCodeSent` | Auth code sent |
| `ChatUpdate` | Chat was updated |
| `Typing` | User is typing |
| `ReactionChange` | Reaction changed |
| `ContactUpdate` | Contact updated |
| `PresenceUpdate` | Presence changed |
| `FileReady` | File upload complete |
| `All` | All events |

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
- [ ] **Health Checks** — endpoint `/health` с метриками активных соединений
- [ ] **Graceful Shutdown** — корректное завершение WebSocket при scale-down
- [ ] **Circuit Breaker** — защита от каскадных отказов (RabbitMQ, webhooks)
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
