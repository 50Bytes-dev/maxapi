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

// Login performs sync/login with the auth token and returns raw sync data
func (c *Client) Login(authToken string) (map[string]interface{}, error) {
	c.AuthToken = authToken

	payload := map[string]interface{}{
		"chatsCount":   100, // Max allowed by API (default was 40)
		"chatsSync":    0,
		"contactsSync": 0,
		"draftsSync":   0,
		"interactive":  true,
		"presenceSync": -1,
		"token":        authToken,
	}

	c.Logger.Info().Msg("Logging in with auth token")

	resp, err := c.sendAndWait(OpLogin, payload)
	if err != nil {
		return nil, err
	}

	// Log chat count
	if chatsRaw, ok := resp.Payload["chats"].([]interface{}); ok {
		c.Logger.Info().Int("count", len(chatsRaw)).Msg("Got chats from login")
	}

	// Parse profile to set c.Me and c.MaxUserID
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

	// Extract participant IDs from chats and fetch their full contact data
	contactIDs := c.extractParticipantIDsFromPayload(resp.Payload)
	if len(contactIDs) > 0 {
		contacts, err := c.fetchContactsByIDs(contactIDs)
		if err != nil {
			c.Logger.Warn().Err(err).Msg("Failed to fetch contacts")
		} else {
			// Add fetched contacts to payload (replaces empty contacts array from login)
			resp.Payload["contacts"] = contacts
		}
	}

	return resp.Payload, nil
}

// Sync performs sync without re-login (for reconnects) using opcode 21
func (c *Client) Sync() (map[string]interface{}, error) {
	if c.AuthToken == "" {
		return nil, NewError("no_token", "Auth token not set", "Sync Error")
	}

	payload := map[string]interface{}{
		"chatsCount":   100,
		"chatsSync":    0,
		"contactsSync": 0,
		"draftsSync":   0,
		"interactive":  true,
		"presenceSync": -1,
		"token":        c.AuthToken, // Token required for sync
	}

	c.Logger.Info().Msg("Syncing data")

	resp, err := c.sendAndWait(OpSync, payload)
	if err != nil {
		return nil, err
	}

	// Log chat count
	if chatsRaw, ok := resp.Payload["chats"].([]interface{}); ok {
		c.Logger.Info().Int("count", len(chatsRaw)).Msg("Got chats from sync")
	}

	// Extract participant IDs from chats and fetch their full contact data
	contactIDs := c.extractParticipantIDsFromPayload(resp.Payload)
	if len(contactIDs) > 0 {
		contacts, err := c.fetchContactsByIDs(contactIDs)
		if err != nil {
			c.Logger.Warn().Err(err).Msg("Failed to fetch contacts")
		} else {
			resp.Payload["contacts"] = contacts
		}
	}

	return resp.Payload, nil
}

// extractParticipantIDsFromPayload extracts unique participant IDs from chats in payload
func (c *Client) extractParticipantIDsFromPayload(payload map[string]interface{}) []int64 {
	idSet := make(map[int64]bool)

	chatsRaw, ok := payload["chats"].([]interface{})
	if !ok {
		return nil
	}

	for _, chatRaw := range chatsRaw {
		chat, ok := chatRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if participants, ok := chat["participants"].(map[string]interface{}); ok {
			for idStr := range participants {
				if parsed, err := parseInt64(idStr); err == nil && parsed > 0 {
					idSet[parsed] = true
				}
			}
		}
	}

	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	return ids
}

// parseInt64 parses a string to int64
func parseInt64(s string) (int64, error) {
	var result int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrInvalidResponse
		}
		result = result*10 + int64(c-'0')
	}
	return result, nil
}

// fetchContactsByIDs fetches contacts by their IDs using opcode 32
func (c *Client) fetchContactsByIDs(contactIDs []int64) ([]map[string]interface{}, error) {
	if len(contactIDs) == 0 {
		return nil, nil
	}

	payload := map[string]interface{}{
		"contactIds": contactIDs,
	}

	c.Logger.Info().Int("count", len(contactIDs)).Msg("Fetching contacts by IDs")

	resp, err := c.sendAndWait(OpContactInfo, payload)
	if err != nil {
		return nil, err
	}

	var contacts []map[string]interface{}
	if contactsRaw, ok := resp.Payload["contacts"].([]interface{}); ok {
		for _, contactRaw := range contactsRaw {
			if contactMap, ok := contactRaw.(map[string]interface{}); ok {
				contacts = append(contacts, contactMap)
			}
		}
	}

	c.Logger.Info().Int("count", len(contacts)).Msg("Fetched contacts")
	return contacts, nil
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
func (c *Client) ConnectAndLogin(authToken string, userAgent *UserAgent) (map[string]interface{}, error) {
	if err := c.Connect(); err != nil {
		return nil, err
	}

	if err := c.SessionInit(userAgent); err != nil {
		c.Close()
		return nil, err
	}

	syncData, err := c.Login(authToken)
	if err != nil {
		c.Close()
		return nil, err
	}

	// Start ping loop
	c.StartPingLoop()

	return syncData, nil
}

// ConnectAndSync connects and performs sync without re-login (for reconnects)
func (c *Client) ConnectAndSync(userAgent *UserAgent) (map[string]interface{}, error) {
	if err := c.Connect(); err != nil {
		return nil, err
	}

	if err := c.SessionInit(userAgent); err != nil {
		c.Close()
		return nil, err
	}

	syncData, err := c.Sync()
	if err != nil {
		c.Close()
		return nil, err
	}

	// Start ping loop
	c.StartPingLoop()

	return syncData, nil
}
