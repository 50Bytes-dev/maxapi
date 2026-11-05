package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"maxapi/maxclient"

	"github.com/rs/zerolog/log"
)

// Sending (C0, C4, C8). Every call that sends a message, POST
// /chat/send/text and the media sends, runs the same checks in this order and
// answers with the delivery codes of codes.go:
//
//	session, body, target, message, pause, phone lookup, send.
//
// Nothing before the send reaches MAX except the phone lookup, and a refusal
// before it is final for this request: nothing was sent.

const (
	// sendTextLookupTimeout and sendTextReplyTimeout bound the waits for MAX
	// in a send: the lookup of a phone, then the send's acknowledgement.
	// With the database calls a send answers within ~9.5 s, before a caller
	// with a 10 s read timeout gives up and loses the outcome code.
	sendTextLookupTimeout = 4 * time.Second
	sendTextReplyTimeout  = 5 * time.Second

	// maxMessageLength is the limit on a message's text: MAX's
	// max-msg-length, in UTF-16 code units, as the web client cuts a draft.
	// Live on 2026-11-05 MAX took 4001 units, and dropped the connection on
	// a text of 10000 characters (docs/research/max-live-checks-2026-11.md,
	// L9).
	maxMessageLength = 4000
	// maxAttachments is the most attachments of one message: live on
	// 2026-11-05 MAX took 12 photos and refused 13 with
	// errors.send-message.attachment.max-size-reached (L4).
	maxAttachments = 12
	// maxRefBytes bounds ref (C8).
	maxRefBytes = 128
)

// utf16Len is the length of s in UTF-16 code units, the way MAX's web client
// counts a text against max-msg-length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// refCID turns the caller's ref into the message's cid (C8): one ref always
// gives one cid, so MessageOutcome finds the message by it, and a repeat of a
// send with the same ref carries the same cid. It lies among the unix
// millisecond values of the web client's own cids (2001 to 2036); a cid of a
// send without a ref is the current time.
func refCID(ref string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(ref))
	return refCIDBase + int64(h.Sum64()%refCIDSpan)
}

const (
	refCIDBase = 1_000_000_000_000
	refCIDSpan = 1 << 40
)

// respondDelivery writes a delivery failure: code, the fixed text msg, MAX's
// error code as upstreamCode when MAX answered with one, the pause when the
// account is paused.
func (s *server) respondDelivery(w http.ResponseWriter, r *http.Request, status int, code, msg string, err error, pause *accountPause) {
	body := map[string]interface{}{
		"success": false,
		"error":   msg,
		"code":    code,
	}
	if upstream, ok := maxclient.ServerErrorCode(err); ok {
		body["upstreamCode"] = upstream
	}
	if pause != nil {
		body["paused"] = true
		body["pauseReason"] = pause.reason
	}
	s.Respond(w, r, status, body)
}

// liveClient returns the user's MAX client when its session is up; otherwise
// it answers 503 not_connected.
func (s *server) liveClient(w http.ResponseWriter, r *http.Request, userID string) (*maxclient.Client, bool) {
	client := clientManager.GetMaxClient(userID)
	if client == nil || !client.IsConnected() {
		s.respondError(w, r, http.StatusServiceUnavailable, CodeNotConnected, "not connected")
		return nil, false
	}
	return client, true
}

// refuseSendWhilePaused answers 422 send_rejected with the pause when the
// user's MAX account is paused, and reports it. It returns the MAX account.
func (s *server) refuseSendWhilePaused(w http.ResponseWriter, r *http.Request, userID string) (int64, bool) {
	maxUserID := s.maxUserIDOf(userID)
	if pause := s.pauseOf(maxUserID); pause != nil {
		s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeSendRejected,
			"the account is paused until the owner resumes it", nil, pause)
		return maxUserID, true
	}
	return maxUserID, false
}

// sendTarget is where a send goes: chatID, and when it was found by phone,
// the phone and whether the lookup cache gave it.
type sendTarget struct {
	chatID    int64
	phone     string
	fromCache bool
}

