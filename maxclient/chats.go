package maxclient

import (
	"encoding/json"
	"time"
)

// GetChatHistory gets message history for a chat
func (c *Client) GetChatHistory(chatID int64, fromTime int64, forward int, backward int) ([]Message, error) {
	if fromTime == 0 {
		fromTime = time.Now().UnixMilli()
	}
	
	if backward == 0 {
		backward = 200
	}
	
	payload := map[string]interface{}{
		"chatId":      chatID,
		"from":        fromTime,
		"forward":     forward,
		"backward":    backward,
		"getMessages": true,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Int("backward", backward).Msg("Fetching chat history")
	
	resp, err := c.sendAndWait(OpChatHistory, payload)
	if err != nil {
		return nil, err
	}
	
	var messages []Message
	
	if msgsRaw, ok := resp.Payload["messages"].([]interface{}); ok {
		for _, msgRaw := range msgsRaw {
			msgMap, ok := msgRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			msgBytes, _ := json.Marshal(msgMap)
			var msg Message
			if err := json.Unmarshal(msgBytes, &msg); err == nil {
				messages = append(messages, msg)
			}
		}
	}
	
	c.Logger.Info().Int("count", len(messages)).Msg("Fetched messages")
	return messages, nil
}

// GetChatInfo gets information about chats by IDs
func (c *Client) GetChatInfo(chatIDs []int64) ([]Chat, error) {
	payload := map[string]interface{}{
		"chatIds": chatIDs,
	}
	
	c.Logger.Info().Ints64("chatIds", chatIDs).Msg("Getting chat info")
	
	resp, err := c.sendAndWait(OpChatInfo, payload)
	if err != nil {
		return nil, err
	}
	
	var chats []Chat
	
	if chatsRaw, ok := resp.Payload["chats"].([]interface{}); ok {
		for _, chatRaw := range chatsRaw {
			chatMap, ok := chatRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			chatBytes, _ := json.Marshal(chatMap)
			var chat Chat
			if err := json.Unmarshal(chatBytes, &chat); err == nil {
				chats = append(chats, chat)
			}
		}
	}
	
	return chats, nil
}

// GetChat gets information about a single chat
func (c *Client) GetChat(chatID int64) (*Chat, error) {
	chats, err := c.GetChatInfo([]int64{chatID})
	if err != nil {
		return nil, err
	}
	
	if len(chats) == 0 {
		return nil, ErrChatNotFound
	}
	
	return &chats[0], nil
}

// CreateGroup creates a new group chat
func (c *Client) CreateGroup(name string, participantIDs []int64, notify bool) (*Chat, *Message, error) {
	if name == "" {
		return nil, nil, NewError("invalid_name", "Group name is required", "Validation Error")
	}
	
	// In MAX, groups are created by sending a special message with CONTROL attachment
	message := map[string]interface{}{
		"cid": time.Now().UnixMilli(),
		"attaches": []map[string]interface{}{
			{
				"_type":    string(AttachTypeControl),
				"event":    "new",
				"chatType": string(ChatTypeChat),
				"title":    name,
				"userIds":  participantIDs,
			},
		},
	}
	
	payload := map[string]interface{}{
		"notify":  notify,
		"message": message,
	}
	
	c.Logger.Info().Str("name", name).Ints64("participants", participantIDs).Msg("Creating group")
	
	resp, err := c.sendAndWait(OpMsgSend, payload)
	if err != nil {
		return nil, nil, err
	}
	
	var chat *Chat
	var msg *Message
	
	// Parse chat from response
	if chatRaw, ok := resp.Payload["chat"].(map[string]interface{}); ok {
		chatBytes, _ := json.Marshal(chatRaw)
		var c Chat
		if err := json.Unmarshal(chatBytes, &c); err == nil {
			chat = &c
		}
	}
	
	// Parse message from response
	msg, _ = c.parseMessageFromResponse(resp.Payload)
	
	return chat, msg, nil
}

// JoinGroup joins a group by invite link
func (c *Client) JoinGroup(link string) (*Chat, error) {
	// Process link to get the join path
	joinPath := link
	if idx := findSubstring(link, "join/"); idx != -1 {
		joinPath = link[idx:]
	}
	
	payload := map[string]interface{}{
		"link": joinPath,
	}
	
	c.Logger.Info().Str("link", link).Msg("Joining group")
	
	resp, err := c.sendAndWait(OpChatJoin, payload)
	if err != nil {
		return nil, err
	}
	
	if chatRaw, ok := resp.Payload["chat"].(map[string]interface{}); ok {
		chatBytes, _ := json.Marshal(chatRaw)
		var chat Chat
		if err := json.Unmarshal(chatBytes, &chat); err == nil {
			return &chat, nil
		}
	}
	
	return nil, ErrChatNotFound
}

// LeaveChat leaves a chat/group
func (c *Client) LeaveChat(chatID int64) error {
	payload := map[string]interface{}{
		"chatId": chatID,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Msg("Leaving chat")
	
	_, err := c.sendAndWait(OpChatLeave, payload)
	return err
}

// UpdateGroupMembers adds or removes members from a group
func (c *Client) UpdateGroupMembers(chatID int64, userIDs []int64, operation string, showHistory bool, cleanMsgPeriod int) (*Chat, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"userIds":   userIDs,
		"operation": operation, // "add" or "remove"
	}
	
	if operation == "add" {
		payload["showHistory"] = showHistory
	} else if operation == "remove" {
		payload["cleanMsgPeriod"] = cleanMsgPeriod
	}
	
	c.Logger.Info().Int64("chatId", chatID).Str("operation", operation).Ints64("userIds", userIDs).Msg("Updating group members")
	
	resp, err := c.sendAndWait(OpChatMembersUpdate, payload)
	if err != nil {
		return nil, err
	}
	
	if chatRaw, ok := resp.Payload["chat"].(map[string]interface{}); ok {
		chatBytes, _ := json.Marshal(chatRaw)
		var chat Chat
		if err := json.Unmarshal(chatBytes, &chat); err == nil {
			return &chat, nil
		}
	}
	
	return nil, nil
}

// AddGroupMembers adds members to a group
func (c *Client) AddGroupMembers(chatID int64, userIDs []int64, showHistory bool) (*Chat, error) {
	return c.UpdateGroupMembers(chatID, userIDs, "add", showHistory, 0)
}

// RemoveGroupMembers removes members from a group
func (c *Client) RemoveGroupMembers(chatID int64, userIDs []int64, cleanMsgPeriod int) (*Chat, error) {
	return c.UpdateGroupMembers(chatID, userIDs, "remove", false, cleanMsgPeriod)
}

// UpdateChatProfile updates chat name and/or description
func (c *Client) UpdateChatProfile(chatID int64, name string, description string) (*Chat, error) {
	payload := map[string]interface{}{
		"chatId": chatID,
	}
	
	if name != "" {
		payload["theme"] = name
	}
	if description != "" {
		payload["description"] = description
	}
	
	c.Logger.Info().Int64("chatId", chatID).Str("name", name).Msg("Updating chat profile")
	
	resp, err := c.sendAndWait(OpChatUpdate, payload)
	if err != nil {
		return nil, err
	}
	
	if chatRaw, ok := resp.Payload["chat"].(map[string]interface{}); ok {
		chatBytes, _ := json.Marshal(chatRaw)
		var chat Chat
		if err := json.Unmarshal(chatBytes, &chat); err == nil {
			return &chat, nil
		}
	}
	
	return nil, nil
}

// GetChatMembers gets members of a chat
func (c *Client) GetChatMembers(chatID int64, marker int64, count int) ([]Member, *int64, error) {
	if count == 0 {
		count = 50
	}
	
	payload := map[string]interface{}{
		"chatId": chatID,
		"type":   "MEMBER",
		"marker": marker,
		"count":  count,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Msg("Getting chat members")
	
	resp, err := c.sendAndWait(OpChatMembers, payload)
	if err != nil {
		return nil, nil, err
	}
	
	var members []Member
	
	if membersRaw, ok := resp.Payload["members"].([]interface{}); ok {
		for _, memberRaw := range membersRaw {
			memberMap, ok := memberRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			memberBytes, _ := json.Marshal(memberMap)
			var member Member
			if err := json.Unmarshal(memberBytes, &member); err == nil {
				members = append(members, member)
			}
		}
	}
	
	var nextMarker *int64
	if markerVal, ok := resp.Payload["marker"].(float64); ok {
		m := int64(markerVal)
		nextMarker = &m
	}
	
	return members, nextMarker, nil
}

// SearchChatMembers searches for members in a chat
func (c *Client) SearchChatMembers(chatID int64, query string) ([]Member, error) {
	payload := map[string]interface{}{
		"chatId": chatID,
		"type":   "MEMBER",
		"query":  query,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Str("query", query).Msg("Searching chat members")
	
	resp, err := c.sendAndWait(OpChatMembers, payload)
	if err != nil {
		return nil, err
	}
	
	var members []Member
	
	if membersRaw, ok := resp.Payload["members"].([]interface{}); ok {
		for _, memberRaw := range membersRaw {
			memberMap, ok := memberRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			memberBytes, _ := json.Marshal(memberMap)
			var member Member
			if err := json.Unmarshal(memberBytes, &member); err == nil {
				members = append(members, member)
			}
		}
	}
	
	return members, nil
}

// RevokeInviteLink revokes and regenerates the invite link for a chat
func (c *Client) RevokeInviteLink(chatID int64) (*Chat, error) {
	payload := map[string]interface{}{
		"chatId":            chatID,
		"revokePrivateLink": true,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Msg("Revoking invite link")
	
	resp, err := c.sendAndWait(OpChatUpdate, payload)
	if err != nil {
		return nil, err
	}
	
	if chatRaw, ok := resp.Payload["chat"].(map[string]interface{}); ok {
		chatBytes, _ := json.Marshal(chatRaw)
		var chat Chat
		if err := json.Unmarshal(chatBytes, &chat); err == nil {
			return &chat, nil
		}
	}
	
	return nil, nil
}

// DeleteChat deletes a chat
func (c *Client) DeleteChat(chatID int64) error {
	payload := map[string]interface{}{
		"chatId": chatID,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Msg("Deleting chat")
	
	_, err := c.sendAndWait(OpChatDelete, payload)
	return err
}

// ClearChatHistory clears chat history
func (c *Client) ClearChatHistory(chatID int64) error {
	payload := map[string]interface{}{
		"chatId": chatID,
	}
	
	c.Logger.Info().Int64("chatId", chatID).Msg("Clearing chat history")
	
	_, err := c.sendAndWait(OpChatClear, payload)
	return err
}

// helper function
func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

