> Research note of 2026-11-05 for maxapi 2.0.0 (task M15). The live checks run
> only from the owner's test MAX account, in chats the owner names, on the
> owner's word. The first run was on 2026-11-05 (account 559563326, QR login
> into a local gateway; chats "Maxapi group" and "Maxapi channel", both the
> owner's): results in section 5. The same day the limits run went 3896 sends
> without a refusal, and the owner ended the checks; the 25 h and 8 day
> checks were not made.

# MAX: what the gateway relies on, and the live checks

## 1. Sources

- **MAX's web client**, `https://web.max.ru/`, read 2026-11-05: the SvelteKit
  bundle `/_app/immutable/chunks/DPMqYNCb.js` (the protocol layer: opcode
  calls, zod schemas of payloads, chat classes) and `nodes/0.mJu5cFch.js`
  (error handling). Public static files, read as published; the names change
  with every release.
- **PyMax** (`../PyMax`): opcode numbers; it calls neither opcode 53 nor 68.
- **A recorded login answer**: `../clasta-app/backend/tests/fixtures/max_messages/sync_data.json`
  (100 chats, 257 contacts; ids scrubbed, so dialog ids there do not follow
  the XOR rule).
- **Production**: the ban of 2026-10-27 (`error.user.restricted.send`, after a
  burst from a fresh session; commit `fb23e6d`).

## 2. Known from the web client (not live-checked)

| Topic | What MAX's web client does | Where the gateway uses it |
|---|---|---|
| Chat list | `send(53, {marker})`; the answer `{chats, marker}`; `this.marker = answer.marker \|\| 0`, and 0 means no more pages. The first page is LOGIN's `chats`, the next one starts at LOGIN's `chatMarker`, which in the recording equals the `lastEventTime` of the oldest chat sent (1775733724991) | `maxclient/chats.go` `GetChatsList`; `chats.go` asks the first page with a marker one day ahead of now |
| Search | `send(68, {query, count: 30, marker})`; the answer `{result: [{chatId, section, chat?, message?, highlights}], marker?}`; sections `CHATS`, `MESSAGES`, `PUBLIC_CHATS`, `BOTS`; a result without `chat` is fetched by CHAT_INFO | not used: live it found no chat by title (L3); `q` filters titles per page |
| Chat info | `send(48, {chatIds})`; ids MAX leaves out are treated as removed chats | `GetChatInfoWithin`, `missing` of `GET /chat/info` |
| Who may write | `viewerCanWrite`: base `!restrictions.get(RESTRICTION_INPUT=64) && participants.has(me)`; group chat: `status` ACTIVE/HIDDEN → base, REMOVED/BLOCKED → no (LEFT, CLOSED count as REMOVED); channel (`type: CHANNEL`): base and admin `permissions` has `FLAG_POST_MESSAGE=256` or `FLAG_POST_EDIT_DELETE_MESSAGE=1`; dialog: person's `accountStatus` 0 (1 blocked, 2 removed) and contact not `BLOCKED`; bot dialog (`hasBots`): not suspended; Favorites (chat 0): yes | `chats.go` `writability` (the contact's `BLOCKED` status and a bot's suspension are not in the data the gateway reads) |
| Restrictions | `RESTRICTION_INVITE=1, MODIFY_ICON=2, MODIFY_TITLE=4, LEAVE_CHAT=8, PIN=16, LIVE_LOCATION=32, INPUT=64, TOGGLE_BOT=128, COMPLAINT=256, MUTE_BOT=8192`. The recording's service dialog has 8128 (INPUT set) | `maxclient/types.go` |
| Admin rights | `POST_EDIT_DELETE_MESSAGE=1, ADD_REMOVE_MEMBER=2, ADD_ADMIN=4, CHANGE_CHAT_INFO=8, PIN_MESSAGE=16, READ_ALL_MESSAGES=32, CALL=64, EDIT_LINK=128, POST_MESSAGE=256, EDIT_MESSAGE=512, DELETE_MESSAGE=1024, VIEW_STATS=2048`. Owner 4095, an admin 2303 in the recording | same |
| Photo upload | `send(80, {count: 1})` → `{url}`; upload the file there; the answer's `photos[0].token` (maxclient reads `{"photos": {"<photoId>": {"token"}}}`, which works in production); the message attaches `{_type: "PHOTO", photoToken}`; an expired upload URL is asked again | `UploadPhotoWithin`, `POST /media/photo` |
| Send | `send(64, {chatId, message: {text, cid, elements, link, attaches}, notify: true})` | `SendMessage` |
| cid | `pk.next() = max(Date.now(), prev + 1)` | `NextCID`; with a `ref` the cid is a hash of it in the same range (`refCID`) |
| Delete | `send(66, {chatId, messageIds, forMe: !forAll})`; the answer `{messageIds}`: those deleted, the rest rolled back | `DeleteMessages`, `results` of `POST /chat/delete` |
| History | `send(49, {chatId, from, forward, backward, getMessages: true})` | `GetChatHistoryWithin`, the `MessageOutcome` look |
| Text length | drafts are cut into `chunk(max-msg-length)` pieces | 4000 UTF-16 units, as the web client cuts drafts (L9) |
| Login refusals | on LOGIN errors `login.token`, `login.blocked`, `login.flood`, `user.not.found` the client drops the login, sends LOGOUT and stops | `maxclient/errors.go`, `stopAfterLoginFailure` |
| Error codes seen in the client | `chat.denied` and `channel.denied` (no access to a chat or channel), `not.found`, `user.not.found`, `service.unavailable`, `too.many.public.channels`, `error.add.self`, `error.invalid-phone`, `file.unsafe.download`, `file.unavailable`; client-side `chat.write.denied` | `codes.go` |
| Server config | `max-msg-length` 4000, `chats-page-size` 50, `edit-timeout` 604800 s, `image-size` 1e7 (web default), `file-upload-max-size` 4 GiB, `min-image-side-size` 64 (recording) | docs |

