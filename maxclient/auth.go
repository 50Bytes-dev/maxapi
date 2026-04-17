package maxclient

import (
	"encoding/json"
)

// SessionInit initializes a session with the MAX server
func (c *Client) SessionInit(userAgent *UserAgent) error {
	if userAgent == nil {
		userAgent = &UserAgent{
			DeviceType: DeviceTypeWeb,
			Locale:     "ru",
			AppVersion: "26.4.3",
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
	_ = resp
	return nil
}

// QRStartResult is the result of OpAuthQRStart.
type QRStartResult struct {
	QRLink          string `json:"qrLink"`
	TrackID         string `json:"trackId"`
	PollingInterval int    `json:"pollingInterval"`
	TTL             int    `json:"ttl"`
	ExpiresAt       int64  `json:"expiresAt"`
}

// QRStatus is the polling status for a QR auth session.
type QRStatus string

const (
	QRStatusPending    QRStatus = "pending"
	QRStatusScanned    QRStatus = "scanned"
	QRStatusAuthorized QRStatus = "authorized"
	QRStatusExpired    QRStatus = "expired"
)

// RequestQRCode requests a new QR auth session.
func (c *Client) RequestQRCode() (*QRStartResult, error) {
	c.Logger.Info().Msg("Requesting QR auth session")

	resp, err := c.sendAndWait(OpAuthQRStart, map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(resp.Payload)
	if err != nil {
		return nil, err
	}

	var result QRStartResult
	if err := json.Unmarshal(payloadBytes, &result); err != nil {
		return nil, err
	}

	if result.TrackID == "" {
		return nil, NewError("no_track_id", "No trackId in response", "Auth Error")
	}

	c.Logger.Info().Str("trackId", result.TrackID).Int("ttl", result.TTL).Msg("QR session created")
	return &result, nil
}

// PollQRStatus polls the status of an ongoing QR auth session.
// Returns QRStatusScanned when the user has confirmed in the mobile app.
// Returns QRStatusExpired when the session is no longer valid.
func (c *Client) PollQRStatus(trackID string) (QRStatus, error) {
	if trackID == "" {
		return "", NewError("invalid_track_id", "trackId is required", "Validation Error")
	}

	resp, err := c.sendAndWait(OpAuthQRStatus, map[string]interface{}{
		"trackId": trackID,
	})
	if err != nil {
		if e, ok := err.(*Error); ok && e.Code == "track.not.found" {
			return QRStatusExpired, nil
		}
		return "", err
	}

	status, ok := resp.Payload["status"].(map[string]interface{})
	if !ok {
		return QRStatusPending, nil
	}

	if loginAvailable, _ := status["loginAvailable"].(bool); loginAvailable {
		return QRStatusScanned, nil
	}

	return QRStatusPending, nil
}

// ConfirmQRLogin exchanges a scanned QR session for an auth token.
// Call only after PollQRStatus returns QRStatusScanned.
func (c *Client) ConfirmQRLogin(trackID string) (string, error) {
	if trackID == "" {
		return "", NewError("invalid_track_id", "trackId is required", "Validation Error")
	}

	c.Logger.Info().Str("trackId", trackID).Msg("Confirming QR login")

	resp, err := c.sendAndWait(OpAuthQRConfirm, map[string]interface{}{
		"trackId": trackID,
	})
	if err != nil {
		return "", err
	}

	tokenAttrs, ok := resp.Payload["tokenAttrs"].(map[string]interface{})
	if !ok {
		return "", NewError("invalid_response", "No tokenAttrs in response", "Auth Error")
	}

	loginAttrs, ok := tokenAttrs["LOGIN"].(map[string]interface{})
	if !ok {
		return "", NewError("no_login_token", "No LOGIN token in response", "Auth Error")
	}

	token, ok := loginAttrs["token"].(string)
	if !ok || token == "" {
		return "", NewError("no_token", "Empty LOGIN token", "Auth Error")
	}

	if profile, ok := resp.Payload["profile"].(map[string]interface{}); ok {
		if contact, ok := profile["contact"].(map[string]interface{}); ok {
			contactBytes, _ := json.Marshal(contact)
			var me Me
			if err := json.Unmarshal(contactBytes, &me); err == nil {
				c.Me = &me
				c.MaxUserID = me.ID
			}
		}
	}

	c.Logger.Info().Msg("QR login confirmed, token received")
	return token, nil
}

// Login performs sync/login with the auth token and returns raw sync data
func (c *Client) Login(authToken string) (map[string]interface{}, error) {
	c.AuthToken = authToken

	payload := map[string]interface{}{
		"chatsCount":   100,
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

	if chatsRaw, ok := resp.Payload["chats"].([]interface{}); ok {
		c.Logger.Info().Int("count", len(chatsRaw)).Msg("Got chats from login")
	}

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
		"token":        c.AuthToken,
	}

	c.Logger.Info().Msg("Syncing data")

	resp, err := c.sendAndWait(OpSync, payload)
	if err != nil {
		return nil, err
	}

	if chatsRaw, ok := resp.Payload["chats"].([]interface{}); ok {
		c.Logger.Info().Int("count", len(chatsRaw)).Msg("Got chats from sync")
	}

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

	c.StartPingLoop()

	return syncData, nil
}