// resolveTarget turns chatID or phone into the chat to send to. A phone is
// looked up in the cache, then in MAX (opcode 46). On
// failure it answers address_not_found or lookup_failed, with the pause when
// MAX's answer paused the account.
func (s *server) resolveTarget(w http.ResponseWriter, r *http.Request, userID string, client *maxclient.Client, maxUserID int64, chatID int64, phone string) (sendTarget, bool) {
	if chatID != 0 {
		return sendTarget{chatID: chatID}, true
	}
	contactID, known := s.cachedLookup(userID, phone)
	if known && contactID == 0 {
		s.respondDelivery(w, r, http.StatusNotFound, CodeAddressNotFound,
			"user not found: MAX had no user with this phone at the last lookup", nil, nil)
		return sendTarget{}, false
	}
	if !known {
		user, err := client.SearchByPhoneWithTimeout(phone, sendTextLookupTimeout)
		if err != nil {
			switch {
			case errors.Is(err, maxclient.ErrInvalidPhone), errors.Is(err, maxclient.ErrUserNotFound):
				// Not cached: only MAX's explicit not.found is trusted for
				// later sends, not an answer without a usable contact.
				s.respondDelivery(w, r, http.StatusNotFound, CodeAddressNotFound, "user not found", err, nil)
			case lookupNotFoundCode(err):
				s.storeLookup(userID, phone, 0)
				s.respondDelivery(w, r, http.StatusNotFound, CodeAddressNotFound, "user not found", err, nil)
			default:
				s.respondDelivery(w, r, http.StatusServiceUnavailable, CodeLookupFailed, "user lookup failed", err,
					s.pauseOnLimit(userID, maxUserID, err))
			}
			return sendTarget{}, false
		}
		contactID = user.ID
		s.storeLookup(userID, phone, contactID)
	}
	return sendTarget{chatID: maxclient.GetDialogID(client.MaxUserID, contactID), phone: phone, fromCache: known}, true
}

// notifyOf is the notify flag of a send: true unless the caller set false.
// MAX's web client sends every message with notify true, and MAX refuses a
// channel post without it (errors.send-message.channel-notify, seen live on
// 2026-11-05).
func notifyOf(p *bool) bool {
	return p == nil || *p
}

// deliverArgs is one message to send.
type deliverArgs struct {
	userID    string
	token     string
	client    *maxclient.Client
	maxUserID int64
	target    sendTarget
	text      string
	replyTo   int64
	notify    bool
	attaches  []maxclient.Attachment
	ref       string
}

// deliver sends the message and answers: 200 with messageId and chatId, or a
// delivery failure by MAX's answer. Only an error MAX answered with proves the
// message was not created; no answer in time, or the session lost once the
// frame was out, leaves the outcome unknown (send_failed). With a ref, such a
// send is confirmed later by MessageOutcome (outcome.go).
func (s *server) deliver(w http.ResponseWriter, r *http.Request, a deliverArgs) {
	cid := int64(0)
	if a.ref != "" {
		cid = refCID(a.ref)
	}
	sentAt := time.Now()
	result, err := a.client.SendMessage(maxclient.SendMessageOptions{
		ChatID:      a.target.chatID,
		Text:        a.text,
		ReplyTo:     a.replyTo,
		Notify:      a.notify,
		Attachments: a.attaches,
		CID:         cid,
		Timeout:     sendTextReplyTimeout,
	})
	if errors.Is(err, maxclient.ErrFrameTooLarge) {
		s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeMessageRejected,
			"the message is too long to send", err, nil)
		return
	}
	if err != nil {
		code, rejected := maxclient.ServerErrorCode(err)
		if !rejected {
			if a.ref != "" {
				s.scheduleOutcome(outcomeCheck{
					userID: a.userID, token: a.token, maxUserID: a.maxUserID,
					chatID: a.target.chatID, cid: cid, ref: a.ref, sentAt: sentAt,
				})
			}
			s.respondDelivery(w, r, http.StatusInternalServerError, CodeSendFailed,
				"MAX did not confirm the message; it may have arrived", err, nil)
			return
		}
		if pause := s.pauseOnLimit(a.userID, a.maxUserID, err); pause != nil {
			s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeSendRejected, "MAX refused the message", err, pause)
			return
		}
		if a.target.fromCache {
			// The cached id may be stale (the number changed hands, the
			// contact's account is gone): the next send looks it up again.
			s.dropLookup(a.userID, a.target.phone)
		}
		status, outcome := sendRefusal(code, len(a.attaches) > 0)
		s.respondDelivery(w, r, status, outcome, "MAX refused the message", err, nil)
		return
	}

	s.Respond(w, r, http.StatusOK, map[string]interface{}{
		"success":   true,
		"messageId": result.ID,
		"chatId":    a.target.chatID,
	})
}

