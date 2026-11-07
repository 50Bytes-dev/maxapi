# maxapi 2.0.0 for Sadpos

## 3.0.0 (2026-11-07): the QR login

3.0.0 answers what Sadpos saw in production on 2026-11-07 (90 of 97 QR
confirmations failed, orphan sessions after deletes, 500 on
`GET /session/qr`). The contract is `API.md`, "What changed in 3.0.0" and
"Login at a glance"; the research is
`docs/research/max-qr-login-2026-11-07.md`. What Sadpos changes:

- **Cloud password.** The failed confirmations were accounts with a cloud
  password. Subscribe to `QRPasswordRequired` (fields `hint`, `email`); on it
  (or on `GET /session/qr` `409 password_required`) show the owner a
  password field and send it with `POST /session/password {"password"}`.
  `200 accepted` → `QRAuthorized` and `Sync` follow; `422 invalid_password`
  → ask again; `410` → the login ended. Never store or log the password.
- **Codes instead of 500.** `GET /session/qr`: `409 already_logged_in`,
  `409 password_required`, `410 login_expired` / `login_failed` /
  `login_unsupported`. `QRExpired.reason` is the same code, always present;
  `login_unsupported` names MAX's step in `step`: show it, it is the case to
  report.
- `POST /session/connect` without a login answers `503 not_connected` or
  `503 login_unavailable` when no QR could be issued (was `500`).
- A drop during the login shows a new QR by itself (`QRGenerated`): show the
  latest one.
- `DELETE /admin/users/{id}` and `POST /session/disconnect` now stop the
  login and session for good; nothing else to do.
- `GET /session/status` `connected` is `false` during a QR login.
- After `AuthExpired` `logged_out` (MAX ended the session), `POST
  /session/connect` starts a QR login again; in 2.0.0 it answered `500` "DB
  error" and the shop could not log in again until its gateway user was
  deleted and created anew.
- An owner who sets a cloud password in the MAX app ends the gateway's
  session (live, 2026-11-07): `AuthExpired` `logged_out`, then a QR login
  with the password.

Checked live on 2026-11-07 on the test account: QR login without and with a
cloud password (a wrong one answered `422 invalid_password`, the right one
logged in), a delete during the login (no socket left), the device shown as
"WEB, Chrome, macOS".

Pins:

| What | Pin |
|---|---|
| Image | `sobytes/maxapi:3.0.0` (linux/amd64), built from `eedfd8b` |
| SDK | `maxapi-python` 6.0.0: `{ git = "https://github.com/50Bytes-dev/maxapi-python.git", rev = "200e822e40d37bb8dc47503e9a85fe1e9b33fd82" }` |
| Schema | `/api/spec.yml` of the image (`info.version` 6.0.0) |

The rest of this note is the 2.0.0 hand-off.

> Hand-off of 2026-11-05 for the Sadpos agent: the plan
> `../Sadpos/docs/plans/2026-11-03-1441-feat-messenger-chat-export-plan.md`
> describes maxapi 1.5.1 in "Состояние шлюзов" and keeps R38 open for MAX.
> Both changed. The contract is `API.md`; this note only maps the plan to it.
> maxapi does not change Sadpos: the Sadpos agent does.

## What to pin

| What | Pin |
|---|---|
| Image | `sobytes/maxapi:2.0.0` (linux/amd64, index digest `sha256:86d22ff23213026526271654dd8af3cf7cdcb26febef75060fcae09821a24872`). Not `1.4.1`, not `latest` (no such tag) |
| SDK | `maxapi-python` 5.0.0: `{ git = "https://github.com/50Bytes-dev/maxapi-python.git", rev = "903d14b9809c160a4fde59ced91fc235de9ac30d" }`, the way retail pins 4.0.1 |
| Schema | `/api/spec.yml` of the image (`info.version` 5.0.0), Swagger UI at `/api/` |

