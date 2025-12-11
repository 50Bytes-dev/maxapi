package maxclient

import (
	"encoding/json"
)

// GetUsers gets information about users by IDs
func (c *Client) GetUsers(userIDs []int64) ([]User, error) {
	// First check cache
	var cachedUsers []User
	var missingIDs []int64
	
	c.usersMu.RLock()
	for _, id := range userIDs {
		if user, ok := c.users[id]; ok {
			cachedUsers = append(cachedUsers, *user)
		} else {
			missingIDs = append(missingIDs, id)
		}
	}
	c.usersMu.RUnlock()
	
	if len(missingIDs) == 0 {
		return cachedUsers, nil
	}
	
	// Fetch missing users
	payload := map[string]interface{}{
		"contactIds": missingIDs,
	}
	
	c.Logger.Info().Ints64("userIds", missingIDs).Msg("Fetching users")
	
	resp, err := c.sendAndWait(OpContactInfo, payload)
	if err != nil {
		return cachedUsers, err
	}
	
	var fetchedUsers []User
	
	if contactsRaw, ok := resp.Payload["contacts"].([]interface{}); ok {
		for _, contactRaw := range contactsRaw {
			contactMap, ok := contactRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			contactBytes, _ := json.Marshal(contactMap)
			var user User
			if err := json.Unmarshal(contactBytes, &user); err == nil {
				fetchedUsers = append(fetchedUsers, user)
				c.cacheUser(&user)
			}
		}
	}
	
	// Combine cached and fetched users in original order
	result := make([]User, 0, len(userIDs))
	for _, id := range userIDs {
		for _, user := range cachedUsers {
			if user.ID == id {
				result = append(result, user)
				break
			}
		}
		for _, user := range fetchedUsers {
			if user.ID == id {
				result = append(result, user)
				break
			}
		}
	}
	
	return result, nil
}

// GetUser gets information about a single user
func (c *Client) GetUser(userID int64) (*User, error) {
	// Check cache first
	if user := c.GetCachedUser(userID); user != nil {
		return user, nil
	}
	
	users, err := c.GetUsers([]int64{userID})
	if err != nil {
		return nil, err
	}
	
	if len(users) == 0 {
		return nil, ErrUserNotFound
	}
	
	return &users[0], nil
}

// SearchByPhone searches for a user by phone number
func (c *Client) SearchByPhone(phone string) (*User, error) {
	if !ValidatePhone(phone) {
		return nil, ErrInvalidPhone
	}
	
	payload := map[string]interface{}{
		"phone": phone,
	}
	
	c.Logger.Info().Str("phone", phone).Msg("Searching user by phone")
	
	resp, err := c.sendAndWait(OpContactInfoByPhone, payload)
	if err != nil {
		return nil, err
	}
	
	if contactRaw, ok := resp.Payload["contact"].(map[string]interface{}); ok {
		contactBytes, _ := json.Marshal(contactRaw)
		var user User
		if err := json.Unmarshal(contactBytes, &user); err == nil {
			c.cacheUser(&user)
			return &user, nil
		}
	}
	
	return nil, ErrUserNotFound
}

// AddContact adds a user to contacts
func (c *Client) AddContact(contactID int64) (*Contact, error) {
	payload := map[string]interface{}{
		"contactId": contactID,
		"action":    string(ContactActionAdd),
	}
	
	c.Logger.Info().Int64("contactId", contactID).Msg("Adding contact")
	
	resp, err := c.sendAndWait(OpContactUpdate, payload)
	if err != nil {
		return nil, err
	}
	
	if contactRaw, ok := resp.Payload["contact"].(map[string]interface{}); ok {
		contactBytes, _ := json.Marshal(contactRaw)
		var contact Contact
		if err := json.Unmarshal(contactBytes, &contact); err == nil {
			return &contact, nil
		}
	}
	
	return nil, nil
}