// checkMessage answers message_rejected for a message MAX would refuse as it
// is, without a MAX call: no text and no attachment, too many attachments, a
// text over max-msg-length.
func (s *server) checkMessage(w http.ResponseWriter, r *http.Request, text string, attachments int) bool {
	switch {
	case attachments > maxAttachments:
		s.respondError(w, r, http.StatusUnprocessableEntity, CodeMessageRejected,
			fmt.Sprintf("a message takes at most %d attachments", maxAttachments))
	case text == "" && attachments == 0:
		s.respondError(w, r, http.StatusUnprocessableEntity, CodeMessageRejected, "the message has neither text nor attachments")
	case utf16Len(text) > maxMessageLength:
		s.respondError(w, r, http.StatusUnprocessableEntity, CodeMessageRejected,
			fmt.Sprintf("the text is over %d characters", maxMessageLength))
	default:
		return true
	}
	return false
}

// checkRef answers 400 INVALID_INPUT for a ref over maxRefBytes.
func (s *server) checkRef(w http.ResponseWriter, r *http.Request, ref string) bool {
	if len(ref) > maxRefBytes {
		s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, fmt.Sprintf("ref is over %d bytes", maxRefBytes))
		return false
	}
	return true
}

// SendMessage sends a text, with photos uploaded by POST /media/photo, to a
// chat or to a person by phone (C4).
func (s *server) SendMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		client, ok := s.liveClient(w, r, userID)
		if !ok {
			return
		}
		var msg MessageBody
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		if !s.checkRef(w, r, msg.Ref) {
			return
		}
		attaches := make([]maxclient.Attachment, 0, len(msg.Attachments))
		for _, photo := range msg.Attachments {
			if strings.TrimSpace(photo) == "" {
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "an attachment is empty")
				return
			}
			attaches = append(attaches, maxclient.Attachment{Type: maxclient.AttachTypePhoto, PhotoToken: photo})
		}
		// chatId 0 is the account's own "Favorites" chat, so a request
		// without a recipient must not fall through to it.
		if msg.ChatID == 0 && msg.Phone == "" {
			s.respondError(w, r, http.StatusBadRequest, CodeInvalidTarget, "chatId or phone is required")
			return
		}
		if !s.checkMessage(w, r, msg.Text, len(attaches)) {
			return
		}
		maxUserID, paused := s.refuseSendWhilePaused(w, r, userID)
		if paused {
			return
		}
		target, ok := s.resolveTarget(w, r, userID, client, maxUserID, msg.ChatID, msg.Phone)
		if !ok {
			return
		}
		s.deliver(w, r, deliverArgs{
			userID: userID, token: token, client: client, maxUserID: maxUserID, target: target,
			text: msg.Text, replyTo: int64(msg.ReplyTo), notify: notifyOf(msg.Notify), attaches: attaches, ref: msg.Ref,
		})
	}
}

// mediaSend is one file to send by POST /chat/send/document, /audio or
// /video.
type mediaSend struct {
	ChatID   int64
	Phone    string
	Data     string
	FileName string
	Caption  string
	Notify   bool
	Ref      string
	video    bool
}

