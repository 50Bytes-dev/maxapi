# MaxAPI - REST API for MAX Messenger

MaxAPI is a multi-tenant REST gateway for MAX Messenger. It holds logins into
personal MAX accounts (by QR) and exposes them over HTTP: the account's
chats, sending text and photos and files, deleting for everyone, history,
contacts, groups, and webhook events. It guards each account with a pause of
the whole MAX account when MAX answers with a limit or restriction error.

**The gateway does not pace sends**: it sends each call at once, and sends of
one account may run at the same time. How fast an account sends is the job of
the project that uses the gateway (owner's decision of 2026-11-05). **The
pause stops what the account does in MAX, not what it reads**: sends,
deletes, edits, reactions, uploads, phone lookups and group changes are
refused while it lasts; the chat list, chat info, history, mark read,
downloads, contacts and user info go on.

This document is the contract of the gateway. The codes, the pause, the chat
list, photo upload, deletion and `MessageOutcome` follow the
contract shared with the VK gateway (vkapi); the differences that come from
MAX itself are named where they occur. The same API is an OpenAPI 3.1 schema,
`static/api/spec.yml`, served at `/api/spec.yml` with Swagger UI at `/api/`;
`spec_test.go` keeps its operations equal to the routes. The Python SDK
[maxapi-python](https://github.com/50Bytes-dev/maxapi-python) is generated
from it. Where this document and the code disagree, the code is right and
this document gets fixed.

- [What changed in 2.0.0](#what-changed-in-200)
- [Authentication](#authentication)
- [Codes and errors](#codes-and-errors)
- [Time budgets](#time-budgets)
- [Session](#session-endpoints)
- [Chats](#chats): `GET /chat/list`, `GET /chat/info`
- [Photos](#photos): `POST /media/photo`
- [Sending](#sending): `POST /chat/send/text`, `ref`, media sends
- [Deleting](#deleting): `POST /chat/delete`
- [Account pause](#account-pause)
- [Other message calls](#other-message-calls)
- [Users](#user-endpoints), [Groups](#group-endpoints), [Webhook URL](#webhook-endpoints), [Admin](#admin-endpoints)
- [Webhook events](#webhook-events)
- [Health](#health)
- [What MAX does, and what is not verified yet](#what-max-does-and-what-is-not-verified-yet)

---

## What changed in 2.0.0

2.0.0 makes the gateway a product of its own with one contract for every
caller (owner's decision of 2026-11-05): no flags or modes for one project,
breaking changes where they make the API right. The Python SDK goes to
`maxapi-python` 5.0.0 with it (schema `info.version` 5.0.0). Every capability
of 1.5.1 stays, some in another form:

| What | 1.5.1 | 2.0.0 | How to move |
|---|---|---|---|
| The account's chats | none (`GET /group/list` was documented, never routed); `POST /session/sync` logged into MAX again on every call and returned the raw login answer | `GET /chat/list` (pages, filters by kind and by title), `GET /chat/info` (by ids) | Use `GET /chat/list`. A dialog's person is `userId`; texts only with `include=lastMessage` |
| `POST /session/sync` | logged in again, answered MAX's login data and a `Sync` with `manual: true` | **removed** | Chats: `GET /chat/list?include=lastMessage,participants`. People: `POST /user/info`. The account: `GET /session/status` (`name`, `photo`) |
| `POST /group/info` | one chat, raw, with its last message's text | **removed** | `GET /chat/info?chatId=...` |
| `Sync` event | carried every field of MAX's login answer: `profile`, `chats` (100 chats with their last messages), `contacts` (fetched for every participant of every chat at each login), `config`, ... | carries `maxUserID`, `reconnect`, `name`, `photo` | Read chats from `GET /chat/list` after `Sync` (see below for clasta-app's import). The gateway no longer fetches anybody's contacts at login |
| Photos | `POST /chat/send/image` uploaded and sent one photo, without codes or pause; no lookup cache | **`/chat/send/image` removed.** `POST /media/photo` uploads once and answers an `attachment`; `POST /chat/send/text` takes up to 12 in `attachments` | Upload each photo once per account, then send its `attachment` to any chat of the account |
| `POST /chat/send/text` | text only, a text was required | `attachments`, `ref`; a message with attachments may have no text; new codes `address_refused`, `message_rejected`, `attachment_rejected` | Branch on the new codes ([codes](#codes-and-errors)) |
| `notify` of every send | default `false` | default `true`, as MAX's web client sends every message; MAX refuses a channel post with `false` (`errors.send-message.channel-notify`) | Leave it out; pass `false` only for a silent message in a dialog or group chat |
| `POST /chat/send/document`, `/audio`, `/video` | no codes, no pause; lookups bypassed the cache | the codes, pause, lookup cache and `ref` of `/chat/send/text`; answer `messageId` (string) and `chatId` | Branch on `code` |
| `POST /chat/delete` | `messageIds` numbers (MAX's ids lost digits above 2^53), `forMe`; `500` with text on any error | `messageIds` strings (numbers still read), for everyone only, up to 100; `results` per message (`deleted`, `not_found`, `too_old`, `forbidden`); delivery codes and the pause | Send the `messageId` strings of the sends; read `results` |
| `forMe` of a delete | deleted for the account only | **removed**: the gateway deletes for everyone | — |
| Every other call that reaches MAX (edit, mark read, history, react, downloads by id, user and group calls) | `500`/`404` with text, no `code`; ran while paused | `code` on every error: `not_connected` (503), `INVALID_INPUT` (400), `upstream_error` (502) with `upstreamCode`, `page_paused` (409) on a stop signal. While paused, a call that acts (edit, react, group changes) answers `409 page_paused`; a read (history, mark read, downloads, contacts, user info, presence, invite link) goes on | Branch on `code`; a `409` means the account is paused |
| `POST /user/check` | asked MAX for every number, never paused | goes through the lookup cache and fills it; a stop signal pauses the account (`409 page_paused`); at most 50 numbers; `name` only when known | Nothing for `exists`/`maxUserId`; `name` may be missing for a cached number |
| `POST /user/info` | no `phone` | `phone` (when MAX shows it) and `name` | — |
| Groups `create`, `join` | the raw chat with its last message | `chat` as a row of `GET /chat/list` | Read `chat.chatId`, `chat.title` |
| Downloads by URL | any host, no limit | the SSRF guard, `MAXAPI_MEDIA_MAX_BYTES`, `422 fetch_failed` | Only public http(s) URLs (or hosts in `ALLOW_PRIVATE_HOSTS`) |
| `LoggedOut` (MAX ended the session) | the gateway user was **deleted**, its token answered `401`, a new `POST /admin/users` was needed | the user **stays**: the login is erased and `AuthExpired` goes out with `reason: "logged_out"`; `LoggedOut` is never sent (accepted in a subscription) | Handle `AuthExpired` `logged_out` like an erased login: start a QR login with `POST /session/connect` on the same token |
| `POST /session/logout` | ended the session and deleted the gateway user | ends the session and erases the login; the user stays | Call `DELETE /admin/users/{id}` to delete it |
| `AuthExpired.reason` | the error text, MAX's code, `"login timed out"` | a machine code: `token_revoked`, `page_blocked`, `refresh_failed`, `logged_out` (shared with vkapi); MAX's text goes to the log | Branch on the code |
| `login.blocked`, `login.flood` | retried up to 3 times | `login.blocked` erases the login (`page_blocked`); `login.flood` stops at once and keeps it (`refresh_failed`), as MAX's web client does | — |
| `Paused`, `Resumed` | none | sent to every gateway user of the MAX account subscribed to them (or `All`), with a session or without | Subscribe instead of polling the status |
| `MessageOutcome` | none | a send with a `ref` answered `send_failed` is confirmed a minute later | Pass `ref`, subscribe to `MessageOutcome` |
| `messageId` | a string in sends; numbers in edit, mark read, reply, downloads | a string everywhere (numbers still read) | Keep MAX's ids as strings |
| `ChatUpdate` | carried the chat's last message's text | the chat without `lastMessage` | Texts come with `Message` |
| Work for unsubscribed events | every message was parsed, its photo downloaded, its first 50 characters logged | an event the user is not subscribed to is dropped unparsed; media only for `Message` subscribers; no text in the log; tokens and phones masked | — |
| Media of incoming messages | `media_delivery` read from the cache only after a restart: before it, no photo was downloaded | read from the database too | — |
| Logins at startup | all accounts at once | one by one, `MAX_STARTUP_LOGIN_INTERVAL_MS` (1 s) apart, ±50% | — |
| `GET /session/status` | no profile | `name`, `photo` of the MAX account | — |

**clasta-app's import from `Sync`** (`extensions/max/sync.py:68-237`) moves
to the new calls, field for field:

| `Sync` 1.5.1 | 2.0.0 |
|---|---|
| `maxUserID` | `Sync.maxUserID` (unchanged) |
| `profile.contact` (label) | `Sync.name`, or `GET /session/status` `name`, `photo` |
| `chats[]`: dialogs (`type` `DIALOG`, positive `id`), `hasBots`, `participants` (the other one), `lastMessage` (time, text, attaches), `newMessages` | `GET /chat/list?type=user&include=lastMessage` page by page: `chatId`, `userId` (the other one), `lastMessage` (MAX's message: `time`, `text`, `attaches`), `lastMessageDate`, `unreadCount`, `title` and `photo` of the person. Bots are dialogs too; `include=participants` gives the members |
| `contacts[]`: `names`, `phone`, `baseUrl`, `options` (`BOT`, `OFFICIAL`) | `POST /user/info` with the `userId`s (up to 100 per call): `names`, `name`, `phone`, `baseUrl`, `avatarUrl`, `options` |
| contacts without a dialog (clasta's `chats_from_contacts`) | `GET /user/contacts` (the account's contact list, as before) |

---

## Authentication

| Calls | Header |
|---|---|
| `/admin/*` | `Authorization: <admin token>` (`MAXAPI_ADMIN_TOKEN` or `-admintoken`), raw, without `Bearer` |
| every other call but `/health` | `token: <account token>` from `POST /admin/users` (a `?token=` query parameter when the header is absent) |
| `/health` | none |

A missing or unknown token answers `401` `{"success": false, "error": "unauthorized"}`
without `code`. For an account token that worked before, the account was
deleted (`DELETE /admin/users/{id}`).

---

## Codes and errors

Every error body:

```json
{
    "success": false,
    "error": "MAX refused the message",
    "code": "send_rejected",
    "upstreamCode": "error.user.restricted.send",
    "paused": true,
    "pauseReason": "spam"
}
```

- **Branch on `code` and the HTTP status**, never on `error`, a text that may
  change.
- **`upstreamCode`** is a string: MAX's own error code, present on every
  failure MAX answered with an error. It is absent when no MAX call was made
  (a pause already set, a check of the gateway) or MAX did not answer.
- **`paused`, `pauseReason`** are present only when the MAX account is paused.
- Treat a code you do not know as a failure of the call.

The codes of the contract shared with vkapi:

| HTTP | `code` | Where | Meaning | Reached MAX? |
|---|---|---|---|---|
| `503` | `not_connected` | every account call that reaches MAX | No live MAX session, or MAX did not answer a call that is not a send | No |
| `400` | `invalid_target` | send, delete | No `chatId` and no `phone` (chat `0` is the account's Favorites and is never addressed) | No |
| `404` | `address_not_found` | send | `phone` malformed or not on MAX, or MAX knows no such chat (`not.found`, `chat.not.found`, `user.not.found`) | Maybe the lookup |
| `403` | `address_refused` | send | The chat does not let the account write: MAX's `chat.denied`, `channel.denied`, `chat.write.denied` | Yes, refused |
| `503` | `lookup_failed` | send by `phone` | The phone lookup timed out, lost the connection, or MAX answered it with another error | The lookup |
| `422` | `send_rejected` | send, delete | MAX refused (`upstreamCode`); or the account is paused (`paused`, no `upstreamCode`) | Not sent |
| `422` | `message_rejected` | send | The message itself is refused: no text and no attachment, a text over 4000 UTF-16 units, over 12 attachments (no MAX call), or MAX refused its text (a code with `too.long`, `length`, `text` or `empty`). **Final for this message in this chat** | Not sent |
| `422` | `attachment_rejected` | send | MAX refused an attachment (a code with `attach`, `photo`, `token`, `file`, `video` or `upload` on a message with attachments), or the upload of a media send failed. Upload the photo again | Not sent |
| `500` | `send_failed` | send, delete | **The outcome is unknown**: no answer in time, or the session lost after the frame went out. Never resend automatically; with a `ref`, `MessageOutcome` tells later | Maybe |
| `409` | `page_paused` | every other call that reaches MAX | The MAX account is paused: before a call that acts (no MAX call, no `upstreamCode`), or by the call itself (a stop signal, with `upstreamCode`). Reads are not refused for a pause | — |
| `502` | `upstream_error` | every other call that reaches MAX | MAX refused the call (`upstreamCode`), or its answer was unreadable | Yes |
| `400` | `invalid_image` | `POST /media/photo` | Not a JPEG, PNG, GIF or WebP image, or broken base64 | No |
| `422` | `fetch_failed` | photo by URL, media sends, downloads | The URL could not be fetched: not http(s), an internal address, no answer within the time, a non-2xx answer, over the limit | No |
| `502` | `upload_failed` | `POST /media/photo` | MAX did not take the upload (refused opcode 80, no upload address, the upload failed); try again | Yes |
| `400` | `INVALID_INPUT` | any | The body is not JSON, or a field is out of its bounds | No |
| `413` | `INVALID_INPUT` | `POST /media/photo` | The body is over `MAXAPI_PHOTO_MAX_BYTES` | No |
| `500` | `INTERNAL_ERROR` | `POST /session/resume`, `GET /admin/users/{id}` | Database error | — |
| `404` | `NOT_FOUND` | `GET /admin/users/{id}` | No such account | — |

The pause has its own answer per kind of call (C0 of the shared contract): a
send or a delete answers `422 send_rejected`, every other call that acts in
MAX `409 page_paused`; a call that only reads goes on. vkapi answers
`page_paused` with `409` too.

Calls that never reach MAX keep their pre-2.0 errors without `code`: the
session calls (`connect`, `qr`, `disconnect`, `logout`, `status`), the
webhook URL calls, and the admin calls other than those listed.

---

## Time budgets

Each call answers within these times (MAX's waits plus the gateway's own
database calls of at most 200 ms each):

| Call | Budget | Made of |
|---|---|---|
| `POST /chat/send/text` | ~9.5 s | phone lookup ≤ 4 s, send ≤ 5 s, cache and pause writes |
| `POST /chat/send/document`, `/audio` | ~100 s | fetch ≤ 8 s, upload address ≤ 30 s, upload ≤ 30 s, MAX processing ≤ 30 s, then as a text |
| `POST /chat/send/video` | ~280 s | fetch ≤ 8 s, upload address ≤ 30 s, upload ≤ 120 s, MAX processing ≤ 120 s, then as a text |
| `POST /chat/delete` | ~5.5 s | delete ≤ 5 s |
| `POST /media/photo` | ~27 s | fetch ≤ 8 s, opcode 80 ≤ 4 s, upload ≤ 15 s |
| `GET /chat/list` | ~7 s | page ≤ 4 s, people ≤ 2.5 s |
| `GET /chat/info` | ~5.5 s | chats ≤ 2.5 s, people ≤ 2.5 s |
| `POST /chat/history`, `/chat/send/edit`, `/chat/markread`, `/chat/react`, `/user/presence`, group calls, `GET /user/contacts` | ≤ 30 s | one MAX call (history 8 s) |
| `POST /user/info` | ~8 s | people ≤ 8 s |
| `POST /user/check` | 4 s per number not in the cache | lookups one by one |
| `POST /chat/downloaddocument`, `/downloadvideo` | ~90 s | address ≤ 30 s, fetch ≤ 60 s |
| `POST /chat/downloadimage`, `/downloadaudio` | ~60 s | fetch ≤ 60 s |
| `POST /session/connect` | ~5 s (`immediate: false`), at once otherwise | the login runs in the background; a QR login asks MAX for a QR first (≤ 30 s) |
| `GET /session/status`, `GET /session/qr`, `POST /session/resume`, `POST /session/disconnect`, `POST /session/logout`, webhook, admin | ~1 s | the database |
| `GET /health` | ~1 s | a database ping |

---

## Session endpoints

### Connect

Open a MAX session. Behaviour depends on whether the user already has a stored
auth token:

- **No auth token** → the gateway starts a QR auth session and responds with
  `{"success": true, "details": "Awaiting QR scan"}`. Fetch the code via
  `GET /session/qr` (or the `QRGenerated` webhook), scan it in the MAX app;
  `QRAuthorized` and `Sync` follow (the gateway connects by itself).
- **Auth token present** → the gateway reconnects with it and responds with
  `{"success": true, "details": "Connected to MAX"}`.
- **Already connected** → idempotent; subscriptions are still refreshed and the
  response carries `"alreadyConnected": true` plus the
  [pause](#account-pause): `paused` always, `pauseReason` and `upstreamCode`
  while paused.

```http
POST /session/connect
Content-Type: application/json

{
    "subscribe": ["Sync", "AuthExpired", "Paused", "Resumed", "MessageOutcome"],
    "immediate": true
}
```

`subscribe` overwrites the subscription; unknown names are dropped. Without
`immediate` the call waits 5 s and answers `500` "failed to connect" when the
session is not up by then.

### Get QR Code

Returns the QR code of an in-progress QR login. The gateway issues a new one
when MAX's expires (up to 10 times) and sends `QRGenerated` each time.

```http
GET /session/qr
```

```json
{ "success": true, "qrcode": "data:image/png;base64,..." }
```

Errors: `500` `"no session"` when no QR login runs, `500` `"already logged in"`
once a login is stored.

### Disconnect

Closes the MAX session, or cancels an in-progress QR login. Keeps the stored
login.

```http
POST /session/disconnect
```

### Logout

Ends the MAX session (opcode 20) and **erases the stored login. The gateway
user stays** with its token, webhook, subscription and lookups; a new
login is a QR login through `POST /session/connect`. Deleting the user is
`DELETE /admin/users/{id}`. A [pause](#account-pause) of its MAX account
stays. No webhook is sent.

```http
POST /session/logout
```

### Get Status

```http
GET /session/status
```

```json
{
    "success": true,
    "connected": true,
    "authenticated": true,
    "loggedIn": true,
    "maxUserID": 123456789,
    "name": "Магазин у дома",
    "photo": "https://i.oneme.ru/i?r=...",
    "paused": false
}
```

| Field | Present | Meaning |
|---|---|---|
| `connected` | always | The MAX WebSocket of this user is up. A paused account keeps its session. |
| `authenticated` | always | A login is stored. |
| `loggedIn` | always | `connected && authenticated`. |
| `maxUserID` | always | The MAX id of the live session's account; `0` without a session. |
| `name`, `photo` | when known | The MAX account's name and photo: the live session's, else those of the last login while the login is stored (C11). |
| `paused` | always | Whether the MAX account is [paused](#account-pause). **Read it before `connected`.** |
| `pauseReason`, `upstreamCode` | while paused | `spam`, and MAX's code that paused it. |

No MAX call.

### Resume

Lifts the [pause](#account-pause) of the MAX account this user is logged into
(the live session's, or the one stored at the last login), for every gateway
user of it; each one subscribed to `Resumed` gets that event. The body is
optional (`{}` and `{"captchaKey": "..."}` work; `captchaKey` belongs to
vkapi and is ignored).

```http
POST /session/resume
```

```json
{ "success": true, "maxUserID": 123456789 }
```

The stored pause is deleted first: if that fails the account stays paused and
the answer is `500` `INTERNAL_ERROR`. An account that is not paused answers
`200` too. No MAX call. Errors: `400` `INVALID_INPUT`, `500` `INTERNAL_ERROR`.

### Reconnect

A user with a stored login is kept connected by a reconnect loop. A dropped
session is logged into again after a backoff that doubles from
`MAX_CONNECT_DELAY` up to `MAX_CONNECT_CAP` seconds, spread by ±20%.

- **Network failures** on the way to the login are retried with that backoff,
  up to `MAX_CONNECT_RETRIES` (`0` = forever), then `ReconnectExhausted`.
- **An auth error** stops the loop at once and **erases the stored login**:
  `login.token`, `login.expired`, `auth.invalid`, `auth.expired` and
  `user.not.found` (`AuthExpired` `token_revoked`), `login.blocked`
  (`AuthExpired` `page_blocked`: the account is blocked, and the gateway makes
  no further call for it).
- **A refusal 3 times in a row** stops the loop and **keeps the login**
  (`AuthExpired` `refresh_failed`; `POST /session/connect` tries again): any
  other MAX error code on SessionInit or the login, or the login left
  unanswered or losing its socket. **`login.flood`** (too many logins) counts
  as the third at once: MAX's web client stops at it too.
- **At startup** the stored logins are logged into one by one,
  `MAX_STARTUP_LOGIN_INTERVAL_MS` (1000) apart, each spread by ±50%: 300
  accounts take about five minutes. A user that got a session in the meantime
  is not logged in again.

---

## Chats

### List and search

```http
GET /chat/list?type=chat&include=lastMessage
GET /chat/list?marker=1776820000000
GET /chat/list?q=Покупатели
token: <account token>
```

The account's chats, the newest first, as the MAX web client pages them
(CHATS_LIST, opcode 53). With `q`, only the rows whose title (a dialog's: the
person's name) holds `q`, in any letter case. No message's text is in the
answer unless `include=lastMessage` asks for it.

`q` filters each page, as `type` does: page on while `nextMarker` is present
for every match. MAX's own search (CHAT_SEARCH, opcode 68) is not used: live
on 2026-11-05 it found none of the account's chats by title, only messages by
their text, and MAX's web client searches titles among the chats it has
loaded.

| Parameter | Meaning |
|---|---|
| `marker` | `nextMarker` of the previous page; absent for the first page. |
| `type` | Keep only rows of these kinds: `user`, `chat`, `channel`; comma-separated or repeated. MAX has no such filter: the gateway keeps the rows of each page, so a page may hold fewer rows, even none, while `nextMarker` is still there. Go on while `nextMarker` is present. |
| `q` | Part of the title, at most 200 characters; the rows of each page whose title holds it, in any letter case. With `type` and `marker` as above. |
| `include` | `lastMessage`: the last message as MAX gives it, its text included. `participants`: the members' MAX ids. Comma-separated or repeated. |

```json
{
    "success": true,
    "nextMarker": "1776820000000",
    "items": [
        {
            "chatId": -70000000000000,
            "type": "chat",
            "title": "Покупатели",
            "membersCount": 17,
            "photo": "https://i.oneme.ru/i?r=...",
            "canWrite": true,
            "unreadCount": 0,
            "lastMessageId": "116448890093701000",
            "lastMessageDate": 1776869050
        },
        {
            "chatId": 123457555,
            "type": "user",
            "userId": 987654321,
            "title": "Иван",
            "canWrite": true,
            "unreadCount": 3,
            "lastMessageId": "116448890093700000",
            "lastMessageDate": 1776869000
        },
        {
            "chatId": -70000000000001,
            "type": "channel",
            "title": "Новости",
            "membersCount": 2400,
            "canWrite": false,
            "cantWriteReason": "not_admin",
            "unreadCount": 0
        }
    ]
}
```

| Field | Meaning |
|---|---|
| `chatId` | The chat; `POST /chat/send/text` takes it as it is. A dialog's id is positive (the XOR of the two members' ids), a group chat's and a channel's negative. |
| `type` | `user` (a dialog with a person or a bot: MAX's `DIALOG`), `chat` (a group chat: `CHAT`), `channel` (`CHANNEL`). MAX has no communities; vkapi's `group` does not occur. |
| `userId` | A dialog's person (or bot). |
| `title` | A chat's title, or the person's name; `""` when unknown. |
| `membersCount` | Members of a group chat or channel. |
| `photo` | The chat's or the person's photo. |
| `canWrite`, `cantWriteReason` | Whether the account may write there, and why not ([rules](#who-may-write-where)). |
| `unreadCount` | MAX's count of unread messages (`newMessages`). |
| `lastMessageId`, `lastMessageDate` | The last message's id and unix time (seconds), without its text. |
| `lastMessage` | With `include=lastMessage` only: MAX's message (`id`, `sender`, `time` in ms, `text`, `type`, `attaches`, ...). |
| `participants` | With `include=participants` only: the members' MAX ids. |

The account's own Favorites (chat `0`) is not listed: it is no address.

Pages: MAX sets the page size (`chats-page-size`, 50 at the time of
writing). The first page is asked with a marker one day ahead of now (the
marker is the `lastEventTime`, in ms, a page starts before; LOGIN's own
`chatMarker` works the same way), each next one with MAX's `marker` from the
previous answer, given out as `nextMarker`. A chat that gets a message while
you page moves to the top and may be missed or seen twice: page again from
the start for a fresh list.

One MAX call for the page, one for the people behind its dialogs (their names
and photos, CONTACT_INFO, opcode 32, kept in the session's memory
afterwards).

Failures: `400` `INVALID_INPUT` (an unknown `type` or `include`, a `marker`
that is no `nextMarker`, `q` over 200 characters), `409` `page_paused` (MAX
answered a stop signal: the account is now paused), `502` `upstream_error`,
`503` `not_connected`. A paused account's list is read as usual.

### Chats by id

```http
GET /chat/info?chatId=-70000000000000&chatId=123457555
```

Up to 100 ids, comma-separated or repeated, in one CHAT_INFO call (opcode
48). The rows come in the order asked, as in the list; the chats MAX does not
show the account are listed in `missing`. `include` works as in the list.

```json
{ "success": true, "items": [ { "chatId": -70000000000000, "type": "chat", "...": "..." } ], "missing": [123457555] }
```

Failures: `400` `INVALID_INPUT` (no `chatId`, over 100, a `chatId` that is not
a non-zero integer), `409`, `502`, `503` as for the list.

### Who may write where

MAX does not say whether the account may write in a chat; MAX's web client
works it out from the chat, and the gateway follows its rules (its
`viewerCanWrite`, read from the web client of 2026-11-05):

| Case | `canWrite` | `cantWriteReason` |
|---|---|---|
| A group chat or channel whose `status` is `REMOVED`, `BLOCKED`, `LEFT` or `CLOSED` | false | `chat_closed` |
| `restrictions` has the input bit (64): MAX does not let the account write there. MAX's service dialogs carry it (`restrictions: 8128`) | false | `restricted` |
| The account is not among `participants` | false | `not_member` |
| A channel where the account is neither the owner nor an admin with the right to post (`permissions` bit 256 or 1) | false | `not_admin` |
| A dialog whose person's MAX account is blocked (`accountStatus` 1) or deleted (2) | false | `user_blocked`, `user_removed` |
| Anything else | true | |

The live check of these rules in a chat where only admins write and in a
channel without rights was not made, see [below](#what-max-does-and-what-is-not-verified-yet).
A send to a chat the account may not write in answers `403`
`address_refused`.

---

## Photos

### Upload a photo once

```http
POST /media/photo
Content-Type: application/json

{ "url": "https://cdn.example.com/p/1.jpg" }
```

`{"data": "<base64 or data URL>"}` or a `multipart/form-data` body with a
`file` field work too.

```json
{ "success": true, "attachment": "AAAAB3NzaC1...", "width": 1080, "height": 1440 }
```

- The gateway uploads the photo to MAX for the account (PHOTO_UPLOAD, opcode
  80, for an upload address; then an HTTP upload, as the web client does) and
  answers MAX's photo token as `attachment`. `POST /chat/send/text` takes it
  in `attachments`, for **any chat of this account**: upload each photo once.
- MAX does not attach a photo by an outside address, so a URL is fetched by
  the gateway: http and https only, never an internal address (loopback,
  private, link-local, CGNAT, multicast and the like; also after a redirect;
  `ALLOW_PRIVATE_HOSTS` opens it for named hosts), at most 8 s, a 2xx answer.
- The body, and the photo, are at most `MAXAPI_PHOTO_MAX_BYTES` (10 MiB):
  base64 makes data a third larger, so send large photos by URL or as a file.
- The photo must be a JPEG, PNG, GIF or WebP image; `width` and `height` are
  read from its header.
- An `attachment` works in other chats of the account: live on 2026-11-05 in
  a group chat and a channel, at once and 48 minutes after the upload. How
  long MAX keeps it is not known (a day later is not checked): a send that
  MAX refuses for an attachment answers `422 attachment_rejected`; upload the
  photo again.
- The pause applies (`409 page_paused`, no MAX call), and a stop signal on
  opcode 80 pauses the account. A size limit of MAX's (`file.size.limit` and
  the like) is no stop signal.

Failures: `400` `invalid_image`, `400` `INVALID_INPUT` (neither `url`, `data`
nor a file; both `url` and `data`), `413` `INVALID_INPUT`, `422`
`fetch_failed`, `409` `page_paused`, `502` `upload_failed` (try again), `503`
`not_connected`.

---

## Sending

### Send a message

```http
POST /chat/send/text
Content-Type: application/json

{
    "chatId": -70000000000000,
    "text": "Платье, 1200 ₽",
    "attachments": ["AAAAB3NzaC1...", "AAAAC4NzaD2..."],
    "ref": "post-42-chat-7",
    "notify": true
}
```

| Field | Meaning |
|---|---|
| `chatId` | The chat, as `chatId` of `GET /chat/list`: a dialog (positive) or a group chat or channel (negative). `0` is no address. |
| `phone` | Instead of `chatId`: a person's phone with country code, digits only with an optional `+`, passed to MAX as given (10 to 15 digits, else `404 address_not_found` without a MAX call). Looked up through the [cache](#phone-lookup-cache). With both, `chatId` wins. |
| `text` | At most 4000 UTF-16 code units, MAX's `max-msg-length` as its web client counts it (an emoji counts 2). May be empty when `attachments` has one. MAX has no markdown here: the text goes as it is. |
| `attachments` | Up to 12 `attachment`s of `POST /media/photo`, in this order: MAX's own limit (live 2026-11-05; 13 is refused with `errors.send-message.attachment.max-size-reached`). |
| `ref` | Your own id of the message, at most 128 bytes ([below](#ref-and-messageoutcome)). |
| `replyTo` | The id of a message to reply to (a string; a number is read too). |
| `notify` | Whether MAX notifies the recipients; default `true`, as MAX's web client sends every message. MAX refuses a channel post with `false`. |

```json
{ "success": true, "messageId": "115478893120847321", "chatId": -70000000000000 }
```

`messageId` is MAX's id of the new message, a string: keep it as is, and pass
it to `POST /chat/delete`. `chatId` is the chat it went to: for a send by
`phone`, the dialog of the account and the person.

The checks run in this order, and each answers on its own failure: session
(`not_connected`), body (`INVALID_INPUT`), target (`invalid_target`), message
(`message_rejected`), pause (`send_rejected` paused), lookup (cache, then
MAX), send. Everything before the lookup makes no MAX call.

The gateway does not pace sends: each send goes to MAX at once, and sends of
one account may run at the same time. How fast an account sends is the job of
the project that uses the gateway (owner's decision of 2026-11-05). The pace
measured live: one post every 3–5 s, see
[What MAX does](#what-max-does-and-what-is-not-verified-yet).

### Phone lookup cache

A send by `phone` needs the person's MAX id, which MAX gives for the phone
(opcode 46). Repeated lookups, above all of numbers MAX does not know, are
what MAX limits and then restricts accounts for, so MAX's answer is kept per
gateway user:

- **Found**: the person's MAX id, for 90 days. If MAX refuses a send to a
  cached id with an error of its own (other than a pause code), the entry is
  dropped and the next send looks the number up again.
- **Not found**: only MAX's explicit `not.found`, for 30 days.
- Nothing else is cached. The key is the phone exactly as sent. The cache
  goes with the gateway user. `POST /user/check` reads and fills it too.

### ref and MessageOutcome

`ref` is your own id of a message, at most 128 bytes (longer is
`400 INVALID_INPUT`). The gateway does not take a `cid` from you: it makes the
message's cid (MAX's client id of a message, which MAX echoes in it) from the
`ref`, the same for every send with that `ref`, in the range of the unix
millisecond values the web client uses. A send without a `ref` gets the
current time, as the web client does.

When a send with a `ref` answers `send_failed` and the gateway user is
subscribed to `MessageOutcome` (or `All`):

1. A minute after the send, the gateway reads the chat's last 200 messages
   (CHAT_HISTORY, opcode 49) and looks for the account's message with that
   cid. It sends nothing and gives out no text.
2. It posts one `MessageOutcome`: `delivered: true` with `messageId`, or
   `delivered: false`.
3. MAX unreachable (no session, no answer) gets the look made again at 3 and
   at 10 minutes; then the check ends without an event.
4. No look and no event while the account is paused, when the user is logged
   into another account by then, when MAX refuses the look (a stop signal
   pauses the account), or when all 200 messages are newer than the send and
   none is it. The checks live in memory: a restart drops them.

MAX keeps one message per cid: live on 2026-11-05 a send repeated with the
same `ref` a minute later, and 44 minutes later, answered the first message's
`messageId`, and no second message appeared. A repeat hours later is not
checked, so after a
`send_failed` still wait for `MessageOutcome` before you resend, or resend
with the same `ref` within minutes.

### Media sends

`POST /chat/send/document` (`document`, `fileName`, `caption`),
`POST /chat/send/audio` (`audio`, `fileName`) and `POST /chat/send/video`
(`video`, `fileName`, `caption`) send one file each, with `chatId` or `phone`,
`notify` and `ref`. The media is an http(s) URL (fetched through the same
guard, at most `MAXAPI_MEDIA_MAX_BYTES`, 50 MiB), a data URL or base64. The
file is uploaded to MAX (opcodes 87 and 82) after the pause check and before
the phone lookup, then sent with the rules and codes of `/chat/send/text`. MAX
refusing the upload, or the upload failing, answers `422 attachment_rejected`;
a URL that cannot be fetched `422 fetch_failed`. Audio goes as a file, as MAX
takes it. The answer is `messageId` and `chatId`.

Photos are not sent this way: upload them with `POST /media/photo` and send
them in `attachments`.

---

## Deleting

```http
POST /chat/delete
Content-Type: application/json

{ "chatId": -70000000000000, "messageIds": ["115478893120847321", "115478893120847322"] }
```

```json
{
    "success": true,
    "chatId": -70000000000000,
    "results": [
        { "messageId": "115478893120847321", "status": "deleted" },
        { "messageId": "115478893120847322", "status": "not_found" }
    ]
}
```

- Deletes for everyone, up to 100 messages of one chat, in one MAX call
  (MSG_DELETE, opcode 66, `forMe: false`), as the web client does. Repeated
  ids are taken once.
- MAX answers with the ids it deleted: those are `deleted`; the others
  `not_found` (MAX did not delete them and said no more).
- When MAX refuses the whole call with a code of a message, every id gets its
  status with `upstreamCode`: `not_found` (`not.found` and other "not found"
  codes), `too_old` (a code with `time`, `old`, `expired` or `late`),
  `forbidden` (a code with `denied`, `forbidden`, `access`, `permission` or
  `rights`). Ask again for single ids to tell them apart.
- How long after a send MAX deletes for everyone is not known: live on
  2026-11-05 it deleted at once and 44 minutes after the send; a day and a
  week later are not checked (MAX's `edit-timeout` of 7 days is for editing).
- The pause applies as to a send (`422 send_rejected` with `paused`, no MAX
  call), and a stop signal pauses the account.
- `forMe` of 1.5.1 is gone: a delete is for everyone.

Failures: `400` `invalid_target` (`chatId` 0), `400` `INVALID_INPUT`
(`messageIds` empty, over 100, an id that is not a positive integer), `422`
`send_rejected`, `500` `send_failed` (no answer: the messages may be deleted),
`503` `not_connected`.

---

## Account pause

MAX answers an account that ran into one of its limits, or that it
restricted, with an error whose code says so. Going on after that turns a
temporary restriction into a block, so the gateway stops what the account
does in MAX until the owner resumes it. What the account reads goes on.

- **Trigger:** a MAX error on any call of the account (a lookup, a send, a
  delete, an upload, the chat list, a history read, ...) whose code contains,
  in any case, `restricted`, `too.many`, `flood`, `spam`, or `limit` other
  than a bound on the request (`limit` with `size`, `length`, `file`,
  `upload`, `attach`, `photo`, `video`, `audio`, `participant`, `member`,
  `count` or `text` is no stop signal: `file.size.limit` is not,
  `error.limit.violate` is). Seen live: `error.user.restricted.send`.
- **The call MAX refused** answers by its kind plus `paused: true`,
  `pauseReason: "spam"` and MAX's code in `upstreamCode`: a send `422
  send_rejected` (or `503 lookup_failed` for the lookup), a delete `422
  send_rejected`, any other call `409 page_paused`.
- **The pause belongs to the MAX account**, not to the gateway user. Every
  gateway user logged into the account is paused, and the pause outlives
  them: the deletion of the gateway user, a new QR login into the same
  account, a restart. It is stored before the answer goes out.
- **`Paused`** goes out once, to every gateway user of the account subscribed
  to it (or `All`), with or without a session: `pauseReason`, `upstreamCode`,
  `maxUserID`. A further refusal keeps the first pause and sends nothing.
- **While paused** no call that acts reaches MAX, from any gateway user of
  the account: a send or delete answers `422 send_rejected`; an edit, a
  reaction, a photo upload, a phone lookup (`/user/check`) and the group
  calls but `invitelink` answer `409 page_paused`; both with `paused` and
  `pauseReason` and without `upstreamCode`.
- **Reads go on while paused** (owner's decision of 2026-11-05): `GET
  /chat/list`, `GET /chat/info`, `POST /chat/history`, `/chat/markread`,
  `/chat/downloaddocument`, `/chat/downloadvideo`, `/user/info`,
  `/user/presence`, `/group/invitelink` and `GET /user/contacts` reach MAX as
  usual. A stop signal MAX answers on one of them still pauses the account
  (`409 page_paused` with `upstreamCode`) and keeps the pause. The session
  itself stays up, and the session calls work.
- `GET /session/status` and an already connected `POST /session/connect`
  report it.
- Only `POST /session/resume` ends it, through any gateway user of the
  account; **`Resumed`** goes to every one subscribed.
- `pauseReason` is `spam` for every pause so far: MAX's codes are not known
  well enough to tell kinds of pause apart.

---

## Other message calls

These calls follow the shared rules: no session `503 not_connected`, a body
that does not decode `400 INVALID_INPUT`, a MAX refusal `502 upstream_error`
with `upstreamCode` (a stop signal: `409 page_paused`, now paused), MAX not
answering `503 not_connected`. While the account is paused, an edit and a
reaction answer `409 page_paused` without a MAX call; mark read, history and
the downloads go on.

### Edit a message

```http
POST /chat/send/edit

{ "chatId": 123456789, "messageId": "115478893120847321", "text": "Updated" }
```

A text over 4000 UTF-16 units answers `422 message_rejected`. MAX lets a
message be edited for 7 days (`edit-timeout`).

### Mark as read

```http
POST /chat/markread

{ "chatId": 123456789, "messageId": "115478893120847321" }
```

### History

```http
POST /chat/history

{ "chatId": 123456789, "count": 50, "fromTime": 1699999999999 }
```

```json
{ "success": true, "messages": [ { "id": "115478893120847321", "chatId": 123456789, "sender": 987654321, "text": "...", "time": 1699999999999, "type": "USER" } ] }
```

The messages before `fromTime` (ms; now by default), their texts included:
this call exists for them.

### Reaction

```http
POST /chat/react

{ "chatId": 123456789, "messageId": "111222333", "reaction": "👍" }
```

An empty `reaction` removes the account's reaction.

### Downloads

- `POST /chat/downloadimage` and `/downloadaudio` `{url}`: the gateway fetches
  the URL (MAX's `baseUrl` of a photo of a `Message`) through the
  [guard](#upload-a-photo-once), at most `MAXAPI_MEDIA_MAX_BYTES`, within 60 s,
  and answers `data` (base64) and `mimeType`. No MAX call; `422 fetch_failed`.
- `POST /chat/downloaddocument` `{chatId, messageId, fileId}` and
  `/downloadvideo` `{chatId, messageId, videoId}`: MAX gives the file's address
  (opcodes 88 and 83), the gateway fetches it the same way; the video answer
  adds `url`.

---

## User endpoints

### Check phone numbers

```http
POST /user/check

{ "phone": ["79001234567", "79007654321"] }
```

```json
{
    "success": true,
    "users": [
        { "phone": "79001234567", "exists": true, "maxUserId": 123456789, "name": "Иван" },
        { "phone": "79007654321", "exists": false, "maxUserId": 0, "error": "Service Error: Try later (service.unavailable)", "upstreamCode": "service.unavailable" }
    ]
}
```

- At most 50 numbers. Each goes through the [lookup cache](#phone-lookup-cache)
  and fills it: a number found within 90 days, or refused by MAX's
  `not.found` within 30, makes no MAX call, and a send by that phone after the
  check makes no second lookup. The others are looked up in MAX one by one.
- `exists: false` without `error`: MAX has no such user. With `error` the
  lookup failed and existence is unknown (`upstreamCode` when MAX answered).
- `name` is there when known: always for a fresh lookup, for a cached number
  only while the session remembers the person.
- A stop signal on a lookup pauses the account and ends the call with `409
  page_paused`; while paused no lookup is made (`409 page_paused`): a lookup
  of strangers' numbers is what MAX limits.

### Users by id

```http
POST /user/info

{ "userIds": [123456789, 987654321] }
```

```json
{ "success": true, "count": 1, "users": [ { "id": 123456789, "name": "Иван Петров", "names": [ ... ], "phone": 79001234567, "accountStatus": 0, "options": ["TT", "ONEME"], "baseUrl": "...", "avatarUrl": "..." } ] }
```

1 to 100 ids, from the session's memory or MAX (CONTACT_INFO, opcode 32).
`phone` when MAX shows it to the account. `options` carries MAX's flags such
as `BOT` and `OFFICIAL`.

### Typing indicator

```http
POST /user/presence

{ "chatId": 123456789 }
```

### Contacts

```http
GET /user/contacts
```

The account's contact list (opcode 36).

---

## Group endpoints

`POST /group/create` (`name`, `participants`), `POST /group/join` (`link`),
`POST /group/leave`, `POST /group/name`, `POST /group/topic`,
`POST /group/updateparticipants` (`operation` `add` or `remove`) and
`POST /group/invitelink` (`chatId`) follow the shared rules above. While the
account is paused, all but `invitelink` (a read) answer `409 page_paused`
without a MAX call. `create` and `join` answer the chat as a row of
`GET /chat/list`:

```json
{ "success": true, "chat": { "chatId": -70000000000005, "type": "chat", "title": "My Group", "canWrite": true, "unreadCount": 0 } }
```

`invitelink` answers `{"success": true, "inviteLink": "https://max.ru/join/..."}`,
or `404 address_not_found` when MAX does not show the account the chat.

---

## Webhook endpoints

- `POST /webhook` and `PUT /webhook` `{"webhook": "https://..."}` set the
  account's webhook URL; `GET /webhook` reads it; `DELETE /webhook` clears it.

---

## Admin endpoints

All admin calls take the admin token in `Authorization`, raw. A missing or
wrong one answers `401` without `code`.

### List users

```http
GET /admin/users
```

```json
{
    "success": true,
    "data": [
        {
            "id": "a1b2c3d4-...",
            "name": "Acme Bot",
            "token": "<user_token>",
            "maxUserId": 123456789,
            "webhook": "https://example.com/webhook",
            "events": "Message,ReadReceipt",
            "connected": 1,
            "authenticated": true
        }
    ]
}
```

`data` is `null` without users.

### Get a user

```http
GET /admin/users/{userid}
```

The user itself, without `success` or `data`; secrets are never returned:
`auth_configured` instead of the MAX login, no S3 keys, the proxy password
masked. Fields: `id`, `name`, `token`, `webhook`, `max_user_id`, `device_id`,
`connected` (live), `connected_flag` (stored), `auth_configured`, `events`
(array), `proxy_configured`, `proxy_url`, `history`, `s3_enabled`,
`s3_endpoint`, `s3_region`, `s3_bucket`, `media_delivery`. Errors:
`404 NOT_FOUND`, `500 INTERNAL_ERROR`.

### Create a user

```http
POST /admin/users

{ "name": "Shop 12", "webhook": "https://...", "events": "Sync,AuthExpired,Paused,Resumed" }
```

```json
{ "success": true, "id": "a1b2c3d4-...", "token": "<user_token>", "name": "Shop 12" }
```

`events` is stored as given.

### Edit a user

```http
PUT /admin/users/{userid}

{ "name": "New Name", "webhook": "https://...", "events": "All" }
```

All three fields are overwritten; a field left out becomes `""`. The change
takes effect at once (the next event goes to the new webhook).
`POST /session/connect` later overwrites `events` with its
`subscribe`. An unknown id answers `200` too.

### Delete a user

```http
DELETE /admin/users/{userid}
```

Deletes the user with its stored login and phone lookups, stops its loop and
session (closed, not logged out in MAX), and drops its token from the cache at
once: the next call with it answers `401`. A pause of its MAX account stays.
No webhook is sent. An unknown id answers `200` too.

---

## Webhook events

Subscribe with `subscribe` of `POST /session/connect`, or `events` of
`POST`/`PUT /admin/users`. **The gateway does only the work a user is
subscribed to**: a MAX notification the user is not subscribed to is dropped
before it is parsed, the media of an incoming message is fetched only for a
`Message` subscriber, and no event's text goes to the log.

| Event | Sent when | Fields |
|---|---|---|
| `Message` | A new message in one of the account's chats (opcode 128), the account's own included | `opcode`, `event` (MAX's payload, the text included), plus media fields of its attachments: `mediaType`, `mediaUrl`, `base64` and `mimeType` (photos, by `media_delivery`), `s3`, `videoId`, `videoToken`, `fileId`, `fileName`, `fileSize`, `audioId`, `audioUrl` |
| `MessageEdit`, `MessageDelete` | A message was edited or deleted (opcode 128 with `EDITED`, `REMOVED`) | `opcode`, `event` |
| `ReadReceipt`, `Typing`, `ReactionChange`, `ContactUpdate`, `PresenceUpdate` | MAX's notifications 130, 129, 155, 131, 132 | `opcode`, `event` |
| `ChatUpdate` | A chat changed (opcode 135) | `opcode`, `event` without the chat's `lastMessage` |
| `Disconnected` | MAX pushed opcode 3 | `opcode`, `event` |
| `Sync` | The session is logged in: a connect with a stored login, after `QRAuthorized`, after a reconnect | `maxUserID`, `reconnect`, `name`, `photo` |
| `Reconnecting` | The session dropped and is logged into again (first attempt and every 10th) | `attempt`, `max` |
| `ReconnectExhausted` | `MAX_CONNECT_RETRIES` used up | `attempts`, `reason` (`max_reconnect_attempts`) |
| `QRGenerated` | A QR login started, or a new QR replaced an expired one | `qrCodeBase64`, `qrLink`, `trackId`, `expiresAt` |
| `QRScanned` | The QR was scanned | `trackId` |
| `QRAuthorized` | The QR login is done; `Sync` follows | `authToken`: the MAX login (a secret) |
| `QRExpired` | The QR login ended without a login | `reason` when MAX refused the confirmation |
| `AuthExpired` | The session stopped and will not reconnect by itself | `reason`: `token_revoked` (MAX refused the stored login; erased), `page_blocked` (MAX blocked the account; erased), `refresh_failed` (3 refusals or `login.flood`; kept, `POST /session/connect` tries again), `logged_out` (MAX ended the session; erased). The gateway user stays in every case |
| `Paused` | The MAX account was paused | `maxUserID`, `pauseReason`, `upstreamCode` |
| `Resumed` | The pause was lifted | `maxUserID` |
| `MessageOutcome` | A send with a `ref` answered `send_failed` was found or not | `ref`, `chatId`, `delivered`, `messageId` (when delivered), `maxUserID` |
| `All` | Subscribes to every event | |

`Connected`, `FileReady`, `HistorySync` and `LoggedOut` are accepted in a
subscription and never sent.

`Paused` and `Resumed` go to every gateway user of the MAX account (by the
account stored at its last login), with a session or without.

### Payload

The body is JSON when the gateway runs with `WEBHOOK_FORMAT=json`, otherwise
a form with the same fields, nested values JSON-encoded. Every body carries
`type`, `userID` and `instanceID` (the gateway user's id), `instanceName` and
`eventTimestamp` (unix seconds, set when the delivery starts).

```json
{
    "type": "Paused",
    "maxUserID": 123456789,
    "pauseReason": "spam",
    "upstreamCode": "error.user.restricted.send",
    "userID": "a1b2c3d4-...",
    "instanceID": "a1b2c3d4-...",
    "instanceName": "Shop 12",
    "eventTimestamp": 1793001234
}
```

The body never carries the account token. Secrets it does carry, each in the
event made for it: `QRAuthorized` the MAX login, `QRGenerated` the QR,
`Message` and `MessageEdit` the texts. No other event carries a message's
text (C10). The same body goes to the global webhook and to RabbitMQ (plus
`serverHost`) when configured.

### Delivery

- Retries on a network error, `5xx`, `408` and `429`, up to
  `WEBHOOK_RETRY_COUNT` (5) attempts; the wait before attempt *n* is
  `WEBHOOK_RETRY_DELAY × 2^(n-1)` seconds (60, 120, 240, 480 s by default).
  Any other status ends the delivery.
- Each event is delivered on its own: events are not ordered.
- RabbitMQ: persistent messages, a durable queue, reconnects; with
  `RABBITMQ_EXCHANGE` a topic exchange with the event type as routing key.

---

## Health

```http
GET /health
```

```json
{ "success": true, "status": "ok", "version": "2.0.0", "uptime_seconds": 12345, "db_ok": true, "rabbitmq_ok": true, "connected_users": 42, "goroutines": 87, "mem_alloc_mb": 67, "timestamp": 1700000000 }
```

`503` with `"status": "degraded"` when the database does not answer. MAX is
not checked. No authentication.

---

## What MAX does, and what is not verified yet

The protocol facts the gateway relies on come from MAX's own web client
(`web.max.ru`, read 2026-11-05), PyMax, a recorded login answer (clasta-app's
fixture, 100 chats), and the live checks of 2026-11-05 on the owner's test
account (`docs/research/max-live-checks-2026-11.md`, results in its section 5):

- Pages of chats: opcode 53 `{marker}` answers `{chats, marker}`; a marker of
  0 means no more pages. **Live:** a marker a day ahead of now gives the
  newest chats first.
- Search: opcode 68 `{query, count, marker}` answers `{result: [{chatId,
  section, chat?, message?}], marker?}`. **Live:** it found none of the
  account's chats by title, only messages by their text; the web client
  searches titles among its loaded chats. The gateway does not use it (`q`
  filters titles).
- Photo upload: opcode 80 `{count: 1}` answers `{url}`; the upload answers
  `{"photos": {"<photoId>": {"token"}}}`; a message attaches
  `{"_type": "PHOTO", "photoToken"}`. **Live:** 12 photos go in one message,
  13 are refused (`errors.send-message.attachment.max-size-reached`); an
  `attachment` goes to another chat of the account right away and 48 minutes
  later.
- Send: opcode 64 with `notify: true`, as the web client always sends.
  **Live:** a channel post with `notify: false` is refused
  (`errors.send-message.channel-notify`); a group chat's negative id works.
- MAX keeps one message per cid. **Live:** a repeat a minute later, and
  44 minutes later, answers the first message's id.
- MAX closes the WebSocket on a request split into continuation frames, so
  the gateway writes each request as one frame (up to 64 KiB). **Live:** it
  also closes it on a text of 10000 characters (20 KB), while 4000 UTF-16
  units (12 KB of CJK) go through; MAX took 4001 units too, the gateway keeps
  the web client's 4000.
- Delete: opcode 66 `{chatId, messageIds, forMe}` answers `{messageIds}`, the
  ones deleted. **Live:** at once and 44 minutes later it deletes; an id MAX
  does not know comes back as `not_found`.
- Two WEB sessions of one account (two QR logins into two gateways) both
  stay up and both send. **Live:** watched for 11.5 hours while one of them
  sent 3896 posts.
- Sending pace. **Live:** one account sent 3896 posts with 1–5 photos each, one
  every 3–5 s to a group chat and a channel in turn (~845 an hour), 3 h 15 min
  on end and 4 h 37 min over the day: every one went through, with no pause,
  no refusal and no `MessageOutcome`. A send takes ~0.1 s to a group chat and
  ~0.4 s to a channel.
- The web client stops logging in on `login.token`, `login.blocked`,
  `login.flood` and `user.not.found`.
- The write rules of [Who may write where](#who-may-write-where). **Live:**
  a service dialog is `restricted`, the account's own group and channel are
  writable.
- `max-msg-length` 4000, `chats-page-size` 50, `edit-timeout` 7 days; MAX
  sends no captcha.

Not verified live: paging past the first page (the test account has five
chats); an `attachment` after a day; deleting for everyone after 25 h and
8 days; a repeated `ref` hours later; the write rules in a chat where only
admins write and in a channel without the right to post; a pace faster than
one send in 3 s, more than two chats, or a burst from a fresh session (the
ban of 2026-10-27 came after one).
