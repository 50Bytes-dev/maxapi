package maxclient

import (
	"encoding/json"
	"regexp"
)

var phoneRegex = regexp.MustCompile(`^\+?\d{10,15}$`)

// ValidatePhone validates a phone number format
func ValidatePhone(phone string) bool {
	return phoneRegex.MatchString(phone)
}

// SessionInit initializes a session with the MAX server
func (c *Client) SessionInit(userAgent *UserAgent) error {
	if userAgent == nil {
		userAgent = &UserAgent{
			DeviceType: DeviceTypeWeb,
			Locale:     "ru",
			AppVersion: "25.10.13",
		}
	}
	
	payload := map[string]interface{}{
		"deviceId":  c.DeviceID,
		"userAgent": userAgent,
	}
	
	resp, err := c.sendAndWait(OpSessionInit, payload)
	if err != nil {
		return err
	}
	
	c.Logger.Info().Msg("Session initialized")
	_ = resp // Session init response contains config data
	return nil
}

// RequestAuthCode requests an SMS verification code
func (c *Client) RequestAuthCode(phone string, language string) (string, error) {
	if !ValidatePhone(phone) {
		return "", ErrInvalidPhone
	}
	
	if language == "" {
		language = "ru"
	}
	
	payload := map[string]interface{}{
		"phone":    phone,
		"type":     string(AuthTypeStartAuth),
		"language": language,
	}
	
	c.Logger.Info().Str("phone", phone).Msg("Requesting auth code")
	
	resp, err := c.sendAndWait(OpAuthRequest, payload)
	if err != nil {
		return "", err
	}
	
	token, ok := resp.Payload["token"].(string)
	if !ok {
		return "", NewError("no_token", "No token in response", "Auth Error")
	}
	
	c.Logger.Info().Msg("Auth code requested successfully")
	return token, nil
}

// SubmitAuthCode submits the verification code and returns the result
// Returns: authToken (if login successful), registerToken (if registration needed), error
func (c *Client) SubmitAuthCode(code string, tempToken string) (authToken string, registerToken string, err error) {
	if len(code) != 6 {
		return "", "", ErrInvalidCode
	}
	
	payload := map[string]interface{}{
		"token":         tempToken,
		"verifyCode":    code,
		"authTokenType": string(AuthTypeCheckCode),
	}
	
	c.Logger.Info().Msg("Submitting verification code")
	
	resp, err := c.sendAndWait(OpAuth, payload)
	if err != nil {
		return "", "", err
	}
	
	// Parse tokenAttrs
	tokenAttrs, ok := resp.Payload["tokenAttrs"].(map[string]interface{})
	if !ok {
		return "", "", NewError("invalid_response", "No tokenAttrs in response", "Auth Error")
	}
	
	// Check for LOGIN token (existing user)
	if loginAttrs, ok := tokenAttrs["LOGIN"].(map[string]interface{}); ok {
		if token, ok := loginAttrs["token"].(string); ok {
			c.Logger.Info().Msg("Login successful - existing user")
			return token, "", nil
		}
	}
	
	// Check for REGISTER token (new user)
	if registerAttrs, ok := tokenAttrs["REGISTER"].(map[string]interface{}); ok {
		if token, ok := registerAttrs["token"].(string); ok {
			c.Logger.Info().Msg("Registration required - new user")
			return "", token, nil
		}
	}
	
	return "", "", NewError("no_token", "No valid token in response", "Auth Error")
}

// Register completes registration for a new user
func (c *Client) Register(firstName string, lastName string, registerToken string) (string, error) {
	if firstName == "" {
		return "", NewError("invalid_name", "First name is required", "Validation Error")
	}
	
	payload := map[string]interface{}{
		"firstName": firstName,
		"token":     registerToken,
		"tokenType": string(AuthTypeRegister),
	}
	
	if lastName != "" {
		payload["lastName"] = lastName
	}
	
	c.Logger.Info().Str("firstName", firstName).Msg("Completing registration")
	
	resp, err := c.sendAndWait(OpAuthConfirm, payload)
	if err != nil {
		return "", err
	}
	
	token, ok := resp.Payload["token"].(string)
	if !ok {
		return "", NewError("no_token", "No token in response", "Auth Error")
	}
	
	c.Logger.Info().Msg("Registration completed successfully")
	return token, nil
}

