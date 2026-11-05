package maxclient

import (
	"encoding/json"
	"sync/atomic"
	"time"
)

// SendMessageOptions contains options for sending a message
type SendMessageOptions struct {
	ChatID      int64
	Text        string
	Notify      bool
	ReplyTo     int64
	Attachments []Attachment
	Elements    []Element

	// CID is the client id of the message, which MAX echoes in it: the key
	// to find the message again. Zero takes the next of NextCID.
	CID int64

	// Timeout bounds the wait for MAX to acknowledge the message; zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// NextCID returns a new client message id the way the web client makes one:
// the current unix time in ms, and always above the previous one.
func (c *Client) NextCID() int64 {
	for {
		prev := atomic.LoadInt64(&c.lastCID)
		next := time.Now().UnixMilli()
		if next <= prev {
			next = prev + 1
		}
		if atomic.CompareAndSwapInt64(&c.lastCID, prev, next) {
			return next
		}
	}
}

// SendMessage sends a text message to a chat
// Note: ChatID=0 is valid for "Favorites/Saved Messages" chat
func (c *Client) SendMessage(opts SendMessageOptions) (*Message, error) {
	cid := opts.CID
	if cid == 0 {
		cid = c.NextCID()
	}
	message := map[string]interface{}{
		"text": opts.Text,
		"cid":  cid,
	}

	if len(opts.Elements) > 0 {
		message["elements"] = opts.Elements
	}

	if len(opts.Attachments) > 0 {
		message["attaches"] = opts.Attachments
	}

	if opts.ReplyTo > 0 {
		message["link"] = map[string]interface{}{
			"type":      "REPLY",
			"messageId": opts.ReplyTo,
		}
	}

	payload := map[string]interface{}{
		"chatId":  opts.ChatID,
		"message": message,
		"notify":  opts.Notify,
	}

	c.Logger.Info().Int64("chatId", opts.ChatID).Msg("Sending message")

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	resp, err := c.sendAndWaitWithTimeout(OpMsgSend, payload, timeout)
	if err != nil {
		return nil, err
	}

	return c.parseMessageFromResponse(resp.Payload)
}

// SendTextMessage is a convenience method for sending text messages
func (c *Client) SendTextMessage(chatID int64, text string, notify bool) (*Message, error) {
	return c.SendMessage(SendMessageOptions{
		ChatID: chatID,
		Text:   text,
		Notify: notify,
	})
}

// SendReply sends a reply to a message
func (c *Client) SendReply(chatID int64, text string, replyToID int64, notify bool) (*Message, error) {
	return c.SendMessage(SendMessageOptions{
		ChatID:  chatID,
		Text:    text,
		ReplyTo: replyToID,
		Notify:  notify,
	})
}

// EditMessage edits an existing message
func (c *Client) EditMessage(chatID int64, messageID int64, text string, attachments []Attachment) (*Message, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
		"text":      text,
	}

	if len(attachments) > 0 {
		payload["attaches"] = attachments
	}

	c.Logger.Info().Int64("chatId", chatID).Int64("messageId", messageID).Msg("Editing message")

	resp, err := c.sendAndWait(OpMsgEdit, payload)
	if err != nil {
		return nil, err
	}

	return c.parseMessageFromResponse(resp.Payload)
}

// DeleteMessages deletes messages (MSG_DELETE, opcode 66), for everyone when
// forMe is false. MAX answers with the ids it deleted (messageIds), as the
// web client reads it; known is false when the answer has no such list.
func (c *Client) DeleteMessages(chatID int64, messageIDs []int64, forMe bool, timeout time.Duration) (deleted []int64, known bool, err error) {
	payload := map[string]interface{}{
		"chatId":     chatID,
		"messageIds": messageIDs,
		"forMe":      forMe,
	}
	c.Logger.Info().Int64("chatId", chatID).Int("count", len(messageIDs)).Bool("forMe", forMe).Msg("Deleting messages")

	resp, err := c.sendAndWaitWithTimeout(OpMsgDelete, payload, timeout)
	if err != nil {
		return nil, false, err
	}
	var body struct {
		MessageIDs *[]flexInt64 `json:"messageIds"`
	}
	if err := resp.decodePayload(&body); err != nil || body.MessageIDs == nil {
		return nil, false, nil
	}
	for _, id := range *body.MessageIDs {
		deleted = append(deleted, int64(id))
	}
	return deleted, true, nil
}