// sendMedia sends a file: the same checks and codes as a text, with the file
// fetched or decoded and uploaded to MAX after the pause check and before the
// phone lookup. MAX refusing the upload, or the upload failing, answers
// attachment_rejected: nothing was sent, and the file may be sent again.
func (s *server) sendMedia(w http.ResponseWriter, r *http.Request, m mediaSend) {
	userID := r.Context().Value("userinfo").(Values).Get("Id")
	token := r.Context().Value("userinfo").(Values).Get("Token")

	client, ok := s.liveClient(w, r, userID)
	if !ok {
		return
	}
	if !s.checkRef(w, r, m.Ref) {
		return
	}
	if m.ChatID == 0 && m.Phone == "" {
		s.respondError(w, r, http.StatusBadRequest, CodeInvalidTarget, "chatId or phone is required")
		return
	}
	if !s.checkMessage(w, r, m.Caption, 1) {
		return
	}
	maxUserID, paused := s.refuseSendWhilePaused(w, r, userID)
	if paused {
		return
	}

	data, err := decodeMedia(r.Context(), m.Data, mediaMaxBytes())
	if err != nil {
		var fetchErr *fetchError
		if errors.As(err, &fetchErr) {
			s.respondError(w, r, http.StatusUnprocessableEntity, CodeFetchFailed, fetchErr.reason)
		} else {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, err.Error())
		}
		return
	}

	if userSemaphoreManager != nil {
		userSemaphoreManager.Acquire(userID)
		defer userSemaphoreManager.Release(userID)
	}
	var attach *maxclient.Attachment
	if m.video {
		attach, err = client.UploadVideo(data, m.FileName)
	} else {
		attach, err = client.UploadFile(data, m.FileName)
	}
	if err != nil {
		if pause := s.pauseOnLimit(userID, maxUserID, err); pause != nil {
			s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeSendRejected, "MAX refused the upload", err, pause)
			return
		}
		if errors.Is(err, maxclient.ErrNotConnected) {
			s.respondDelivery(w, r, http.StatusServiceUnavailable, CodeNotConnected, "the session was lost during the upload", err, nil)
			return
		}
		s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeAttachmentRejected, "MAX did not take the file", err, nil)
		return
	}

	target, ok := s.resolveTarget(w, r, userID, client, maxUserID, m.ChatID, m.Phone)
	if !ok {
		return
	}
	s.deliver(w, r, deliverArgs{
		userID: userID, token: token, client: client, maxUserID: maxUserID, target: target,
		text: m.Caption, notify: m.Notify, attaches: []maxclient.Attachment{*attach}, ref: m.Ref,
	})
}

// SendDocument sends a document.
func (s *server) SendDocument() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg DocumentBody
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		name := msg.FileName
		if name == "" {
			name = "document"
		}
		s.sendMedia(w, r, mediaSend{ChatID: msg.ChatID, Phone: msg.Phone, Data: msg.Document, FileName: name,
			Caption: msg.Caption, Notify: notifyOf(msg.Notify), Ref: msg.Ref})
	}
}

// SendAudio sends an audio file, as a file: MAX takes audio that way.
func (s *server) SendAudio() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg AudioBody
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		name := msg.FileName
		if name == "" {
			name = "audio.mp3"
		}
		s.sendMedia(w, r, mediaSend{ChatID: msg.ChatID, Phone: msg.Phone, Data: msg.Audio, FileName: name,
			Notify: notifyOf(msg.Notify), Ref: msg.Ref})
	}
}

// SendVideo sends a video.
func (s *server) SendVideo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg VideoBody
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		name := msg.FileName
		if name == "" {
			name = "video.mp4"
		}
		s.sendMedia(w, r, mediaSend{ChatID: msg.ChatID, Phone: msg.Phone, Data: msg.Video, FileName: name,
			Caption: msg.Caption, Notify: notifyOf(msg.Notify), Ref: msg.Ref, video: true})
	}
}

// Deleting messages (C5).

const (
	// maxDeleteIDs is the most message ids of one delete.
	maxDeleteIDs = 100
	// deleteReplyTimeout bounds the wait for MAX's answer to a delete.
	deleteReplyTimeout = 5 * time.Second
)

