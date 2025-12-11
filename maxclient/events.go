package maxclient

import (
	"encoding/json"
)

// EventType constants for webhook events
const (
	EventTypeMessage        = "Message"
	EventTypeMessageEdit    = "MessageEdit"
	EventTypeMessageDelete  = "MessageDelete"
	EventTypeReadReceipt    = "ReadReceipt"
	EventTypeConnected      = "Connected"
	EventTypeDisconnected   = "Disconnected"
	EventTypeAuthCodeSent   = "AuthCodeSent"
	EventTypeChatUpdate     = "ChatUpdate"
	EventTypeTyping         = "Typing"
	EventTypeReactionChange = "ReactionChange"
	EventTypeContactUpdate  = "ContactUpdate"
	EventTypePresenceUpdate = "PresenceUpdate"
	EventTypeFileReady      = "FileReady"
)

// MessageEvent represents a message event
type MessageEvent struct {
	ChatID  int64    `json:"chatId"`
	Message *Message `json:"message"`
}

// ReadReceiptEvent represents a read receipt event
type ReadReceiptEvent struct {
	ChatID    int64 `json:"chatId"`
	MessageID int64 `json:"messageId"`
	ReadMark  int64 `json:"readMark"`
}

// ChatUpdateEvent represents a chat update event
type ChatUpdateEvent struct {
	Chat *Chat `json:"chat"`
}

// TypingEvent represents a typing indicator event
type TypingEvent struct {
	ChatID int64 `json:"chatId"`
	UserID int64 `json:"userId"`
}

// ReactionChangeEvent represents a reaction change event
type ReactionChangeEvent struct {
	ChatID       int64             `json:"chatId"`
	MessageID    string            `json:"messageId"`
	TotalCount   int               `json:"totalCount"`
	YourReaction string            `json:"yourReaction,omitempty"`
	Counters     []ReactionCounter `json:"counters,omitempty"`
}

// ContactUpdateEvent represents a contact update event
type ContactUpdateEvent struct {
	Contact *Contact `json:"contact"`
}

// PresenceUpdateEvent represents a presence update event
type PresenceUpdateEvent struct {
	UserID   int64     `json:"userId"`
	Presence *Presence `json:"presence"`
}

// FileReadyEvent represents a file ready event
type FileReadyEvent struct {
	FileID  int64 `json:"fileId,omitempty"`
	VideoID int64 `json:"videoId,omitempty"`
}

// ParseMessageEvent parses a message event from payload
func ParseMessageEvent(payload map[string]interface{}) (*MessageEvent, error) {
	event := &MessageEvent{}

	if chatID, ok := payload["chatId"].(float64); ok {
		event.ChatID = int64(chatID)
	}

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

	if message.ChatID == 0 {
		message.ChatID = event.ChatID
	}

	event.Message = &message
	return event, nil
}

// ParseReadReceiptEvent parses a read receipt event from payload
func ParseReadReceiptEvent(payload map[string]interface{}) (*ReadReceiptEvent, error) {
	event := &ReadReceiptEvent{}

	if chatID, ok := payload["chatId"].(float64); ok {
		event.ChatID = int64(chatID)
	}
	if messageID, ok := payload["messageId"].(float64); ok {
		event.MessageID = int64(messageID)
	}
	if readMark, ok := payload["readMark"].(float64); ok {
		event.ReadMark = int64(readMark)
	}

	return event, nil
}

// ParseChatUpdateEvent parses a chat update event from payload
func ParseChatUpdateEvent(payload map[string]interface{}) (*ChatUpdateEvent, error) {
	event := &ChatUpdateEvent{}

	if chatData, ok := payload["chat"].(map[string]interface{}); ok {
		chatBytes, err := json.Marshal(chatData)
		if err != nil {
			return nil, err
		}

		var chat Chat
		if err := json.Unmarshal(chatBytes, &chat); err != nil {
			return nil, err
		}
		event.Chat = &chat
	}

	return event, nil
}

// ParseTypingEvent parses a typing event from payload
func ParseTypingEvent(payload map[string]interface{}) (*TypingEvent, error) {
	event := &TypingEvent{}

	if chatID, ok := payload["chatId"].(float64); ok {
		event.ChatID = int64(chatID)
	}
	if userID, ok := payload["userId"].(float64); ok {
		event.UserID = int64(userID)
	}

	return event, nil
}

// ParseReactionChangeEvent parses a reaction change event from payload
func ParseReactionChangeEvent(payload map[string]interface{}) (*ReactionChangeEvent, error) {
	event := &ReactionChangeEvent{}

	if chatID, ok := payload["chatId"].(float64); ok {
		event.ChatID = int64(chatID)
	}
	if messageID, ok := payload["messageId"].(string); ok {
		event.MessageID = messageID
	}
	if totalCount, ok := payload["totalCount"].(float64); ok {
		event.TotalCount = int(totalCount)
	}
	if yourReaction, ok := payload["yourReaction"].(string); ok {
		event.YourReaction = yourReaction
	}

	if countersRaw, ok := payload["counters"].([]interface{}); ok {
		for _, counterRaw := range countersRaw {
			if counterMap, ok := counterRaw.(map[string]interface{}); ok {
				counter := ReactionCounter{}
				if reaction, ok := counterMap["reaction"].(string); ok {
					counter.Reaction = reaction
				}
				if count, ok := counterMap["count"].(float64); ok {
					counter.Count = int(count)
				}
				event.Counters = append(event.Counters, counter)
			}
		}
	}

	return event, nil
}

// ParseContactUpdateEvent parses a contact update event from payload
func ParseContactUpdateEvent(payload map[string]interface{}) (*ContactUpdateEvent, error) {
	event := &ContactUpdateEvent{}

	if contactData, ok := payload["contact"].(map[string]interface{}); ok {
		contactBytes, err := json.Marshal(contactData)
		if err != nil {
			return nil, err
		}

		var contact Contact
		if err := json.Unmarshal(contactBytes, &contact); err != nil {
			return nil, err
		}
		event.Contact = &contact
	}

	return event, nil
}

// ParsePresenceUpdateEvent parses a presence update event from payload
func ParsePresenceUpdateEvent(payload map[string]interface{}) (*PresenceUpdateEvent, error) {
	event := &PresenceUpdateEvent{}

	if userID, ok := payload["userId"].(float64); ok {
		event.UserID = int64(userID)
	}

	if presenceData, ok := payload["presence"].(map[string]interface{}); ok {
		presence := &Presence{}
		if seen, ok := presenceData["seen"].(float64); ok {
			presence.Seen = int64(seen)
		}
		event.Presence = presence
	}

	return event, nil
}

// ParseFileReadyEvent parses a file ready event from payload
func ParseFileReadyEvent(payload map[string]interface{}) (*FileReadyEvent, error) {
	event := &FileReadyEvent{}

	if fileID, ok := payload["fileId"].(float64); ok {
		event.FileID = int64(fileID)
	}
	if videoID, ok := payload["videoId"].(float64); ok {
		event.VideoID = int64(videoID)
	}

	return event, nil
}

// EventToWebhookPayload converts an event to a webhook-compatible payload
func EventToWebhookPayload(event Event) map[string]interface{} {
	return map[string]interface{}{
		"type":   event.Type,
		"opcode": int(event.Opcode),
		"event":  event.Payload,
	}
}
