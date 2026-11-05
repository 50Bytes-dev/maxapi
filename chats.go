package main

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maxapi/maxclient"
)

// The account's chats (C1, C2): GET /chat/list and GET /chat/info give each
// chat as one row, with the chatId a send takes, and never a message's text
// unless asked for (include=lastMessage).

const (
	// chatListTimeout bounds MAX's answer to a page of the list.
	chatListTimeout = 4 * time.Second
	// chatInfoTimeout bounds MAX's answer to CHAT_INFO.
	chatInfoTimeout = 2500 * time.Millisecond
	// chatContactsTimeout bounds MAX's answer to the contacts behind a page's
	// dialogs (their names and photos).
	chatContactsTimeout = 2500 * time.Millisecond
	// maxChatInfoIDs is the most chats of one GET /chat/info.
	maxChatInfoIDs = 100
	// maxChatQuery bounds q, in characters.
	maxChatQuery = 200
	// firstPageLead is how far ahead of now the first page's marker lies: the
	// marker is a lastEventTime the page starts before, so a marker in the
	// future starts at the newest chat whatever the clocks.
	firstPageLead = 24 * time.Hour
)

// Values of a row's type.
const (
	ChatKindUser    = "user"    // a dialog with a person or a bot
	ChatKindChat    = "chat"    // a group chat
	ChatKindChannel = "channel" // a channel
)

// Values of cantWriteReason, by the rules the MAX web client writes by.
const (
	CantWriteRestricted = "restricted"   // MAX lets the account not write here (a service chat, a restricted chat)
	CantWriteNotMember  = "not_member"   // the account is not a member
	CantWriteChatClosed = "chat_closed"  // the chat is removed, blocked, left or closed
	CantWriteNotAdmin   = "not_admin"    // a channel the account may not post in
	CantWriteUserBlock  = "user_blocked" // the person's MAX account is blocked
	CantWriteUserGone   = "user_removed" // the person's MAX account is deleted
)

// chatRow is one chat of GET /chat/list and GET /chat/info.
type chatRow struct {
	ChatID          int64              `json:"chatId"`
	Type            string             `json:"type"`
	UserID          int64              `json:"userId,omitempty"`
	Title           string             `json:"title"`
	MembersCount    int                `json:"membersCount,omitempty"`
	Photo           string             `json:"photo,omitempty"`
	CanWrite        bool               `json:"canWrite"`
	CantWriteReason string             `json:"cantWriteReason,omitempty"`
	UnreadCount     int                `json:"unreadCount"`
	LastMessageID   string             `json:"lastMessageId,omitempty"`
	LastMessageDate int64              `json:"lastMessageDate,omitempty"`
	LastMessage     *maxclient.Message `json:"lastMessage,omitempty"`
	Participants    []int64            `json:"participants,omitempty"`
}

// chatIncludes says which heavy fields a caller asked for.
type chatIncludes struct {
	lastMessage  bool
	participants bool
}

// parseIncludes reads include=lastMessage,participants (or repeated).
func parseIncludes(r *http.Request) (chatIncludes, bool) {
	var inc chatIncludes
	for _, v := range multiValues(r, "include") {
		switch v {
		case "lastMessage":
			inc.lastMessage = true
		case "participants":
			inc.participants = true
		default:
			return inc, false
		}
	}
	return inc, true
}