// Statuses of one message in the answer of POST /chat/delete.
const (
	DeleteStatusDeleted   = "deleted"
	DeleteStatusTooOld    = "too_old"
	DeleteStatusNotFound  = "not_found"
	DeleteStatusForbidden = "forbidden"
)

// deleteRefusal tells the status every message of a delete gets when MAX
// refused the whole call with code, and whether the code is one of a
// message at all; otherwise the call answers send_rejected. The markers are
// read from codes as MAX spells them, to be confirmed live.
func deleteRefusal(code string) (string, bool) {
	lower := strings.ToLower(code)
	switch {
	case notFoundCodes[lower], strings.Contains(lower, "not.found"):
		return DeleteStatusNotFound, true
	case containsAny(lower, []string{"time", "old", "expired", "late"}):
		return DeleteStatusTooOld, true
	case containsAny(lower, []string{"denied", "forbidden", "access", "permission", "rights"}):
		return DeleteStatusForbidden, true
	}
	return "", false
}

// DeleteMessage deletes messages for everyone (C5): POST /chat/delete
// {chatId, messageIds}. One MAX call; MAX answers with the ids it deleted.
func (s *server) DeleteMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")

		client, ok := s.liveClient(w, r, userID)
		if !ok {
			return
		}
		var msg DeleteMessageBody
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		if msg.ChatID == 0 {
			s.respondError(w, r, http.StatusBadRequest, CodeInvalidTarget, "chatId is required")
			return
		}
		if len(msg.MessageIDs) == 0 || len(msg.MessageIDs) > maxDeleteIDs {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput,
				fmt.Sprintf("messageIds takes 1 to %d ids", maxDeleteIDs))
			return
		}
		ids := make([]int64, 0, len(msg.MessageIDs))
		seen := map[int64]bool{}
		for _, id := range msg.MessageIDs {
			if id <= 0 {
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "a message id is not a positive number")
				return
			}
			if !seen[int64(id)] {
				seen[int64(id)] = true
				ids = append(ids, int64(id))
			}
		}
		maxUserID, paused := s.refuseSendWhilePaused(w, r, userID)
		if paused {
			return
		}

		deleted, known, err := client.DeleteMessages(msg.ChatID, ids, false, deleteReplyTimeout)
		if err != nil {
			code, rejected := maxclient.ServerErrorCode(err)
			if !rejected {
				s.respondDelivery(w, r, http.StatusInternalServerError, CodeSendFailed,
					"MAX did not confirm the delete; the messages may be deleted", err, nil)
				return
			}
			if pause := s.pauseOnLimit(userID, maxUserID, err); pause != nil {
				s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeSendRejected, "MAX refused the delete", err, pause)
				return
			}
			status, perMessage := deleteRefusal(code)
			if !perMessage {
				s.respondDelivery(w, r, http.StatusUnprocessableEntity, CodeSendRejected, "MAX refused the delete", err, nil)
				return
			}
			results := make([]map[string]interface{}, 0, len(ids))
			for _, id := range ids {
				results = append(results, map[string]interface{}{
					"messageId": strconv.FormatInt(id, 10), "status": status, "upstreamCode": code,
				})
			}
			s.Respond(w, r, http.StatusOK, map[string]interface{}{"success": true, "chatId": msg.ChatID, "results": results})
			return
		}

		gone := map[int64]bool{}
		for _, id := range deleted {
			gone[id] = true
		}
		results := make([]map[string]interface{}, 0, len(ids))
		for _, id := range ids {
			status := DeleteStatusDeleted
			if known && !gone[id] {
				// MAX answered without error but left the message: no such
				// message in the chat, as far as it says.
				status = DeleteStatusNotFound
			}
			results = append(results, map[string]interface{}{"messageId": strconv.FormatInt(id, 10), "status": status})
		}
		log.Info().Str("userID", userID).Int64("chatId", msg.ChatID).Int("asked", len(ids)).Int("deleted", len(deleted)).
			Msg("Messages deleted for everyone")
		s.Respond(w, r, http.StatusOK, map[string]interface{}{"success": true, "chatId": msg.ChatID, "results": results})
	}
}