// Login performs sync/login with the auth token
func (c *Client) Login(authToken string) error {
	c.AuthToken = authToken
	
	payload := map[string]interface{}{
		"token":        authToken,
		"interactive":  true,
		"chatsSync":    0,
		"contactsSync": 0,
		"presenceSync": 0,
		"draftsSync":   0,
		"chatsCount":   40,
	}
	
	c.Logger.Info().Msg("Logging in with auth token")
	
	resp, err := c.sendAndWait(OpLogin, payload)
	if err != nil {
		return err
	}
	
	// Parse profile
	if profile, ok := resp.Payload["profile"].(map[string]interface{}); ok {
		if contact, ok := profile["contact"].(map[string]interface{}); ok {
			contactBytes, _ := json.Marshal(contact)
			var me Me
			if err := json.Unmarshal(contactBytes, &me); err == nil {
				c.Me = &me
				c.MaxUserID = me.ID
				c.Logger.Info().Int64("userId", me.ID).Msg("Login successful")
			}
		}
	}
	
	// Parse chats
	if chats, ok := resp.Payload["chats"].([]interface{}); ok {
		c.parseChats(chats)
	}
	
	// Parse contacts
	if contacts, ok := resp.Payload["contacts"].([]interface{}); ok {
		c.parseContacts(contacts)
	}
	
	return nil
}

// parseChats parses chat data from sync response
func (c *Client) parseChats(chats []interface{}) {
	c.Dialogs = nil
	c.Chats = nil
	c.Channels = nil
	
	for _, chatRaw := range chats {
		chatMap, ok := chatRaw.(map[string]interface{})
		if !ok {
			continue
		}
		
		chatBytes, _ := json.Marshal(chatMap)
		chatType, _ := chatMap["type"].(string)
		
		switch ChatType(chatType) {
		case ChatTypeDialog:
			var dialog Dialog
			if err := json.Unmarshal(chatBytes, &dialog); err == nil {
				c.Dialogs = append(c.Dialogs, dialog)
			}
		case ChatTypeChat:
			var chat Chat
			if err := json.Unmarshal(chatBytes, &chat); err == nil {
				c.Chats = append(c.Chats, chat)
			}
		case ChatTypeChannel:
			var channel Chat
			if err := json.Unmarshal(chatBytes, &channel); err == nil {
				c.Channels = append(c.Channels, channel)
			}
		}
	}
	
	c.Logger.Info().
		Int("dialogs", len(c.Dialogs)).
		Int("chats", len(c.Chats)).
		Int("channels", len(c.Channels)).
		Msg("Parsed chats from sync")
}

// parseContacts parses contact data from sync response
func (c *Client) parseContacts(contacts []interface{}) {
	for _, contactRaw := range contacts {
		contactMap, ok := contactRaw.(map[string]interface{})
		if !ok {
			continue
		}
		
		contactBytes, _ := json.Marshal(contactMap)
		var user User
		if err := json.Unmarshal(contactBytes, &user); err == nil {
			c.cacheUser(&user)
		}
	}
	
	c.Logger.Info().Int("count", len(contacts)).Msg("Parsed contacts from sync")
}

// Logout logs out from the current session
func (c *Client) Logout() error {
	if !c.IsConnected() {
		return nil
	}
	
	c.Logger.Info().Msg("Logging out")
	
	_, err := c.sendAndWait(OpLogout, map[string]interface{}{})
	if err != nil {
		c.Logger.Warn().Err(err).Msg("Logout request failed")
	}
	
	c.AuthToken = ""
	c.Me = nil
	c.MaxUserID = 0
	
	return c.Close()
}

// ConnectAndLogin connects and performs login in one step
func (c *Client) ConnectAndLogin(authToken string, userAgent *UserAgent) error {
	if err := c.Connect(); err != nil {
		return err
	}
	
	if err := c.SessionInit(userAgent); err != nil {
		c.Close()
		return err
	}
	
	if err := c.Login(authToken); err != nil {
		c.Close()
		return err
	}
	
	// Start ping loop
	c.StartPingLoop()
	
	return nil
}