// multiValues splits every value of query parameter name by commas.
func multiValues(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// chatKind maps MAX's chat type to a row's type, as the web client tells a
// chat's class: DIALOG a person or a bot, CHAT a group chat, CHANNEL a
// channel.
func chatKind(c *maxclient.Chat) string {
	switch c.Type {
	case maxclient.ChatTypeDialog:
		return ChatKindUser
	case maxclient.ChatTypeChannel:
		return ChatKindChannel
	case maxclient.ChatTypeChat:
		return ChatKindChat
	}
	if c.ID > 0 {
		return ChatKindUser
	}
	return strings.ToLower(string(c.Type))
}

// dialogPartner is the other person of a dialog: from its participants, else
// from its id (a dialog's id is the XOR of its two members).
func dialogPartner(c *maxclient.Chat, me int64) int64 {
	for k := range c.Participants {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil && id != me {
			return id
		}
	}
	if me != 0 && c.ID != 0 {
		return c.ID ^ me
	}
	return 0
}

// chatStatus folds MAX's chat status the way the web client does: LEFT and
// CLOSED count as REMOVED, an unknown one as ACTIVE.
func chatStatus(c *maxclient.Chat) string {
	switch c.Status {
	case "ACTIVE", "HIDDEN", "REMOVED", "BLOCKED":
		return c.Status
	case "LEFT", "CLOSED":
		return "REMOVED"
	}
	return "ACTIVE"
}

// writability tells whether the account me may write in chat c, and why not,
// by the rules of the MAX web client (viewerCanWrite):
//   - a group chat or channel that is removed, blocked, left or closed: no;
//   - every chat: not when its restrictions carry RESTRICTION_INPUT, and not
//     when the account is not among its participants;
//   - a channel: only its owner, or an admin with the right to post;
//   - a dialog with a person: not when the person's MAX account is blocked
//     or deleted (partner, when known).
func writability(c *maxclient.Chat, me int64, partner *maxclient.User) (bool, string) {
	kind := chatKind(c)
	if kind != ChatKindUser {
		if st := chatStatus(c); st == "REMOVED" || st == "BLOCKED" {
			return false, CantWriteChatClosed
		}
	}
	if c.Restrictions&maxclient.RestrictionInput != 0 {
		return false, CantWriteRestricted
	}
	if _, member := c.Participants[strconv.FormatInt(me, 10)]; !member {
		return false, CantWriteNotMember
	}
	if kind == ChatKindChannel && c.Owner != me {
		admin, ok := c.AdminParticipants[strconv.FormatInt(me, 10)]
		if !ok || admin.Permissions&(maxclient.AdminPermPostMessage|maxclient.AdminPermPostEditDelete) == 0 {
			return false, CantWriteNotAdmin
		}
	}
	if kind == ChatKindUser && partner != nil {
		switch partner.AccountStatus {
		case maxclient.AccountStatusBlocked:
			return false, CantWriteUserBlock
		case maxclient.AccountStatusRemoved:
			return false, CantWriteUserGone
		}
	}
	return true, ""
}

// projectChats turns MAX's chats into rows, the account's own Favorites
// (chat 0, no address for a send) left out. The people behind the dialogs
// are asked from MAX in one call (opcode 32) for their names and photos,
// unless the client already knows them. A MAX error on that call comes back
// as err; any other failure leaves the dialogs without a title.
func projectChats(client *maxclient.Client, chats []maxclient.Chat, inc chatIncludes) ([]chatRow, error) {
	me := client.MaxUserID
	var partnerIDs []int64
	for i := range chats {
		if chatKind(&chats[i]) == ChatKindUser && chats[i].ID != 0 {
			if p := dialogPartner(&chats[i], me); p != 0 {
				partnerIDs = append(partnerIDs, p)
			}
		}
	}
	partners := map[int64]*maxclient.User{}
	var callErr error
	if len(partnerIDs) > 0 {
		users, err := client.GetUsersWithin(partnerIDs, chatContactsTimeout)
		if _, refused := maxclient.ServerErrorCode(err); refused {
			callErr = err
		}
		for i := range users {
			partners[users[i].ID] = &users[i]
		}
	}

	rows := make([]chatRow, 0, len(chats))
	for i := range chats {
		c := &chats[i]
		if c.ID == 0 {
			continue
		}
		row := chatRow{ChatID: c.ID, Type: chatKind(c), UnreadCount: c.NewMessages}
		var partner *maxclient.User
		if row.Type == ChatKindUser {
			row.UserID = dialogPartner(c, me)
			partner = partners[row.UserID]
			if partner != nil {
				row.Title = maxclient.DisplayName(partner.Names)
				row.Photo = partner.BaseURL
			}
		} else {
			row.Title = c.Title
			row.MembersCount = c.ParticipantsCount
			row.Photo = c.BaseIconURL
		}
		row.CanWrite, row.CantWriteReason = writability(c, me, partner)
		if c.LastMessage != nil {
			row.LastMessageID = c.LastMessage.ID
			row.LastMessageDate = c.LastMessage.Time / 1000
			if inc.lastMessage {
				row.LastMessage = c.LastMessage
			}
		}
		if inc.participants {
			row.Participants = make([]int64, 0, len(c.Participants))
			for k := range c.Participants {
				if id, err := strconv.ParseInt(k, 10, 64); err == nil {
					row.Participants = append(row.Participants, id)
				}
			}
			sort.Slice(row.Participants, func(a, b int) bool { return row.Participants[a] < row.Participants[b] })
		}
		rows = append(rows, row)
	}
	return rows, callErr
}

// refuseWhilePaused answers 409 page_paused with the pause when the user's
// MAX account is paused: a call that makes the account act, other than a
// send or a delete, makes no MAX call then. It returns the MAX account.
func (s *server) refuseWhilePaused(w http.ResponseWriter, r *http.Request, userID string) (int64, bool) {
	maxUserID := s.maxUserIDOf(userID)
	if pause := s.pauseOf(maxUserID); pause != nil {
		s.respondDelivery(w, r, http.StatusConflict, CodePagePaused,
			"the account is paused until the owner resumes it", nil, pause)
		return maxUserID, true
	}
	return maxUserID, false
}

// respondMAXFailure answers a MAX call other than a send that failed with
// err: a stop signal pauses the account (409 page_paused with upstreamCode),
// any other MAX error is 502 upstream_error with upstreamCode, and no answer
// at all 503 not_connected.
func (s *server) respondMAXFailure(w http.ResponseWriter, r *http.Request, userID string, maxUserID int64, err error) {
	if _, refused := maxclient.ServerErrorCode(err); refused {
		if pause := s.pauseOnLimit(userID, maxUserID, err); pause != nil {
			s.respondDelivery(w, r, http.StatusConflict, CodePagePaused, "MAX refused the call; the account is paused", err, pause)
			return
		}
		s.respondDelivery(w, r, http.StatusBadGateway, CodeUpstreamError, "MAX refused the call", err, nil)
		return
	}
	if errors.Is(err, maxclient.ErrInvalidResponse) {
		s.respondDelivery(w, r, http.StatusBadGateway, CodeUpstreamError, "MAX's answer is unreadable", err, nil)
		return
	}
	s.respondDelivery(w, r, http.StatusServiceUnavailable, CodeNotConnected, "MAX did not answer", err, nil)
}

// titled keeps the rows whose title holds q, in any letter case.
func titled(rows []chatRow, q string) []chatRow {
	q = strings.ToLower(q)
	kept := rows[:0]
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Title), q) {
			kept = append(kept, row)
		}
	}
	return kept
}

