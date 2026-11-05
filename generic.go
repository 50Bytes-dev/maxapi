package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"maxapi/maxclient"
)

// The calls besides sending, deleting and the chat list: editing, reading,
// downloads, people, groups, history, reactions. Each follows the shared
// rules (C0): no session answers 503 not_connected, a body that does not
// decode 400 INVALID_INPUT; a MAX error carries MAX's code as upstreamCode,
// and a stop signal pauses the account (respondMAXFailure). While the account
// is paused, a call that makes it act (edit, react, the phone lookup, groups)
// makes no MAX call and answers 409 page_paused (providerCall); a call that
// only reads, or marks a chat read, goes on (readCall).

const (
	// callTimeout bounds MAX's answer to one call of these.
	callTimeout = 8 * time.Second
	// fileDownloadTimeout bounds the download of a file MAX serves.
	fileDownloadTimeout = 60 * time.Second
	// maxCheckPhones and maxUserInfoIDs bound one call of /user/check and
	// /user/info.
	maxCheckPhones = 50
	maxUserInfoIDs = 100
)

// decodeBody decodes the JSON body into v, or answers 400 INVALID_INPUT.
func (s *server) decodeBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
		return false
	}
	return true
}

// providerCall opens a call that makes the account act in MAX: the live
// session, the body, then the pause. ok is false when it has answered.
func (s *server) providerCall(w http.ResponseWriter, r *http.Request, body interface{}) (userID string, client *maxclient.Client, maxUserID int64, ok bool) {
	userID = r.Context().Value("userinfo").(Values).Get("Id")
	client, ok = s.liveClient(w, r, userID)
	if !ok {
		return
	}
	if body != nil && !s.decodeBody(w, r, body) {
		return userID, client, 0, false
	}
	maxUserID, paused := s.refuseWhilePaused(w, r, userID)
	return userID, client, maxUserID, !paused
}

// readCall opens a call that only reads from MAX, or marks a chat read: the
// live session and the body. A pause does not hold it (owner's decision of
// 2026-11-05): the pause stops what the account sends and does, not what it
// reads. ok is false when it has answered.
func (s *server) readCall(w http.ResponseWriter, r *http.Request, body interface{}) (userID string, client *maxclient.Client, maxUserID int64, ok bool) {
	userID = r.Context().Value("userinfo").(Values).Get("Id")
	client, ok = s.liveClient(w, r, userID)
	if !ok {
		return
	}
	if body != nil && !s.decodeBody(w, r, body) {
		return userID, client, 0, false
	}
	return userID, client, s.maxUserIDOf(userID), true
}

func (s *server) respondOK(w http.ResponseWriter, r *http.Request, fields map[string]interface{}) {
	fields["success"] = true
	s.Respond(w, r, http.StatusOK, fields)
}

// SendEditMessage edits a message of the account.
func (s *server) SendEditMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg EditMessageBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		if msg.ChatID == 0 || msg.MessageID == 0 {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "chatId and messageId are required")
			return
		}
		if utf16Len(msg.Text) > maxMessageLength {
			s.respondError(w, r, http.StatusUnprocessableEntity, CodeMessageRejected, "the text is over 4000 characters")
			return
		}
		if _, err := client.EditMessage(msg.ChatID, int64(msg.MessageID), msg.Text, nil); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Message edited"})
	}
}

// MarkRead marks a chat read up to a message.
func (s *server) MarkRead() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg MarkReadBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		if msg.ChatID == 0 || msg.MessageID == 0 {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "chatId and messageId are required")
			return
		}
		if err := client.MarkRead(msg.ChatID, int64(msg.MessageID)); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Marked as read"})
	}
}

// GetChatHistory reads a chat's messages, texts included: this call exists
// for them.
func (s *server) GetChatHistory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg ChatHistoryBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		count := msg.Count
		if count <= 0 {
			count = 50
		}
		messages, err := client.GetChatHistoryWithin(msg.ChatID, msg.FromTime, 0, count, callTimeout)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		if messages == nil {
			messages = []maxclient.Message{}
		}
		s.respondOK(w, r, map[string]interface{}{"messages": messages})
	}
}

// React sets or removes the account's reaction on a message.
func (s *server) React() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg ReactBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		var err error
		if msg.Reaction == "" {
			_, err = client.RemoveReaction(msg.ChatID, msg.MessageID)
		} else {
			_, err = client.AddReaction(msg.ChatID, msg.MessageID, msg.Reaction)
		}
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Reaction updated"})
	}
}

