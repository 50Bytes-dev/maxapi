package maxclient

import (
	"encoding/json"
	"sort"
	"strings"
)

// The web client the gateway presents itself as: SESSION_INIT's userAgent
// and the WebSocket handshake's User-Agent tell the same browser. The fields
// are those MAX's web client sends (PyMax, maxrs, max-account-api, max-cli,
// 2026); before 3.0.0 only deviceType, locale and appVersion went, and the
// handshake carried a truncated User-Agent.
const (
	WebAppVersion = "26.8.4"
	WebUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

// DefaultUserAgent is the userAgent of SESSION_INIT.
func DefaultUserAgent() *UserAgent {
	return &UserAgent{
		DeviceType:      DeviceTypeWeb,
		Locale:          "ru",
		DeviceLocale:    "ru",
		OsVersion:       "macOS",
		DeviceName:      "Chrome",
		HeaderUserAgent: WebUserAgent,
		AppVersion:      WebAppVersion,
		Screen:          "1080x1920 1.0x",
		Timezone:        "Europe/Moscow",
	}
}

// SessionInit initializes a session with the MAX server
func (c *Client) SessionInit(userAgent *UserAgent) error {
	if userAgent == nil {
		userAgent = DefaultUserAgent()
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

// LoginStep is what MAX answered a QR confirmation (opcode 291) or a cloud
// password with: the login token, or the next step MAX asks for.
type LoginStep struct {
	// Token is the LOGIN token: the login is done.
	Token string
	// Password is set when MAX asks for the account's cloud password.
	Password *PasswordChallenge
	// Unsupported names a step the client does not support: the keys MAX
	// answered with (a token kind under tokenAttrs, or the payload's keys).
	Unsupported string
}

// PasswordChallenge is MAX's request for the account's cloud password (two
// factor authentication), answered with SubmitPassword.
type PasswordChallenge struct {
	TrackID string `json:"trackId"`
	Hint    string `json:"hint,omitempty"`
	Email   string `json:"email,omitempty"`
}

// ConfirmQRLogin exchanges a scanned QR session for the login token, or for
// the next step MAX asks for. Call only after PollQRStatus returns
// QRStatusScanned.
func (c *Client) ConfirmQRLogin(trackID string) (*LoginStep, error) {
	if trackID == "" {
		return nil, NewError("invalid_track_id", "trackId is required", "Validation Error")
	}

	c.Logger.Info().Str("trackId", trackID).Msg("Confirming QR login")

	resp, err := c.sendAndWait(OpAuthQRConfirm, map[string]interface{}{
		"trackId": trackID,
	})
	if err != nil {
		return nil, err
	}
	return c.loginStep(resp, "QR confirmation"), nil
}

// SubmitPassword sends the account's cloud password for challenge trackID
// (opcode 115). MAX answers the token, or refuses with an error code
// (PasswordRefusal).
//
// The step is MAX's, as the open clients send it after a QR confirmation
// answered with an empty tokenAttrs and a passwordChallenge (read
// 2026-11-07, docs/research/max-qr-login-2026-11-07.md):
// github.com/MaxApiTeam/PyMax (MIT) src/pymax/auth/qr.py,
// github.com/maxsos1/max-account-api (MIT) src/auth.ts,
// github.com/WireCatLabs/max-cli (Apache-2.0) src/session/login.ts.
func (c *Client) SubmitPassword(trackID, password string) (*LoginStep, error) {
	if trackID == "" {
		return nil, NewError("invalid_track_id", "trackId is required", "Validation Error")
	}
	resp, err := c.sendAndWait(OpAuthLoginCheckPassword, map[string]interface{}{
		"trackId":  trackID,
		"password": password,
	})
	if err != nil {
		return nil, err
	}
	return c.loginStep(resp, "cloud password"), nil
}

// How MAX refused a cloud password (PasswordRefusal).
const (
	PasswordWrong       = "wrong"       // the password itself; the same trackId takes another one
	PasswordExpired     = "expired"     // the challenge is over: its time ran out, too many attempts, the track is gone
	PasswordUnsupported = "unsupported" // MAX will not take a password from this client (an old appVersion)
	PasswordOther       = "other"       // any other refusal
)

// PasswordRefusal tells how MAX refused a cloud password by its error code.
// Known codes: password2fa.wrong (measured by max-cli and Telemax, 2026),
// second.factor.disabled.update.app (vkmax-node), track.not.found (an
// expired or used track). The rest is told by markers in the code.
func PasswordRefusal(code string) string {
	lower := strings.ToLower(code)
	switch {
	case lower == "password2fa.wrong" || lower == "password.invalid" || lower == "error.password.invalid":
		return PasswordWrong
	case strings.Contains(lower, "update.app") || strings.Contains(lower, "unsupported"):
		return PasswordUnsupported
	case lower == "track.not.found" || strings.Contains(lower, "expired") || strings.Contains(lower, "time") ||
		strings.Contains(lower, "attempt") || strings.Contains(lower, "exceed") || strings.Contains(lower, "limit") ||
		strings.Contains(lower, "flood"):
		return PasswordExpired
	case strings.Contains(lower, "password") || strings.Contains(lower, "wrong"):
		return PasswordWrong
	}
	return PasswordOther
}

// loginStep reads MAX's answer to a login step. It logs the keys of the
// answer, never a value: the top-level keys of the payload and those under
// tokenAttrs, at Warn when there is no LOGIN token.
func (c *Client) loginStep(resp *Response, what string) *LoginStep {
	top := sortedKeys(resp.Payload)
	tokenAttrs, _ := resp.Payload["tokenAttrs"].(map[string]interface{})
	attrKeys := sortedKeys(tokenAttrs)

	if login, ok := tokenAttrs["LOGIN"].(map[string]interface{}); ok {
		if token, _ := login["token"].(string); token != "" {
			c.Logger.Info().Strs("payloadKeys", top).Strs("tokenAttrsKeys", attrKeys).Msg("MAX handed out the login token: " + what)
			c.rememberProfile(resp.Payload)
			return &LoginStep{Token: token}
		}
	}
	c.Logger.Warn().Strs("payloadKeys", top).Strs("tokenAttrsKeys", attrKeys).Msg("MAX answered the " + what + " without a LOGIN token")

	if pc, ok := resp.Payload["passwordChallenge"].(map[string]interface{}); ok {
		c.Logger.Info().Strs("passwordChallengeKeys", sortedKeys(pc)).Msg("MAX asks for the cloud password")
		ch := &PasswordChallenge{}
		ch.TrackID, _ = pc["trackId"].(string)
		ch.Hint, _ = pc["hint"].(string)
		ch.Email, _ = pc["email"].(string)
		if ch.TrackID != "" {
			return &LoginStep{Password: ch}
		}
	}
	switch {
	case len(attrKeys) > 0:
		return &LoginStep{Unsupported: strings.Join(attrKeys, ",")}
	case len(top) > 0:
		return &LoginStep{Unsupported: strings.Join(top, ",")}
	}
	return &LoginStep{Unsupported: "empty"}
}

// rememberProfile keeps the account of a login answer's profile as Me.
func (c *Client) rememberProfile(payload map[string]interface{}) {
	profile, ok := payload["profile"].(map[string]interface{})
	if !ok {
		return
	}
	contact, ok := profile["contact"].(map[string]interface{})
	if !ok {
		return
	}
	contactBytes, _ := json.Marshal(contact)
	var me Me
	if err := json.Unmarshal(contactBytes, &me); err == nil {
		c.Me = &me
		c.MaxUserID = me.ID
	}
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

	timeout := c.LoginTimeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	resp, err := c.sendAndWaitWithTimeout(OpLogin, payload, timeout)
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

	return resp.Payload, nil
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

// ConnectAndLogin connects and performs login in one step. A failure of the
// login itself comes back as a *LoginError.
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
		return nil, &LoginError{Err: err}
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