// RemoveContact removes a user from contacts
func (c *Client) RemoveContact(contactID int64) error {
	payload := map[string]interface{}{
		"contactId": contactID,
		"action":    string(ContactActionRemove),
	}
	
	c.Logger.Info().Int64("contactId", contactID).Msg("Removing contact")
	
	_, err := c.sendAndWait(OpContactUpdate, payload)
	return err
}

// GetPresence gets user presence information
func (c *Client) GetPresence(userID int64) (*Presence, error) {
	payload := map[string]interface{}{
		"contactId": userID,
	}
	
	c.Logger.Debug().Int64("userId", userID).Msg("Getting presence")
	
	resp, err := c.sendAndWait(OpContactPresence, payload)
	if err != nil {
		return nil, err
	}
	
	if presenceRaw, ok := resp.Payload["presence"].(map[string]interface{}); ok {
		presenceBytes, _ := json.Marshal(presenceRaw)
		var presence Presence
		if err := json.Unmarshal(presenceBytes, &presence); err == nil {
			return &presence, nil
		}
	}
	
	return nil, nil
}

// GetSessions gets active sessions for the current user
func (c *Client) GetSessions() ([]Session, error) {
	c.Logger.Info().Msg("Getting sessions")
	
	resp, err := c.sendAndWait(OpSessionsInfo, map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	
	var sessions []Session
	
	if sessionsRaw, ok := resp.Payload["sessions"].([]interface{}); ok {
		for _, sessionRaw := range sessionsRaw {
			sessionMap, ok := sessionRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			sessionBytes, _ := json.Marshal(sessionMap)
			var session Session
			if err := json.Unmarshal(sessionBytes, &session); err == nil {
				sessions = append(sessions, session)
			}
		}
	}
	
	return sessions, nil
}

// UpdateProfile updates the current user's profile
func (c *Client) UpdateProfile(firstName string, lastName string, description string) error {
	payload := map[string]interface{}{
		"firstName": firstName,
	}
	
	if lastName != "" {
		payload["lastName"] = lastName
	}
	if description != "" {
		payload["description"] = description
	}
	
	c.Logger.Info().Str("firstName", firstName).Msg("Updating profile")
	
	_, err := c.sendAndWait(OpProfile, payload)
	return err
}

// GetContacts gets the contact list
func (c *Client) GetContacts() ([]Contact, error) {
	c.Logger.Info().Msg("Getting contacts")
	
	resp, err := c.sendAndWait(OpContactList, map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	
	var contacts []Contact
	
	if contactsRaw, ok := resp.Payload["contacts"].([]interface{}); ok {
		for _, contactRaw := range contactsRaw {
			contactMap, ok := contactRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			contactBytes, _ := json.Marshal(contactMap)
			var contact Contact
			if err := json.Unmarshal(contactBytes, &contact); err == nil {
				contacts = append(contacts, contact)
			}
		}
	}
	
	return contacts, nil
}

// SearchContacts searches for contacts
func (c *Client) SearchContacts(query string) ([]Contact, error) {
	payload := map[string]interface{}{
		"query": query,
	}
	
	c.Logger.Info().Str("query", query).Msg("Searching contacts")
	
	resp, err := c.sendAndWait(OpContactSearch, payload)
	if err != nil {
		return nil, err
	}
	
	var contacts []Contact
	
	if contactsRaw, ok := resp.Payload["contacts"].([]interface{}); ok {
		for _, contactRaw := range contactsRaw {
			contactMap, ok := contactRaw.(map[string]interface{})
			if !ok {
				continue
			}
			
			contactBytes, _ := json.Marshal(contactMap)
			var contact Contact
			if err := json.Unmarshal(contactBytes, &contact); err == nil {
				contacts = append(contacts, contact)
			}
		}
	}
	
	return contacts, nil
}

// GetUserAvatarURL returns the avatar URL for a user
// In MAX, avatar URL is directly available in the User object
func GetUserAvatarURL(user *User) string {
	if user == nil {
		return ""
	}
	// Prefer baseRawUrl for original quality, fallback to baseUrl
	if user.BaseRawURL != "" {
		return user.BaseRawURL
	}
	return user.BaseURL
}