// respondFetchFailure answers a failed download of a file.
func (s *server) respondFetchFailure(w http.ResponseWriter, r *http.Request, err error) {
	reason := "the file could not be fetched"
	var fetchErr *fetchError
	if errors.As(err, &fetchErr) {
		reason = fetchErr.reason
	}
	s.respondError(w, r, http.StatusUnprocessableEntity, CodeFetchFailed, reason)
}

// DownloadImage downloads a photo by its URL, MAX's baseUrl of an incoming
// photo, through the guarded fetch. It makes no MAX call.
func (s *server) DownloadImage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")
		var msg DownloadBody
		if !s.decodeBody(w, r, &msg) {
			return
		}
		if msg.URL == "" {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "url is required")
			return
		}
		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(userID)
			defer userSemaphoreManager.Release(userID)
		}
		data, _, err := fetchURLWithin(r.Context(), msg.URL, mediaMaxBytes(), fileDownloadTimeout)
		if err != nil {
			s.respondFetchFailure(w, r, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": http.DetectContentType(data),
		})
	}
}

// DownloadAudio downloads an audio file by its URL, as DownloadImage.
func (s *server) DownloadAudio() http.HandlerFunc {
	return s.DownloadImage()
}

// DownloadDocument downloads a file of a message: MAX gives its address
// (opcode 88), the gateway fetches it.
func (s *server) DownloadDocument() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg DownloadFileBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(userID)
			defer userSemaphoreManager.Release(userID)
		}
		info, err := client.GetFileDownloadURL(msg.ChatID, int64(msg.MessageID), msg.FileID)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		data, _, err := fetchURLWithin(r.Context(), info.URL, mediaMaxBytes(), fileDownloadTimeout)
		if err != nil {
			s.respondFetchFailure(w, r, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": http.DetectContentType(data),
		})
	}
}

// DownloadVideo downloads a video of a message: MAX gives its address
// (opcode 83), the gateway fetches it.
func (s *server) DownloadVideo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg DownloadFileBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(userID)
			defer userSemaphoreManager.Release(userID)
		}
		info, err := client.GetVideoDownloadURL(msg.ChatID, int64(msg.MessageID), msg.VideoID)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		data, _, err := fetchURLWithin(r.Context(), info.URL, mediaMaxBytes(), fileDownloadTimeout)
		if err != nil {
			s.respondFetchFailure(w, r, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": http.DetectContentType(data),
			"url":      info.URL,
		})
	}
}

// CheckUser tells for each number whether MAX has a user with it. It goes
// through the lookup cache of the sends and fills it: a number found or
// refused within the cache's time makes no MAX call, and the send that
// follows a check makes no second lookup. A stop signal on a lookup pauses
// the account and ends the call with 409 page_paused.
func (s *server) CheckUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg CheckUserBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		if len(msg.Phone) > maxCheckPhones {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "phone takes at most 50 numbers")
			return
		}
		results := make([]map[string]interface{}, 0, len(msg.Phone))
		for _, phone := range msg.Phone {
			result := map[string]interface{}{"phone": phone, "exists": false, "maxUserId": int64(0)}
			contactID, known := s.cachedLookup(userID, phone)
			if known {
				if contactID != 0 {
					result["exists"] = true
					result["maxUserId"] = contactID
					if u := client.GetCachedUser(contactID); u != nil {
						if name := maxclient.DisplayName(u.Names); name != "" {
							result["name"] = name
						}
					}
				}
				results = append(results, result)
				continue
			}
			user, err := client.SearchByPhoneWithTimeout(phone, sendTextLookupTimeout)
			switch {
			case err == nil:
				s.storeLookup(userID, phone, user.ID)
				result["exists"] = true
				result["maxUserId"] = user.ID
				if name := maxclient.DisplayName(user.Names); name != "" {
					result["name"] = name
				}
			case lookupNotFoundCode(err):
				s.storeLookup(userID, phone, 0)
			case errors.Is(err, maxclient.ErrUserNotFound):
				// MAX answered without a usable contact: no such user, not
				// cached.
			default:
				if pause := s.pauseOnLimit(userID, maxUserID, err); pause != nil {
					s.respondDelivery(w, r, http.StatusConflict, CodePagePaused, "MAX refused the lookup; the account is paused", err, pause)
					return
				}
				// The lookup itself failed: whether the user exists is not
				// known.
				result["error"] = err.Error()
				if code, ok := maxclient.ServerErrorCode(err); ok {
					result["upstreamCode"] = code
				}
			}
			results = append(results, result)
		}
		s.respondOK(w, r, map[string]interface{}{"users": results})
	}
}

// GetContacts returns the account's contact list.
func (s *server) GetContacts() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, client, maxUserID, ok := s.readCall(w, r, nil)
		if !ok {
			return
		}
		contacts, err := client.GetContacts()
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		if contacts == nil {
			contacts = []maxclient.Contact{}
		}
		s.respondOK(w, r, map[string]interface{}{"contacts": contacts, "count": len(contacts)})
	}
}

