# QR login, hanging sessions, reconnects: open MAX clients (2026-11-07)

Why: on 2026-11-07 Sadpos saw in 12 hours of production (maxapi 2.0.0):

1. 90 of 97 QR confirmations failed with `Auth Error: No LOGIN token in
   response (no_login_token)`; 6 of 39 shops logged in.
2. `DELETE /admin/users/{id}` answered 200 while the user's WebSocket lived
   on (pings every 30 s, some polling the QR every 5 s): ~20 orphans in 5 h.
3. A WebSocket dropped during the QR stage (1006) was polled again and again
   (`QR poll failed: WebSocket is not connected`, `QR refresh failed`).
4. `GET /session/qr` answered `500` `no session` / `already logged in`.
5. MAX ended 2 of 8 new sessions within the first hour (`MAX ended the
   session; erasing the login`).

The clients below were read on 2026-11-07, at the commits named; nothing of
them was run. No code was copied: the gateway takes the protocol steps they
agree on (opcodes, payload keys, error codes), and names its sources in
`maxclient/auth.go`.

## 1. The repositories

| Repository | License | Language, transport | Commit read |
|---|---|---|---|
| [MaxApiTeam/PyMax](https://github.com/MaxApiTeam/PyMax) (ink-developer's PyMax) | MIT | Python; JSON v11 on `ws-api.oneme.ru` up to 2.4, binary v10 on `api.oneme.ru` since 2.5.0 | `c4cb917` (2026-10-25, 2.5.0); old copy MaxTeamAPI/PyMax `4b62bba` (2026-03-28) |
| [maxsos1/max-account-api](https://github.com/maxsos1/max-account-api) | MIT | TypeScript; JSON v11 on `ws-api.oneme.ru` | `c5c7f66` (2026-06-18) |
| [WireCatLabs/max-cli](https://github.com/WireCatLabs/max-cli) | Apache-2.0 | TypeScript; binary v10 on `api.oneme.ru`, measured against MAX 2026-09-24/25 | `d39f143` (2026-11-07) |
| [blackyblack/maxrs](https://github.com/blackyblack/maxrs) | MIT | Rust; JSON v11 on `ws-api.oneme.ru`; no QR | `3acc009` (2026-10-04) |
| [MaxApiTeam/Node-Max](https://github.com/MaxApiTeam/Node-Max) | MIT (LICENSE) / ISC (package.json) | JavaScript; JSON v11 | `b4b266a` (2026-01-03) |
| [antirek/vkmax-node](https://github.com/antirek/vkmax-node) | MIT in package.json, no LICENSE file | TypeScript; JSON v11; no QR | `566e6f0` (2025-10-09) |
| [Trollobot/Telemax](https://github.com/Trollobot/Telemax) | AGPL-3.0 | TypeScript; mobile TCP protocol | `e781aa8` (2026-11-03) |
| [MrCatchParkington/go-max-client](https://github.com/MrCatchParkington/go-max-client) | AGPL-3.0 | Go; JSON v11 | `09e2323` (2026-05-02) |
| [pr0bel1230/max-api-docs](https://github.com/pr0bel1230/max-api-docs) | CC-BY-4.0 | protocol notes | `82ff271` |

Also seen: junioraww/rumax (MIT, SMS only), turleo/max2tg (no license; a
recorded successful 291 answer), nsdkinx/vkmax issue #28, Green-API's
documentation of its commercial MAX gateway (a password step after the QR,
with `attempts_exceeded` and `time_limit_expired_logout`).

## 2. Problem 1: no LOGIN token after the QR

**What MAX answers.** An account with a cloud password (two factor
authentication) gets, on QR confirmation (opcode 291):

```json
{
  "tokenAttrs": {},
  "passwordChallenge": {
    "trackId": "…", "hint": "…", "email": "o***@mail.ru",
    "config": {"passMinLen": 6, "passMaxLen": 64, "hintMaxLen": 30}
  }
}
```

`tokenAttrs` is present and empty: exactly the `no_login_token` branch of
2.0.0 (`maxclient/auth.go:129`), which read only `tokenAttrs.LOGIN`.

- max-account-api says so in its types (`src/types.ts:105-120`: "Empty `{}`
  when 2FA cloud password is enabled and `passwordChallenge` is set
  instead") and branches on it (`src/auth.ts:115-138`).
- PyMax (`src/pymax/auth/qr.py:76-84`), Node-Max (`src/core.js:205-208`),
  the old PyMax (`mixins/auth.py:209-215`), maxrs and vkmax-node (after the
  SMS step) branch on the same shape: no `LOGIN`, a `passwordChallenge`.
- max-cli did it against live MAX on 2026-09-24: "`session start qr` on an
  account with a cloud password" (`src/spec/operations/login.ts:94-107`).
- go-max-client reads only `LOGIN` and would fail as 2.0.0 did.

**How the password goes.** Opcode 115 (`AUTH_LOGIN_CHECK_PASSWORD`) on the
same socket, `{"trackId": passwordChallenge.trackId, "password": "…"}`; the
answer is `{"tokenAttrs": {"LOGIN": {"token": "…"}}, "profile": {…}}`, the
token then logs in with LOGIN (19) and the same deviceId, as after a plain QR.

- A wrong password is refused with `password2fa.wrong` (max-cli, measured
  2026-09-24; Telemax, measured 2026-08-14). The same `trackId` takes
  another attempt; it is used up on success (Telemax).
- An old client gets `second.factor.disabled.update.app` (vkmax-node).
- The step has a time limit on MAX's side (Green-API:
  `time_limit_expired_logout`); its length is not recorded.

**Other variants.** No repository shows another key under `tokenAttrs` on
the QR path: `REGISTER` comes only after an SMS code for a number without an
account. None has a "confirm the new device" step.

**What we took.** `ConfirmQRLogin` returns a `LoginStep`: the token, a
`PasswordChallenge`, or the unsupported step named by MAX's keys. The keys of
the 291 and 115 answers go to the log, never a value (`payloadKeys`,
`tokenAttrsKeys`; Warn without a token). The gateway asks the owner for the
password (`QRPasswordRequired`, `POST /session/password`), modeled on vkapi's
`QRCodeRequired` and `POST /session/code`; anything else ends the login with
`login_unsupported` and `step`. Refusals: `password2fa.wrong` and other
password codes → `422 invalid_password` (the fifth ends the login),
`track.not.found`, an expiry or a limit → `login_expired`, `update.app` →
`login_unsupported`.

## 3. Problem 2: a deleted user's session lives on

None of the clients has the gateway's multi-user problem; each closes its
transport and blocks reconnects on `stop()` (max-account-api `closedByUser`,
PyMax `disconnect()` cancelling the ping task, max-cli clearing the ping on
loss).

**Our cause** (2.0.0):

- `removeUser` only sent the kill signal of the reconnect loop. A QR login
  had no loop and no kill channel: its client was dropped from the manager
  but never closed, so its WebSocket and ping goroutine lived until a
  restart.
- `POST /session/disconnect` closed the QR client only when it was *not*
  connected (`!IsConnected`), that is never while the QR socket was up; the
  watcher kept polling on it. Every cancelled login left one.
- `maxclient.Client.Connect` made a fresh context after `Close`, so a closed
  client could come back.
- Two connects at once each opened a socket; the loser was never closed.

**What we did.** A QR login is one goroutine per user owning its socket
(`qrlogin.go`); a stop cancels it and waits for it. `Client.Shutdown` ends a
client for good (no reconnect, waiting requests fail, safe on its own receive
goroutine). `DELETE` stops the QR login, shuts the session's client down and
kills the loop; a test checks that the fake MAX sees no open socket and no
frame afterwards.

## 4. Problem 3: a drop during the QR stage

- PyMax: a drop during auth restarts the whole flow and requests a new QR
  (288); fixed 1 s delay. max-account-api: backoff 1–30 s, SESSION_INIT and
  LOGIN with the same deviceId; no QR resume. max-cli: backoff up to 60 s,
  stops on auth, rate limit and provider errors.
- Nobody resumes a QR or a password challenge on a new socket, and nobody
  records whether a `trackId` survives a reconnect.

**Our cause.** The 2.0.0 watcher polled on the dead socket until the QR
deadline, refreshed on it (failing), then sent `QRExpired` with the error
text; each connect started another.

**What we did.** A drop makes the login connect again with the same deviceId
(backoff from 1 s, doubling, at most 5 times per login) and show a new QR
(`QRGenerated`); waiting for the password, it reconnects without a new QR and
sends the password by the challenge's `trackId` (not verified live: MAX may
refuse it, which ends the login with `login_expired`). Out of attempts:
`QRExpired` `login_failed`.

## 5. Problem 4: `GET /session/qr` answered 500

vkapi already answers `409 code_required`, `409 already_logged_in`, `410
login_expired` / `login_rejected` / `login_failed`. maxapi 3.0.0 takes the
same names: `409 password_required`, `409 already_logged_in`, `410
login_expired` / `login_failed` / `login_unsupported`; the outcome is kept
until the next connect.

## 6. Problem 5: MAX ends the session soon after login

What the clients and notes say, and what holds for us:

| Possible cause | Evidence | Our side |
|---|---|---|
| Several sockets with one deviceId | pr0bel1230 `protocol/reconnect.md`: if several connections share a deviceId, the server may close one of them | **Likely ours.** The user's `device_id` is reused for every QR attempt and for the session. Every cancelled or deleted login of 2.0.0 left a socket open with it (problem 2), so a new session ran beside orphans of its own deviceId. Fixed with problem 2; the QR socket is also shut before the session's opens |
| An incomplete device description | Every web client sends `deviceType`, `locale`, `deviceLocale`, `osVersion`, `deviceName`, `headerUserAgent`, `appVersion`, `screen`, `timezone` (PyMax `config.py:307-325`, maxrs `models.rs:9-47`, max-account-api `auth.ts:38-49`, max-cli `identity.ts:48-64`) | **Ours.** 2.0.0 sent only `deviceType`, `locale`, `appVersion 26.4.3`, and a truncated handshake User-Agent: MAX had no device or browser name to list (what the device list showed then was not seen), and the fingerprint is not a browser's. 3.0.0 sends the full set (Chrome on macOS, `appVersion 26.8.4`, as maxrs on the same endpoint) and the same User-Agent in the handshake; MAX lists it as "WEB, Chrome, macOS" (live) |
| The owner ended it from the phone | max-account-api issue #3 ("end all other sessions"); MAX pushes opcode 20 | Possible: a device without a name invites it. The gateway logs the payload of opcode 20 at Info (`logPushedFrame`): read it in the production log for the two cases |
| Many logins with one token | pr0bel1230: ~30–50 LOGINs with one token in a short time stop INIT; PyMax #106: a relogin loop after a revocation hit `error.limit.violate` | Not ours: the reconnect loop stops after 3 refusals and on `login.flood` (2.0.0) |
| Ping rate | Every client pings opcode 1 every 30 s | Same as ours |
| Server pings and pushes not answered | pr0bel1230: the client must answer server pings; max-account-api acks message pushes (opcode 128, cmd 1) "so the server stops retrying" | Not changed: the live run of 2026-11-05 held two sessions for 11.5 h without answering either. To check in the production log: "Unknown frame from MAX" with opcode 1 |
| Several sessions from one IP | L10 (2026-11-05): two WEB sessions of one account from one IP held 11.5 h | Not measured for dozens of accounts from one IP: part of the proxy task (`TASK-capacity-and-proxy.md`) |

Also fixed on the way: answers were matched to requests by `seq` alone. MAX
numbers its pushes with a counter of its own, so a push (cmd 0) carrying the
seq of a waiting request was taken for its answer; every client above matches
cmd 1/3 and the opcode too (max-account-api `transport.ts:160-185`). 3.0.0
does the same.

**Not checked:** the two production cases themselves (their opcode 20
payloads are in Sadpos's gateway log). Checked live (section 8): MAX shows
the session as "WEB, Chrome, macOS", and setting a cloud password ends the
WEB session (opcode 159, then 20).

## 7. Live check

On a test account, with a local gateway built from this tree:

1. QR login of an account without a cloud password: `QRGenerated` →
   `QRScanned` → `QRAuthorized` → `Sync`; the log line "MAX handed out the
   login token" with `payloadKeys`.
2. Set a cloud password in the MAX app (Settings → Privacy → Cloud password),
   log out the gateway (`POST /session/logout`), log in by QR again: the log
   line "MAX answered the QR confirmation without a LOGIN token" with
   `payloadKeys` `["passwordChallenge","tokenAttrs"]` (expected), the
   `QRPasswordRequired` event, `409 password_required`; a wrong password →
   `422 invalid_password` with `upstreamCode` in the log; the right one →
   `200`, `QRAuthorized`, `Sync`.
3. The device in the MAX app's device list: its name with the new
   userAgent.
4. `DELETE /admin/users/{id}` during a QR login: no socket left (`lsof -i`
   on the gateway process shows no connection to `ws-api.oneme.ru`).

Results go to section 8.

## 8. Results

Run of 2026-11-07, 14:15–14:43 (UTC+4), account 559563326 ("Max"), a local
gateway (3.0.0 from this tree, `127.0.0.1:5577`, its own SQLite, no `.env`),
webhooks to a local sink.

| # | Result |
|---|---|
| 1 | Without a cloud password: `QRGenerated` 14:15:11 → scanned → `QRScanned` 14:15:22 → `QRAuthorized`, `Sync` 14:15:23. The 291 answer: `payloadKeys ["profile","tokenAttrs"]`, `tokenAttrsKeys ["LOGIN"]`. MAX took SESSION_INIT with the full userAgent (`appVersion 26.8.4`) and LOGIN with it. One WebSocket to MAX afterwards: the login's closed before the session's opened. MAX's QR lives 120 s (`ttl` 119993); after it the poll answers `track.not.found` ("Авторизационная сессия не найдена") and the gateway issued a new QR by itself |
| 3 | The MAX app lists the session as **"WEB, Chrome, macOS"** |
| 2 | **Enabling the cloud password in the MAX app ended the gateway's WEB session**: at 14:32:04 MAX pushed opcode 159 (profile, `payloadKeys ["profile"]`) and at once opcode 20 with an empty payload. The gateway sent `AuthExpired` `logged_out`, shut the client down, no reconnect. Then `POST /session/connect` answered **`500` "DB error: … converting NULL to string"**: the erased login is NULL, the connect read it into a string. That bug is in 2.0.0 too, so in production a user MAX logged out could not log in again; fixed in 3.0.0 (`COALESCE`, test `TestConnectAfterMAXEndedTheSession`) and checked live: the same user got a QR at once |
| 2 | With the cloud password (no hint, no email set): the 291 answer `payloadKeys ["passwordChallenge","tokenAttrs"]`, `tokenAttrsKeys []` — **the production failure**; `passwordChallenge` keys `["config","trackId"]` (MAX sends `hint` and `email` only when set). `QRPasswordRequired` went out, `GET /session/qr` answered `409 password_required`. A wrong password (`wrong123`): MAX `password2fa.wrong` ("Неверный пароль, попробуйте ещё раз"), `422 invalid_password`; the right one 29 s later on the same `trackId`: `200`, the 115 answer `["profile","tokenAttrs"]` with `LOGIN`, `QRAuthorized` and `Sync` (14:41:47) |
| 4 | `DELETE /admin/users/{id}` of a second user during its QR login: the gateway's WebSockets to MAX went from 2 to 1 (the other user's session) at once and stayed 1 for 40 s; the deleted user's 3 frames (SESSION_INIT, 288, one poll) all went before the delete |
| — | No password in the gateway's log; the 291 and 115 answers logged by their keys only. MAX closes the socket (1006) right after a LOGOUT (opcode 20) the gateway sends |

The test session was logged out at the end. The test account keeps the
cloud password the owner set for the check.

**For problem 5:** the live run shows one cause MAX has on its own side:
changing the account's security (here: setting a cloud password) ends every
WEB session, with opcode 159 then 20. The two production cases may be that,
the owner ending the session from the device list, or orphans of the same
device id; their opcode 20 payloads and the frames before them (opcode 159?)
in Sadpos's gateway log tell which.