Retail stays on 1.5.1 until its own agent moves it (`API.md`, "What changed
in 2.0.0").

## Compose (R34)

As retail's `maxapi-server` in `../retail/web.yml`, with the image above:
SQLite in a volume on `/app/dbdata` (the logins outlive the container),
`MAXAPI_ADMIN_TOKEN`, `WEBHOOK_FORMAT=json`, `TZ`, its own port. Pass
`POST /media/photo` the photos' public URLs, the ones the site shows.

- `-skipmedia` is not needed for R4: subscribe the account only to the
  session events below; the gateway drops a notification nobody is
  subscribed to before parsing it, fetches media only for `Message`
  subscribers, and logs no message text. Tokens and phones are masked in its
  log.

One MAX account may be logged into retail's gateway and Sadpos's at once:
two WEB sessions of one account both stayed up and both sent (live check
L10, 11.5 h).

## The plan's requirements in 2.0.0

| Plan | 2.0.0 |
|---|---|
| R2, QR login | `POST /admin/users` (with `events`), `POST /session/connect` → `QRGenerated` / `QRScanned` / `QRAuthorized` → `Sync`. The login outlives a restart. A logout or MAX ending the session keeps the gateway user and its token: connect again on the same token |
| R4, only the list of chats | `GET /chat/list?type=chat,channel` (pages by `nextMarker`, the newest first, `q` filters titles), `GET /chat/info?chatId=...` (up to 100, `missing` for chats MAX no longer shows). No message text unless `include=lastMessage` |
| Who may write (R6, R33) | `canWrite`, `cantWriteReason` (`chat_closed`, `restricted`, `not_member`, `not_admin`) per row; a send there answers `403 address_refused` |
| R16–R18, a post | `POST /chat/send/text` with `chatId` (negative for group chats and channels), `text` up to 4000 UTF-16 units (no markdown), `attachments` up to 12 (MAX refuses 13), `ref`. `notify` defaults to true: MAX refuses a channel post without it |
| R20, R22, photos once | `POST /media/photo` `{url}` → `attachment`; the same `attachment` goes to every chat of the account. Live: other chats at once and 48 min later; a day later not checked. `422 attachment_rejected` on a send: upload again |
| Retries | `ref` makes the message's cid: a repeat with the same `ref` answered the first message's `messageId` and made no second message (1 min and 44 min later). After `500 send_failed` wait for `MessageOutcome` a minute later: `delivered: true` carries the `messageId`; on `delivered: false` resend with the same `ref` |
| R19, delete | `POST /chat/delete` `{chatId, messageIds: [strings]}`, for everyone, up to 100; `results` per id: `deleted`, `not_found`, `too_old`, `forbidden`. Live: deleted at once and 44 min later. MAX's window is not known (a day and a week later not checked) |
| R24, R25, R38, pace | The gateway does not pace sends: Sadpos holds the pause per account. Live L11: one account, one post every 3–5 s (1–5 photos each) to a group chat and a channel in turn, 3896 posts, 3 h 15 min on end, 4 h 37 min over the day: every one 200, no pause, no refusal. So the VK bounds (3–5 s) hold for MAX as measured; faster, more chats, or a burst from a fresh session were not measured |
| R27, captcha | MAX has none |
| R28, block | A stop signal of MAX (`restricted`, `too.many`, `flood`, `spam`, `limit` codes) pauses the whole MAX account: the refused call answers with `paused: true` and `upstreamCode` (a send or delete `422 send_rejected`, others `409 page_paused`), `Paused` goes out once. While paused every act is refused without a MAX call; reads (list, info) go on. Only `POST /session/resume` ends it (`Resumed`). A block at login: `AuthExpired` with `reason` `page_blocked`; a revoked login `token_revoked`, MAX ending the session `logged_out` |
| R37, logs | The gateway's own log holds no texts and masks tokens; Sadpos's mask still has to cover `token` of `POST /admin/users` and `authToken` of `QRAuthorized` in JSON bodies |

Events to subscribe for the upload: `QRGenerated`, `QRScanned`,
`QRAuthorized`, `QRExpired`, `Sync`, `AuthExpired`, `Paused`, `Resumed`,
`MessageOutcome`; `Reconnecting`, `ReconnectExhausted` if Sadpos shows the
session's state.

## Codes a send can answer

`200` with `messageId` (string) and `chatId`; `400 INVALID_INPUT`,
`400 invalid_target`, `403 address_refused`, `404 address_not_found`,
`422 message_rejected` (empty, too long, over 12 attachments: final for this
message), `422 attachment_rejected`, `422 send_rejected` (MAX refused;
`paused: true` when the account is paused), `500 send_failed` (no answer: it
may have gone; never resend at once, see `ref`), `503 not_connected`,
`503 lookup_failed` (sends by `phone` only). Each with `code`, and MAX's code in `upstreamCode` when
MAX gave one. Full table: `API.md`, "Codes and errors".

## Not verified live

An `attachment` a day later; delete for everyone after a day and a week;
the write rules in a chat where only admins write and in a channel without
the right to post; paging past the first page of chats (the test account had
five); a pace faster than one send in 3 s. Details: `docs/research/max-live-checks-2026-11.md`.