// GetUser returns MAX users by id (opcode 32): names, photo, and the phone
// when MAX shows it to the account. It replaces the contacts Sync carried
// before 2.0.0.
func (s *server) GetUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg UserInfoBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		if len(msg.UserIDs) == 0 || len(msg.UserIDs) > maxUserInfoIDs {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "userIds takes 1 to 100 ids")
			return
		}
		users, err := client.GetUsersWithin(msg.UserIDs, callTimeout)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		out := make([]map[string]interface{}, 0, len(users))
		for i := range users {
			u := &users[i]
			row := map[string]interface{}{
				"id":            u.ID,
				"accountStatus": u.AccountStatus,
				"names":         u.Names,
				"name":          maxclient.DisplayName(u.Names),
				"options":       u.Options,
				"baseUrl":       u.BaseURL,
				"baseRawUrl":    u.BaseRawURL,
				"photoId":       u.PhotoID,
				"description":   u.Description,
				"gender":        u.Gender,
				"link":          u.Link,
				"updateTime":    u.UpdateTime,
				"webApp":        u.WebApp,
				"avatarUrl":     maxclient.GetUserAvatarURL(u),
			}
			if u.Phone != 0 {
				row["phone"] = u.Phone
			}
			out = append(out, row)
		}
		s.respondOK(w, r, map[string]interface{}{"users": out, "count": len(out)})
	}
}

// SendPresence sends the typing indicator to a chat.
func (s *server) SendPresence() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg PresenceBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		if err := client.SendTyping(msg.ChatID); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Presence sent"})
	}
}

// chatAnswer answers a group call with the chat MAX gave back, as a chat row
// (C10: no message's text).
func (s *server) chatAnswer(w http.ResponseWriter, r *http.Request, client *maxclient.Client, chat *maxclient.Chat) {
	var row interface{}
	if chat != nil {
		rows, _ := projectChats(client, []maxclient.Chat{*chat}, chatIncludes{})
		if len(rows) == 1 {
			row = rows[0]
		}
	}
	s.respondOK(w, r, map[string]interface{}{"chat": row})
}

// CreateGroup creates a group chat.
func (s *server) CreateGroup() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg CreateGroupBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		chat, _, err := client.CreateGroup(msg.Name, msg.Participants, true)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.chatAnswer(w, r, client, chat)
	}
}

// GetGroupInviteLink returns a chat's invite link.
func (s *server) GetGroupInviteLink() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg GroupInfoBody
		userID, client, maxUserID, ok := s.readCall(w, r, &msg)
		if !ok {
			return
		}
		chats, err := client.GetChatInfoWithin([]int64{msg.ChatID}, callTimeout)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		if len(chats) == 0 {
			s.respondError(w, r, http.StatusNotFound, CodeAddressNotFound, "chat not found")
			return
		}
		s.respondOK(w, r, map[string]interface{}{"inviteLink": chats[0].Link})
	}
}

// GroupJoin joins a chat by its invite link.
func (s *server) GroupJoin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg GroupJoinBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		chat, err := client.JoinGroup(msg.Link)
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.chatAnswer(w, r, client, chat)
	}
}

// GroupLeave leaves a chat.
func (s *server) GroupLeave() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg GroupInfoBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		if err := client.LeaveChat(msg.ChatID); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Left group"})
	}
}

// UpdateGroupParticipants adds or removes members of a chat.
func (s *server) UpdateGroupParticipants() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg UpdateParticipantsBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		var err error
		switch msg.Operation {
		case "add":
			_, err = client.AddGroupMembers(msg.ChatID, msg.UserIDs, true)
		case "remove":
			_, err = client.RemoveGroupMembers(msg.ChatID, msg.UserIDs, 0)
		default:
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "operation is add or remove")
			return
		}
		if err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Participants updated"})
	}
}

// SetGroupName renames a chat.
func (s *server) SetGroupName() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg GroupNameBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		if _, err := client.UpdateChatProfile(msg.ChatID, msg.Name, ""); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Group name updated"})
	}
}

// SetGroupTopic sets a chat's description.
func (s *server) SetGroupTopic() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg GroupTopicBody
		userID, client, maxUserID, ok := s.providerCall(w, r, &msg)
		if !ok {
			return
		}
		if _, err := client.UpdateChatProfile(msg.ChatID, "", msg.Topic); err != nil {
			s.respondMAXFailure(w, r, userID, maxUserID, err)
			return
		}
		s.respondOK(w, r, map[string]interface{}{"message": "Group topic updated"})
	}
}