// ListChats is GET /chat/list: a page of the account's chats, the newest
// first (CHATS_LIST, opcode 53); marker is the nextMarker of the previous
// page. type keeps the rows of the given kinds of the page, q the rows whose
// title holds q. MAX's own search (opcode 68) is not used: live, it found no
// chat of the account by its title, only messages by their text, and MAX's
// web client searches titles in the chats it has loaded, as q does here
// (docs/research/max-live-checks-2026-11.md, L3).
func (s *server) ListChats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")

		client, ok := s.liveClient(w, r, userID)
		if !ok {
			return
		}
		inc, ok := parseIncludes(r)
		if !ok {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "include takes lastMessage and participants")
			return
		}
		kinds := map[string]bool{}
		for _, k := range multiValues(r, "type") {
			if k != ChatKindUser && k != ChatKindChat && k != ChatKindChannel {
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "type takes user, chat and channel")
				return
			}
			kinds[k] = true
		}
		var marker int64
		if v := strings.TrimSpace(r.URL.Query().Get("marker")); v != "" {
			m, err := strconv.ParseInt(v, 10, 64)
			if err != nil || m <= 0 {
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "marker is not a nextMarker")
				return
			}
			marker = m
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if utf8.RuneCountInString(q) > maxChatQuery {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "q is over 200 characters")
			return
		}
		// A pause does not hold a read (generic.go, readCall).
		maxUserID := s.maxUserIDOf(userID)

		if marker == 0 {
			marker = time.Now().Add(firstPageLead).UnixMilli()
		}
		page, err := client.GetChatsList(marker, chatListTimeout)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		chats, next := page.Chats, page.Marker

		if len(kinds) > 0 {
			kept := chats[:0]
			for _, c := range chats {
				if kinds[chatKind(&c)] {
					kept = append(kept, c)
				}
			}
			chats = kept
		}
		rows, err := projectChats(client, chats, inc)
		if err != nil && s.pauseOnLimit(userID, maxUserID, err) != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		if q != "" {
			rows = titled(rows, q)
		}
		body := map[string]interface{}{"success": true, "items": rows}
		if next > 0 {
			body["nextMarker"] = strconv.FormatInt(next, 10)
		}
		s.Respond(w, r, http.StatusOK, body)
	}
}

// ChatInfo is GET /chat/info?chatId=..: the rows of up to 100 chats by id, in
// the order asked (CHAT_INFO, opcode 48). The chats MAX does not show the
// account are listed in missing.
func (s *server) ChatInfo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")

		client, ok := s.liveClient(w, r, userID)
		if !ok {
			return
		}
		inc, ok := parseIncludes(r)
		if !ok {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "include takes lastMessage and participants")
			return
		}
		var ids []int64
		seen := map[int64]bool{}
		for _, v := range multiValues(r, "chatId") {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id == 0 {
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "chatId is not a chat id")
				return
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 || len(ids) > maxChatInfoIDs {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "chatId takes 1 to 100 chat ids")
			return
		}
		// A pause does not hold a read (generic.go, readCall).
		maxUserID := s.maxUserIDOf(userID)

		found, err := client.GetChatInfoWithin(ids, chatInfoTimeout)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		byID := map[int64]maxclient.Chat{}
		for _, c := range found {
			byID[c.ID] = c
		}
		ordered := make([]maxclient.Chat, 0, len(ids))
		missing := []int64{}
		for _, id := range ids {
			if c, ok := byID[id]; ok {
				ordered = append(ordered, c)
			} else {
				missing = append(missing, id)
			}
		}
		rows, err := projectChats(client, ordered, inc)
		if err != nil && s.pauseOnLimit(userID, maxUserID, err) != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.Respond(w, r, http.StatusOK, map[string]interface{}{"success": true, "items": rows, "missing": missing})
	}
}