// MarkRead marks messages as read in a chat
func (c *Client) MarkRead(chatID int64, messageID int64) error {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
	}

	c.Logger.Debug().Int64("chatId", chatID).Int64("messageId", messageID).Msg("Marking as read")

	_, err := c.sendAndWait(OpChatMark, payload)
	return err
}

// SendTyping sends typing indicator
func (c *Client) SendTyping(chatID int64) error {
	payload := map[string]interface{}{
		"chatId": chatID,
	}

	_, err := c.sendAndWait(OpMsgTyping, payload)
	return err
}

// AddReaction adds a reaction to a message
func (c *Client) AddReaction(chatID int64, messageID string, reaction string) (*ReactionInfo, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
		"reaction": map[string]interface{}{
			"reactionType": "EMOJI",
			"id":           reaction,
		},
	}

	c.Logger.Info().Int64("chatId", chatID).Str("messageId", messageID).Str("reaction", reaction).Msg("Adding reaction")

	resp, err := c.sendAndWait(OpMsgReaction, payload)
	if err != nil {
		return nil, err
	}

	if reactionInfo, ok := resp.Payload["reactionInfo"].(map[string]interface{}); ok {
		infoBytes, _ := json.Marshal(reactionInfo)
		var info ReactionInfo
		if err := json.Unmarshal(infoBytes, &info); err == nil {
			return &info, nil
		}
	}

	return nil, nil
}

// RemoveReaction removes a reaction from a message
func (c *Client) RemoveReaction(chatID int64, messageID string) (*ReactionInfo, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
	}

	c.Logger.Info().Int64("chatId", chatID).Str("messageId", messageID).Msg("Removing reaction")

	resp, err := c.sendAndWait(OpMsgCancelReaction, payload)
	if err != nil {
		return nil, err
	}

	if reactionInfo, ok := resp.Payload["reactionInfo"].(map[string]interface{}); ok {
		infoBytes, _ := json.Marshal(reactionInfo)
		var info ReactionInfo
		if err := json.Unmarshal(infoBytes, &info); err == nil {
			return &info, nil
		}
	}

	return nil, nil
}

// GetReactions gets reactions for messages
func (c *Client) GetReactions(chatID int64, messageIDs []string) (map[string]*ReactionInfo, error) {
	payload := map[string]interface{}{
		"chatId":     chatID,
		"messageIds": messageIDs,
	}

	resp, err := c.sendAndWait(OpMsgGetReactions, payload)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*ReactionInfo)

	if messagesReactions, ok := resp.Payload["messagesReactions"].(map[string]interface{}); ok {
		for msgID, reactionData := range messagesReactions {
			if reactionMap, ok := reactionData.(map[string]interface{}); ok {
				infoBytes, _ := json.Marshal(reactionMap)
				var info ReactionInfo
				if err := json.Unmarshal(infoBytes, &info); err == nil {
					result[msgID] = &info
				}
			}
		}
	}

	return result, nil
}

// PinMessage pins a message in a chat
func (c *Client) PinMessage(chatID int64, messageID int64, notifyPin bool) error {
	payload := map[string]interface{}{
		"chatId":       chatID,
		"pinMessageId": messageID,
		"notifyPin":    notifyPin,
	}

	c.Logger.Info().Int64("chatId", chatID).Int64("messageId", messageID).Msg("Pinning message")

	_, err := c.sendAndWait(OpChatUpdate, payload)
	return err
}

// parseMessageFromResponse parses a message from response payload
func (c *Client) parseMessageFromResponse(payload map[string]interface{}) (*Message, error) {
	// The message might be in "message" field or directly in payload
	msgData := payload
	if msg, ok := payload["message"].(map[string]interface{}); ok {
		msgData = msg
	}

	msgBytes, err := json.Marshal(msgData)
	if err != nil {
		return nil, err
	}

	var message Message
	if err := json.Unmarshal(msgBytes, &message); err != nil {
		return nil, err
	}

	// Get chatId from parent payload if not in message
	if message.ChatID == 0 {
		if chatID, ok := payload["chatId"].(float64); ok {
			message.ChatID = int64(chatID)
		}
	}

	return &message, nil
}

// GetMessage gets a single message by ID
func (c *Client) GetMessage(chatID int64, messageID int64) (*Message, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
	}

	resp, err := c.sendAndWait(OpMsgGet, payload)
	if err != nil {
		return nil, err
	}

	return c.parseMessageFromResponse(resp.Payload)
}