Nothing in the client or the recording says how long a photo token lives,
how long a message can be deleted for everyone, or what MAX's sending
limits are.

## 3. The live checks

Rules for every check:

- Only the owner's **test** MAX account, logged into a local or staging
  gateway by QR; only chats the owner creates or names for the test; only
  after the owner says go.
- Before the limits run the session must **rest**: the first ban came after a
  burst from a fresh session. Log in, then leave the session up for at least
  a day with no sends before the run.
- Subscribe the test account to `All` and keep the webhook log: `Paused`,
  `AuthExpired` and `MessageOutcome` are part of the result.
- Write every MAX error code seen into the results, with the call it came on.

| # | Check | How | What it decides |
|---|---|---|---|
| L1 | The first page of the chat list | `GET /chat/list` right after login; compare with the MAX app's list (order, count ~50, `nextMarker`); page to the end | Whether a marker ahead of now gives the newest page (else: marker 0, or LOGIN's chats) |
| L2 | A group chat's id on send | `GET /chat/list?type=chat`, send to its `chatId` (negative) | The send path for chats |
| L3 | Search | `GET /chat/list?q=<part of a title>`; also a word found only in a message's text | That `CHATS` hits come, `MESSAGES` are dropped |
| L4 | 5 photos in one message, then 10, then 11 | `POST /media/photo` ×11, send 5, 10, then 11 `attachments` (11 is refused by the gateway; to see MAX's own limit, raise `maxAttachments` in a local build) | `maxAttachments`; MAX's code for too many |
| L5 | A photo token in other chats and later | Upload once; send it to chat A, chat B, chat A again after 1 h, after 25 h | Whether to upload once per account (R20) and how long to keep tokens; MAX's code for an expired token (`attachment_rejected` markers) |
| L6 | Deleting for everyone over time | Send 4 messages; delete one at once, one after 1 h, one after 25 h, one after 7 days (8 days) | `too_old`: the window and MAX's code; whether unlisted ids come back as `not_found` |
| L7 | The write rule | A group chat where only admins write (the account a member, not admin); a channel where the account is a member; one where it is an admin without the post right | `canWrite` matches what MAX allows; MAX's code when it does not (`address_refused` codes) |
| L8 | Repeated cid | Send twice with the same `ref` to one chat, minutes apart; then hours apart | Whether a repeat with the same `ref` is safe (C8); if MAX drops it, what it answers |
| L9 | Text length | Send 4000 Cyrillic letters; 2000 emoji (4000 UTF-16 units); 2001 emoji with a local build that lets them through | Counting in UTF-16 or in characters; MAX's code for too long (`message_rejected` markers) |
| L10 | Two WEB sessions | Log the test account into two gateway instances (two QR logins), send from both | Whether MAX drops one session (LoggedOut/opcode 3) or flags the account; whether retail and Sadpos can share an account |
| L11 | Limits (R38) | Section 4 | How fast the projects may send (the gateway does not pace sends) |

## 4. The limits run (R38)

As the 12-hour VK run (`../vkapi/docs/research/vk-messaging-limits-2026-10.md`,
"Measured"): a rested test account, 10 group chats the owner made for it, one
post per message with 1 to 5 photos and a text like a collection post, the
next chat in turn, a random pause of 3 to 5 s between any two sends, for
hours. `max_limits_run.py` (next to this note, standard library only) does it
through the gateway's API:

```bash
GATEWAY=http://localhost:5555 TOKEN=<test account token> \
python3 docs/research/max_limits_run.py \
    --chats=-70000000000001,-70000000000002,... \
    --photos https://cdn.example.com/1.jpg,https://cdn.example.com/2.jpg \
    --hours 12 --pause 3-5 --out max-run.csv
```

- It uploads each photo once and sends the tokens; its own random pause is
  the only one (the gateway does not pace sends).
- It writes one CSV row per send: time, chat, HTTP status, `code`,
  `upstreamCode`, the answer's latency, attachments.
- It stops at once on a pause (`paused` in any answer), on `AuthExpired`
  (a 503 that does not go away), or on any `upstreamCode` it has not seen
  before twice in a row, and prints the counts.
- Afterwards: count the sends per hour, the codes, every `send_failed` and its
  `MessageOutcome`; check that the account still sends from the MAX app.

The run tells the projects that use the gateway how fast they may send: the
lower bound of the pause that went through without a stop signal, as for VK.
The gateway itself has no floor (owner's decision of 2026-11-05).

## 5. Results

Run of 2026-11-05, 05:25–05:50 (UTC+4), account 559563326 ("Max"), QR login
into a local gateway built from the working tree; group chat
-80897482330686 "Maxapi group", channel -80897473221182 "Maxapi channel".
About 30 sends, 30 photo uploads, 5 session drops (each the gateway's own
fault or a probe, see L9), no pause, no `AuthExpired`.

| # | Date | Result | Changes made |
|---|---|---|---|
| L1 | 2026-11-05 | A marker a day ahead gives the newest chats first; MAX sent 5 chats (4 rows: Favorites is left out), marker 0, no `nextMarker`. Paging past page one not checked: the account has 5 chats | none |
| L2 | 2026-11-05 | A text to the group's negative id went through (200, `messageId`) | none |
| L3 | 2026-11-05 | Opcode 68 found **none** of the account's chats by title ("Maxapi", "Maxapi channel", "group", the contact's name, "Безопасность" all gave `result: []`); "Max" found one message by its text (`section: MESSAGES`). MAX's web client searches titles in its own loaded chats (`G.chats.searchIndex`) and calls 68 only for messages and 60 for public chats | `q` filters each page's rows by title; opcode 68 is no longer used (`chats.go` `titled`) |
| L4 | 2026-11-05 | 5, 10, 11 and 12 photos in one message went through; 13, 15, 20 and 30 were refused with `errors.send-message.attachment.max-size-reached` (answered as `attachment_rejected`). Each upload ~0.17 s | `maxAttachments` 10 → 12 |
| L5 | 2026-11-05 | The same 3 `attachment`s sent to the group went to the channel a minute later; tokens uploaded at 05:34 went to the group and the channel again at 06:21 (48 min). After 25 h: not checked | none |
| L6 | 2026-11-05 | Deleted at once: `deleted`; an id MAX does not know came back `not_found`; deleted 44 min after the send (06:21): `deleted`. Two messages wait for the 25 h and 8 day deletes (ids 117561271714459536, 117561272047588832, sent 05:37) | none |
| L7 | 2026-11-05 | The account's own group and channel are `canWrite: true`, the service dialog "Безопасность" `restricted` (as the web client's rule). A channel post with `notify: false` was refused with `errors.send-message.channel-notify`; with `notify: true` it went through. A chat where only admins write and a channel without the post right: not checked (none on the account) | `notify` defaults to true, as the web client sends every message |
| L8 | 2026-11-05 | A send repeated with the same `ref` 65 s later, and again 44 min later (06:22), answered the first message's `messageId` (117561273365456076); the history holds one message. 25 h apart: not checked | API.md: a repeat within minutes is safe |
| L9 | 2026-11-05 | 4000 Latin letters (4 KB) dropped the WebSocket (close 1006) and the message was not created (`MessageOutcome` `delivered: false` a minute later): gorilla/websocket wrote any request over its 4 KiB buffer as continuation frames, and MAX closes the connection on those. After the fix: 4000 Cyrillic letters, 2000 emoji (4000 UTF-16 units), 4000 CJK (12 KB) went through. A probe build took 4001 units and 2001 emoji too; 10000 Cyrillic letters (20 KB, one frame) dropped the connection again | Write buffer 64 KiB, a longer request refused as `ErrFrameTooLarge` (`maxclient/client.go`); the 4000-unit limit kept (the web client's `max-msg-length`) |
| L10 | 2026-11-05 | The owner logged the same account into a second local gateway by QR at 06:20 (the first session up since 05:25): both sessions stayed connected, both read the chat list, the second one sent to the group at 06:22 (200) while the first one kept its session. Watched every 10 min until 17:53 while the first one sent 3896 posts: 73 of 73 looks found the second session connected and logged in; the first one missed one look (07:42), a local port clash, not MAX. Same machine and IP for both | none |
| L11 | 2026-11-05 | 3896 posts, every one 200, no `code`, no `upstreamCode`, no `MessageOutcome`, `Paused`, `AuthExpired` or `Reconnecting` (see "Measured" below) | none |

### Measured (L11)

Started 06:24 without the day of rest (owner's word), an hour after the QR
login (05:25): the group and the channel in turn, a random 3–5 s pause between
sends, 1–5 photos per post (10 tokens uploaded once at the start of each
part), `notify` true, a fresh `ref` each.

| Part | Time | Posts | Answers |
|---|---|---|---|
| 1 | 06:24–07:38 | 1042 | all 200 |
| 2 | 14:36–17:51 | 2743 | all 200 |
| 3 | 17:53–18:01 | 111 | all 200 |

- 3896 posts with 11748 photos, 3896 distinct `messageId`s; 839–847 posts in
  each full hour (15:00, 16:00), that is ~420 an hour into each chat.
- Latency: the group median 0.11 s (p95 0.18 s, max 3.15 s), the channel
  0.41 s (p95 0.51 s, max 3.43 s).
- The stops between the parts were local: an SSH tunnel on the same Mac took
  `127.0.0.1:5555`, so the calls reset before they reached the gateway (3
  rows of status 0, not MAX's). `max_limits_run.py` now retries a call that
  did not reach the gateway and stops after 5 in a row.
- The owner ended the run at 18:01 as stable. Both sessions were logged out.

**What the projects may take from it:** one account sending one post every
3–5 s, to two chats in turn, held for 3 h 15 min on end and 4 h 37 min over the day
without a sign from MAX. A faster pace, more chats, other accounts, and a
burst with no pause (the ban of 2026-10-27) were not measured.

Also seen: the pause, set by hand in the local database, let the list,
history, mark read and `/user/info` through while a send answered `422`
paused, an edit and a photo upload `409 page_paused`; `/session/resume` sent
`Resumed`. `MessageOutcome` came a minute after each `send_failed` with the
right `delivered`.
