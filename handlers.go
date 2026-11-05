package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"maxapi/maxclient"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog/log"
	"github.com/skip2/go-qrcode"
)

type Values struct {
	m map[string]string
}

func (v Values) Get(key string) string {
	return v.m[key]
}

// Admin middleware
func (s *server) authadmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token != *adminToken {
			s.Respond(w, r, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// User token middleware
func (s *server) authalice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ctx context.Context
		txtid := ""
		name := ""
		webhook := ""
		events := ""
		proxyURL := ""

		token := r.Header.Get("token")
		if token == "" {
			token = strings.Join(r.URL.Query()["token"], "")
		}

		myuserinfo, found := userinfocache.Get(token)
		if !found {
			log.Info().Msg("Looking for user information in DB")
			rows, err := s.db.Query("SELECT id, name, webhook, max_user_id, events, proxy_url, history FROM users WHERE token=$1 LIMIT 1", token)
			if err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			defer rows.Close()

			var history sql.NullInt64
			var maxUserID sql.NullInt64
			for rows.Next() {
				err = rows.Scan(&txtid, &name, &webhook, &maxUserID, &events, &proxyURL, &history)
				if err != nil {
					s.Respond(w, r, http.StatusInternalServerError, err)
					return
				}

				historyStr := "0"
				if history.Valid {
					historyStr = fmt.Sprintf("%d", history.Int64)
				}

				maxUserIDStr := ""
				if maxUserID.Valid {
					maxUserIDStr = fmt.Sprintf("%d", maxUserID.Int64)
				}

				v := Values{map[string]string{
					"Id":        txtid,
					"Name":      name,
					"MaxUserID": maxUserIDStr,
					"Webhook":   webhook,
					"Token":     token,
					"Proxy":     proxyURL,
					"Events":    events,
					"History":   historyStr,
				}}

				userinfocache.Set(token, v, cache.NoExpiration)
				log.Info().Str("name", name).Msg("User info from DB")
				ctx = context.WithValue(r.Context(), "userinfo", v)
			}
		} else {
			ctx = context.WithValue(r.Context(), "userinfo", myuserinfo)
			log.Info().Str("name", myuserinfo.(Values).Get("Name")).Msg("User info from Cache")
			txtid = myuserinfo.(Values).Get("Id")
		}

		if txtid == "" {
			s.Respond(w, r, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ========== QR SESSION HELPERS ==========

// maxQRRefreshes caps how many times a QR session may be reissued on the same
// auth WebSocket before we treat the session as abandoned.
const maxQRRefreshes = 10

// renderAndStoreQR renders the QR link to a base64 PNG and writes the full
// session state (trackId, base64, link, expiresAt) to the users row.
func (s *server) renderAndStoreQR(userID string, result *maxclient.QRStartResult) (string, error) {
	png, err := qrcode.Encode(result.QRLink, qrcode.Medium, 256)
	if err != nil {
		return "", fmt.Errorf("QR render failed: %v", err)
	}
	qrCodeBase64 := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

	if _, err := s.db.Exec(
		`UPDATE users SET qr_track_id=$1, qr_code_base64=$2, qr_link=$3, qr_expires_at=$4 WHERE id=$5`,
		result.TrackID, qrCodeBase64, result.QRLink, result.ExpiresAt, userID,
	); err != nil {
		log.Error().Err(err).Str("userID", userID).Msg("Failed to persist QR state")
		return qrCodeBase64, nil
	}
	return qrCodeBase64, nil
}

// clearQRState wipes any stored QR artefacts for the user. Called on terminal
// QRExpired, on a successful scan, and on disconnect.
func (s *server) clearQRState(userID string) {
	_, _ = s.db.Exec(
		`UPDATE users SET qr_track_id='', qr_code_base64='', qr_link='', qr_expires_at=0 WHERE id=$1`,
		userID,
	)
}

// startQRSession opens the MAX auth WebSocket, requests a QR session, persists
// state, installs a shell MyClient (so QR-lifecycle webhooks can fire before
// any /session/connect call) and spawns the watcher goroutine. Used by the
// Connect handler when the user has no auth token yet.
func (s *server) startQRSession(userID, token string) (*maxclient.QRStartResult, error) {
	var deviceID string
	_ = s.db.Get(&deviceID, "SELECT device_id FROM users WHERE id=$1", userID)
	if deviceID == "" {
		deviceID = uuid.New().String()
	}

	if old := clientManager.GetMaxClient(userID); old != nil {
		old.Close()
		clientManager.DeleteMaxClient(userID)
	}

	logger := log.With().Str("userID", userID).Logger()
	client := maxclient.NewClient(deviceID, logger)

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("connection failed: %v", err)
	}
	if err := client.SessionInit(nil); err != nil {
		client.Close()
		return nil, fmt.Errorf("session init failed: %v", err)
	}

	result, err := client.RequestQRCode()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("QR request failed: %v", err)
	}

	if _, err := s.db.Exec("UPDATE users SET device_id=$1 WHERE id=$2", deviceID, userID); err != nil {
		log.Error().Err(err).Msg("Failed to persist device_id")
	}
	qrCodeBase64, err := s.renderAndStoreQR(userID, result)
	if err != nil {
		client.Close()
		return nil, err
	}

	clientManager.SetMaxClient(userID, client)
	client.StartPingLoop()

	s.ensureHTTPClient(userID)

	if clientManager.GetMyClient(userID) == nil {
		clientManager.SetMyClient(userID, &MyClient{
			userID: userID,
			token:  token,
			db:     s.db,
			s:      s,
		})
	}

	go s.watchQRSession(userID, client, result)

	if mycli := clientManager.GetMyClient(userID); mycli != nil {
		sendEventWithWebHook(mycli, map[string]interface{}{
			"type":         maxclient.EventTypeQRGenerated,
			"qrLink":       result.QRLink,
			"qrCodeBase64": qrCodeBase64,
			"trackId":      result.TrackID,
			"expiresAt":    result.ExpiresAt,
		}, "")
	}

	return result, nil
}

// watchQRSession polls opcode 289 in the background until the session is
// scanned, abandoned (too many refreshes), or replaced. Auto-refreshes the QR
// when the current one expires on the same auth WebSocket, firing QRGenerated
// again — mirrors whatsmeow's qrChannel reissue behaviour.
func (s *server) watchQRSession(userID string, client *maxclient.Client, start *maxclient.QRStartResult) {
	result := start
	refreshes := 0
	scannedNotified := false

	interval := func() time.Duration {
		d := time.Duration(result.PollingInterval) * time.Millisecond
		if d < time.Second {
			d = 5 * time.Second
		}
		return d
	}
	deadline := time.Now().Add(time.Duration(result.TTL)*time.Millisecond + 15*time.Second)

	tryRefresh := func() bool {
		if refreshes >= maxQRRefreshes {
			return false
		}
		newResult, err := client.RequestQRCode()
		if err != nil {
			log.Warn().Err(err).Str("userID", userID).Msg("QR refresh failed")
			return false
		}
		qrCodeBase64, err := s.renderAndStoreQR(userID, newResult)
		if err != nil {
			log.Warn().Err(err).Str("userID", userID).Msg("QR refresh render failed")
			return false
		}
		if mycli := clientManager.GetMyClient(userID); mycli != nil {
			sendEventWithWebHook(mycli, map[string]interface{}{
				"type":         maxclient.EventTypeQRGenerated,
				"qrLink":       newResult.QRLink,
				"qrCodeBase64": qrCodeBase64,
				"trackId":      newResult.TrackID,
				"expiresAt":    newResult.ExpiresAt,
			}, "")
		}
		result = newResult
		deadline = time.Now().Add(time.Duration(result.TTL)*time.Millisecond + 15*time.Second)
		refreshes++
		return true
	}

	for {
		if time.Now().After(deadline) {
			if tryRefresh() {
				continue
			}
			s.clearQRState(userID)
			s.finishQRSession(userID, client, maxclient.EventTypeQRExpired, nil)
			return
		}

		time.Sleep(interval())
		if clientManager.GetMaxClient(userID) != client {
			return // Client replaced or cancelled; stop watching.
		}

		status, err := client.PollQRStatus(result.TrackID)
		if err != nil {
			log.Warn().Err(err).Str("userID", userID).Msg("QR poll failed")
			continue
		}

		switch status {
		case maxclient.QRStatusPending:
			continue
		case maxclient.QRStatusExpired:
			if tryRefresh() {
				continue
			}
			s.clearQRState(userID)
			s.finishQRSession(userID, client, maxclient.EventTypeQRExpired, nil)
			return
		case maxclient.QRStatusScanned:
			if !scannedNotified {
				if mycli := clientManager.GetMyClient(userID); mycli != nil {
					sendEventWithWebHook(mycli, map[string]interface{}{
						"type":    maxclient.EventTypeQRScanned,
						"trackId": result.TrackID,
					}, "")
				}
				scannedNotified = true
			}
			authToken, err := client.ConfirmQRLogin(result.TrackID)
			if err != nil {
				log.Error().Err(err).Str("userID", userID).Msg("QR confirm failed")
				s.clearQRState(userID)
				s.finishQRSession(userID, client, maxclient.EventTypeQRExpired, map[string]interface{}{
					"reason": err.Error(),
				})
				return
			}
			if _, err := s.db.Exec(
				`UPDATE users SET auth_token=$1, qr_track_id='', qr_code_base64='', qr_link='', qr_expires_at=0 WHERE id=$2`,
				authToken, userID,
			); err != nil {
				log.Error().Err(err).Str("userID", userID).Msg("Failed to persist auth token")
			}
			// Refresh cached userinfo so subsequent requests see the new token.
			userToken := s.tokenForUser(userID)
			if cached, ok := userinfocache.Get(userToken); ok {
				if v, ok := cached.(Values); ok {
					v.m["AuthToken"] = authToken
					userinfocache.Set(userToken, v, cache.NoExpiration)
				}
			}
			// Tear down the temporary auth WebSocket and all per-user state —
			// completeQRAuth rebuilds it from scratch via initClient, so any
			// webhook fires only after the full client is live.
			client.Close()
			cleanupClient(userID)
			s.completeQRAuth(userID, authToken)
			return
		}
	}
}

// tokenForUser looks up the user's API token (used to invalidate cached userinfo).
func (s *server) tokenForUser(userID string) string {
	var token string
	_ = s.db.Get(&token, "SELECT token FROM users WHERE id=$1", userID)
	return token
}

// completeQRAuth runs the full MAX client bring-up synchronously after a
// successful QR scan. It blocks until the session WebSocket is connected (or
// the attempt fails) so that the QRAuthorized webhook only fires when the
// client is actually ready — no dead window between "scanned" and "usable".
// Event subscriptions are read from the user's DB row, the same source
// /session/connect uses.
func (s *server) completeQRAuth(userID, authToken string) {
	if clientManager.IsConnected(userID) {
		return
	}

	var row struct {
		Token    string `db:"token"`
		DeviceID string `db:"device_id"`
		Events   string `db:"events"`
	}
	if err := s.db.Get(&row, "SELECT token, COALESCE(device_id,'') AS device_id, COALESCE(events,'') AS events FROM users WHERE id=$1", userID); err != nil {
		log.Error().Err(err).Str("userID", userID).Msg("completeQRAuth: failed to load user")
		return
	}

	var subscribedEvents []string
	for _, arg := range strings.Split(row.Events, ",") {
		arg = strings.TrimSpace(arg)
		if arg != "" && Find(supportedEventTypes, arg) && !Find(subscribedEvents, arg) {
			subscribedEvents = append(subscribedEvents, arg)
		}
	}

	// Retire any lingering loop goroutine and install a fresh kill channel
	// before initClient, so runClientLoop's first GetKillChannel picks ours up.
	clientManager.SendKill(userID)
	clientManager.NewKillChannel(userID)

	log.Info().Str("userID", userID).Msg("Auto-connecting to MAX after QR authorisation")
	client, mycli, _, err := s.initClient(userID, authToken, row.DeviceID, row.Token, subscribedEvents)
	if err != nil {
		// initClient has already fired AuthExpired (if applicable) and cleaned
		// state. Nothing else to announce here — the authToken is persisted,
		// so callers can retry via POST /session/connect if desired.
		return
	}

	// Now that the session WebSocket is live, fire QRAuthorized (so consumers
	// know auth succeeded) immediately followed by Sync (so consumers have
	// maxUserID and any server-provided profile data in hand).
	sendEventWithWebHook(mycli, map[string]interface{}{
		"type":      maxclient.EventTypeQRAuthorized,
		"authToken": authToken,
	}, "")
	sendEventWithWebHook(mycli, buildSyncPostmap(false, client), "")

	go s.runClientLoop(userID, authToken, client, mycli)
}

// finishQRSession closes the temporary auth client, fires a terminal failure
// event (QRExpired or QR confirm error), and purges the per-user state that
// the QR flow created. Only used for aborted paths — a successful scan hands
// off to completeQRAuth, which keeps and re-uses the clients it needs.
func (s *server) finishQRSession(txtid string, client *maxclient.Client, eventType string, extra map[string]interface{}) {
	if client != nil {
		client.Close()
	}
	clientManager.DeleteMaxClient(txtid)

	if mycli := clientManager.GetMyClient(txtid); mycli != nil {
		payload := map[string]interface{}{"type": eventType}
		for k, v := range extra {
			payload[k] = v
		}
		sendEventWithWebHook(mycli, payload, "")
	}

	// QR path created a MyClient and HTTPClient purely for webhook dispatch.
	// The session is over — drop them so a fresh retry starts from a clean slate.
	clientManager.DeleteMyClient(txtid)
	clientManager.DeleteHTTPClient(txtid)
}

// ========== SESSION ENDPOINTS ==========

// Connect opens a MAX session. When the user has no auth token yet, a QR auth
// session is started server-side and the caller is expected to fetch the code
// via GET /session/qr (or consume the QRGenerated webhook). When an auth token
// is already stored, the saved credentials are used to ConnectAndLogin
// immediately. Calling twice while already connected is idempotent — only the
// subscription list is refreshed.
func (s *server) Connect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		decoder := json.NewDecoder(r.Body)
		var t ConnectBody
		if err := decoder.Decode(&t); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		// Process subscriptions (done before the connected-check so repeat callers
		// can update their subscription list without reconnecting).
		var subscribedEvents []string
		for _, arg := range t.Subscribe {
			if Find(supportedEventTypes, arg) && !Find(subscribedEvents, arg) {
				subscribedEvents = append(subscribedEvents, arg)
			}
		}

		eventstring := strings.Join(subscribedEvents, ",")
		_, err := s.db.Exec("UPDATE users SET events=$1 WHERE id=$2", eventstring, txtid)
		if err != nil {
			log.Warn().Err(err).Msg("Could not set events in users table")
		}

		v := updateUserInfo(r.Context().Value("userinfo"), "Events", eventstring)
		userinfocache.Set(token, v, cache.NoExpiration)
		clientManager.UpdateMyClientSubscriptions(txtid, subscribedEvents)

		var authToken, deviceID string
		err = s.db.QueryRow("SELECT auth_token, device_id FROM users WHERE id=$1", txtid).Scan(&authToken, &deviceID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("DB error: %v", err))
			return
		}

		if authToken == "" {
			// No stored auth yet. A live client here is a QR-auth websocket, not
			// a logged-in session, so don't report alreadyConnected — restart/continue
			// the QR flow instead. startQRSession closes any stale client first.
			if _, err := s.startQRSession(txtid, token); err != nil {
				s.Respond(w, r, http.StatusInternalServerError, err)
				return
			}
			s.Respond(w, r, http.StatusOK, map[string]interface{}{
				"success": true,
				"details": "Awaiting QR scan",
				"events":  eventstring,
			})
			return
		}

		if clientManager.IsConnected(txtid) {
			resp := map[string]interface{}{
				"success":          true,
				"details":          "Already connected to MAX",
				"events":           eventstring,
				"alreadyConnected": true,
			}
			addPauseFields(resp, s.pauseOf(s.maxUserIDOf(txtid)))
			s.Respond(w, r, http.StatusOK, resp)
			return
		}

		log.Info().Str("userID", txtid).Msg("Connecting to MAX")
		clientManager.NewKillChannel(txtid)
		go s.startClient(txtid, authToken, deviceID, token, subscribedEvents)

		if !t.Immediate {
			time.Sleep(5 * time.Second)
			if !clientManager.IsConnected(txtid) {
				s.Respond(w, r, http.StatusInternalServerError, errors.New("failed to connect"))
				return
			}
		}

		s.Respond(w, r, http.StatusOK, map[string]interface{}{
			"success": true,
			"details": "Connected to MAX",
			"events":  eventstring,
		})
	}
}

// GetQR returns the current QR code PNG (base64) for an in-progress auth
// session. Intended for pull-based UIs; webhook consumers receive the same QR
// via the QRGenerated event. Mirrors wuzapi's /session/qr so that a client can
// share a single abstraction across both providers.
func (s *server) GetQR() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		if clientManager.GetMaxClient(txtid) == nil {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("no session"))
			return
		}
		// "logged in" means we have a stored auth_token, not merely that a websocket
		// is up — the QR auth websocket is also a live connection.
		var authToken string
		_ = s.db.Get(&authToken, "SELECT COALESCE(auth_token,'') FROM users WHERE id=$1", txtid)
		if authToken != "" {
			s.Respond(w, r, http.StatusInternalServerError, errors.New("already logged in"))
			return
		}

		var qrcodeStr string
		_ = s.db.Get(&qrcodeStr, "SELECT COALESCE(qr_code_base64,'') FROM users WHERE id=$1", txtid)

		s.Respond(w, r, http.StatusOK, map[string]interface{}{
			"success": true,
			"qrcode":  qrcodeStr,
		})
	}
}

// Disconnect disconnects from MAX. Also cancels an in-progress QR auth
// session if one is active — so callers have a single endpoint to tear down
// whatever state exists server-side.
func (s *server) Disconnect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		// Cancel any in-progress QR auth session first. The watcher observes the
		// replaced/deleted maxClient and exits cleanly.
		if client := clientManager.GetMaxClient(txtid); client != nil && !clientManager.IsConnected(txtid) {
			client.Close()
			clientManager.DeleteMaxClient(txtid)
			s.clearQRState(txtid)
		}

		if !clientManager.SendKill(txtid) {
			// No live loop goroutine to signal; drop any stale channel entry.
			clientManager.DeleteKillChannel(txtid)
		}

		_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", txtid)
		if err != nil {
			log.Error().Err(err).Msg("Failed to update disconnected status")
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Disconnected",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// Logout ends the MAX session (opcode 20) and erases the stored login. The
// gateway user stays, with its token, webhook, settings and lookups: a new
// login is a QR login through POST /session/connect. Deleting the user is
// DELETE /admin/users/{id}. No webhook is sent: the caller asked for it.
func (s *server) Logout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client != nil && client.IsConnected() {
			client.Logout() // Sends opcode 20 and closes the client
		}
		s.endLogin(txtid)

		s.Respond(w, r, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Logged out",
		})
	}
}

// GetStatus returns connection status, and the account's name and photo
// (C11): the live session's, else those of the last login.
func (s *server) GetStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		connected := clientManager.IsConnected(txtid)

		var maxUserID int64
		var name, photo string
		client := clientManager.GetMaxClient(txtid)
		if client != nil && client.Me != nil {
			maxUserID = client.MaxUserID
			name, photo = accountProfile(client)
		}

		// Check if user has auth_token (authenticated)
		var row struct {
			AuthToken string `db:"auth_token"`
			Name      string `db:"max_name"`
			Photo     string `db:"max_photo"`
		}
		_ = s.db.Get(&row, "SELECT COALESCE(auth_token, '') AS auth_token, COALESCE(max_name, '') AS max_name, COALESCE(max_photo, '') AS max_photo FROM users WHERE id=$1", txtid)
		authenticated := row.AuthToken != ""
		if name == "" && photo == "" && authenticated {
			name, photo = row.Name, row.Photo
		}

		response := map[string]interface{}{
			"success":       true,
			"connected":     connected,
			"authenticated": authenticated,
			"loggedIn":      connected && authenticated,
			"maxUserID":     maxUserID,
		}
		if name != "" {
			response["name"] = name
		}
		if photo != "" {
			response["photo"] = photo
		}
		// The pause of the MAX account this user is logged into, also without
		// a session. A paused account keeps its session: connected stays as
		// it is, and callers read paused first.
		addPauseFields(response, s.pauseOf(s.maxUserIDOf(txtid)))

		s.Respond(w, r, http.StatusOK, response)
	}
}

// Resume lifts the pause of the MAX account the user is logged into (see
// pauses.go), for every gateway user of that account, and returns the account
// as maxUserID: the caller resumes all its connections of it. An account that
// is not paused answers the same, and nothing changes. The body is optional;
// captchaKey, which the VK gateway takes, is ignored. No MAX call is made.
func (s *server) Resume() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		var body ResumeBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}

		maxUserID := s.maxUserIDOf(txtid)
		if err := s.resumeAccount(txtid, maxUserID); err != nil {
			log.Error().Err(err).Str("userID", txtid).Msg("Failed to lift the account pause")
			s.respondError(w, r, http.StatusInternalServerError, ErrInternalFailure, "database error, the account stays paused")
			return
		}

		s.Respond(w, r, http.StatusOK, map[string]interface{}{
			"success":   true,
			"maxUserID": maxUserID,
		})
	}
}

// maxUserIDOf returns the MAX account the user is logged into: the live
// session's, else the one stored at the last login (users.max_user_id), 0
// when none is known. Pauses are kept by it.
func (s *server) maxUserIDOf(userID string) int64 {
	if client := clientManager.GetMaxClient(userID); client != nil && client.MaxUserID != 0 {
		return client.MaxUserID
	}
	var maxUserID sql.NullInt64
	_ = s.db.Get(&maxUserID, "SELECT max_user_id FROM users WHERE id=$1", userID)
	return maxUserID.Int64
}

// ========== MESSAGE ENDPOINTS ==========

// lookupNotFoundCode reports whether MAX answered a phone lookup with its
// not.found error. That is a fresh *maxclient.Error, so it is told by its
// server code: errors.Is with ErrUserNotFound compares pointers and misses
// it, which made such numbers answer 503 lookup_failed, and callers looked
// them up again and again.
func lookupNotFoundCode(err error) bool {
	code, fromMAX := maxclient.ServerErrorCode(err)
	return fromMAX && code == maxclient.ServerCodeNotFound
}

// lookupFoundNoUser reports whether a phone lookup failed because MAX has no
// user with the number: MAX answered without a usable contact
// (ErrUserNotFound), or with its not.found error.
func lookupFoundNoUser(err error) bool {
	return errors.Is(err, maxclient.ErrUserNotFound) || lookupNotFoundCode(err)
}

// ========== WEBHOOK ENDPOINTS ==========

// GetWebhook returns current webhook
func (s *server) GetWebhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		webhook := r.Context().Value("userinfo").(Values).Get("Webhook")

		response := map[string]interface{}{
			"success": true,
			"webhook": webhook,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SetWebhook sets webhook URL
func (s *server) SetWebhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		decoder := json.NewDecoder(r.Body)
		var msg WebhookBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		_, err := s.db.Exec("UPDATE users SET webhook=$1 WHERE id=$2", msg.Webhook, txtid)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		v := updateUserInfo(r.Context().Value("userinfo"), "Webhook", msg.Webhook)
		userinfocache.Set(token, v, cache.NoExpiration)

		response := map[string]interface{}{
			"success": true,
			"webhook": msg.Webhook,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// UpdateWebhook is alias for SetWebhook
func (s *server) UpdateWebhook() http.HandlerFunc {
	return s.SetWebhook()
}

// DeleteWebhook removes webhook
func (s *server) DeleteWebhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		_, err := s.db.Exec("UPDATE users SET webhook='' WHERE id=$1", txtid)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		v := updateUserInfo(r.Context().Value("userinfo"), "Webhook", "")
		userinfocache.Set(token, v, cache.NoExpiration)

		response := map[string]interface{}{
			"success": true,
			"message": "Webhook deleted",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== ADMIN ENDPOINTS ==========

// userRow is the shared shape for admin user listings.
type userRow struct {
	ID            string `json:"id" db:"id"`
	Name          string `json:"name" db:"name"`
	Token         string `json:"token" db:"token"`
	MaxUserID     *int64 `json:"maxUserId" db:"max_user_id"`
	Webhook       string `json:"webhook" db:"webhook"`
	Events        string `json:"events" db:"events"`
	Connected     int    `json:"connected" db:"connected"`
	AuthToken     string `json:"-" db:"auth_token"`
	Authenticated bool   `json:"authenticated"`
}

const userRowQuery = "SELECT id, name, token, max_user_id, webhook, events, connected, COALESCE(auth_token, '') as auth_token FROM users"

// ListUsers lists all users in the system.
func (s *server) ListUsers() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var users []userRow
		if err := s.db.Select(&users, userRowQuery+" ORDER BY id"); err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		for i := range users {
			users[i].Authenticated = users[i].AuthToken != ""
		}
		s.Respond(w, r, http.StatusOK, users)
	}
}

// userDetailRow carries every column we might want to project from `users`.
// Secrets (auth_token, s3 keys, proxy password) stay on this struct but never
// leave it raw — GetUserByID masks or omits them before responding.
type userDetailRow struct {
	ID            string  `db:"id"`
	Name          string  `db:"name"`
	Token         string  `db:"token"`
	Webhook       string  `db:"webhook"`
	MaxUserID     *int64  `db:"max_user_id"`
	AuthToken     *string `db:"auth_token"`
	DeviceID      *string `db:"device_id"`
	Connected     int     `db:"connected"`
	Events        string  `db:"events"`
	ProxyURL      *string `db:"proxy_url"`
	History       int     `db:"history"`
	S3Enabled     bool    `db:"s3_enabled"`
	S3Endpoint    *string `db:"s3_endpoint"`
	S3Region      *string `db:"s3_region"`
	S3Bucket      *string `db:"s3_bucket"`
	MediaDelivery *string `db:"media_delivery"`
}

// maskProxyURL hides the password but keeps the rest of the URL visible so
// operators can still confirm which upstream proxy is in use.
func maskProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	p, err := url.Parse(raw)
	if err != nil || p.User == nil {
		return raw
	}
	if _, hasPass := p.User.Password(); !hasPass {
		return raw
	}
	p.User = url.UserPassword(p.User.Username(), "***")
	return p.String()
}

// GetUserByID returns a single user by ID.
func (s *server) GetUserByID() http.HandlerFunc {
	const userDetailQuery = `SELECT id, name, token, webhook, max_user_id,
		COALESCE(auth_token, '') as auth_token, COALESCE(device_id, '') as device_id,
		connected, events, COALESCE(proxy_url, '') as proxy_url,
		COALESCE(history, 0) as history,
		COALESCE(s3_enabled, 0) as s3_enabled,
		COALESCE(s3_endpoint, '') as s3_endpoint,
		COALESCE(s3_region, '') as s3_region,
		COALESCE(s3_bucket, '') as s3_bucket,
		COALESCE(media_delivery, 'base64') as media_delivery
		FROM users`

	return func(w http.ResponseWriter, r *http.Request) {
		userID := mux.Vars(r)["userid"]
		if userID == "" {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "userid is required")
			return
		}

		var row userDetailRow
		if err := s.db.Get(&row, userDetailQuery+" WHERE id=$1", userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.respondError(w, r, http.StatusNotFound, ErrNotFound, "user not found")
				return
			}
			log.Error().Err(err).Str("userID", userID).Msg("Failed to load user")
			s.respondError(w, r, http.StatusInternalServerError, ErrInternalFailure, "database error")
			return
		}

		eventList := []string{}
		for _, e := range strings.Split(row.Events, ",") {
			if e = strings.TrimSpace(e); e != "" {
				eventList = append(eventList, e)
			}
		}

		proxyRaw := ""
		if row.ProxyURL != nil {
			proxyRaw = *row.ProxyURL
		}

		data := map[string]interface{}{
			"id":               row.ID,
			"name":             row.Name,
			"token":            row.Token,
			"webhook":          row.Webhook,
			"max_user_id":      safeInt64(row.MaxUserID),
			"device_id":        safeString(row.DeviceID),
			"connected":        clientManager.IsConnected(row.ID),
			"connected_flag":   row.Connected != 0,
			"auth_configured":  row.AuthToken != nil && *row.AuthToken != "",
			"events":           eventList,
			"proxy_configured": proxyRaw != "",
			"proxy_url":        maskProxyURL(proxyRaw),
			"history":          row.History,
			"s3_enabled":       row.S3Enabled,
			"s3_endpoint":      safeString(row.S3Endpoint),
			"s3_region":        safeString(row.S3Region),
			"s3_bucket":        safeString(row.S3Bucket),
			"media_delivery":   safeString(row.MediaDelivery),
		}
		s.Respond(w, r, http.StatusOK, data)
	}
}

// AddUser creates a new user
func (s *server) AddUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		var msg AddUserBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}
		// Generate unique ID and token
		id := uuid.New().String()
		token := uuid.New().String()

		_, err := s.db.Exec(`INSERT INTO users (id, name, token, webhook, events, connected)
			VALUES ($1, $2, $3, $4, $5, 0)`, id, msg.Name, token, msg.Webhook, msg.Events)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		response := map[string]interface{}{
			"success": true,
			"id":      id,
			"token":   token,
			"name":    msg.Name,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// EditUser updates a user
func (s *server) EditUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		userID := vars["userid"]

		decoder := json.NewDecoder(r.Body)
		var msg EditUserBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}
		_, err := s.db.Exec("UPDATE users SET name=$1, webhook=$2, events=$3 WHERE id=$4",
			msg.Name, msg.Webhook, msg.Events, userID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}
		// Events read the webhook and the subscription from the cached user
		// info only: without this the new values waited for a restart.
		refreshCachedUser(s.tokenForUser(userID), map[string]string{
			"Name":    msg.Name,
			"Webhook": msg.Webhook,
			"Events":  msg.Events,
		})

		response := map[string]interface{}{
			"success": true,
			"message": "User updated",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DeleteUser deletes a user
func (s *server) DeleteUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := mux.Vars(r)["userid"]

		if err := s.removeUser(userID); err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "User deleted",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== HELPER FUNCTIONS ==========

// GetHealth reports liveness/readiness for monitoring. Returns 200 with
// `"status":"ok"` when DB pings; 503 with `"status":"degraded"` otherwise.
// Intentionally unauthenticated so load balancers and orchestrators can probe.
func (s *server) GetHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		dbOk := s.db.Ping() == nil
		status := "ok"
		if !dbOk {
			status = "degraded"
		}

		resp := map[string]interface{}{
			"success":         true,
			"status":          status,
			"version":         version,
			"uptime_seconds":  int64(time.Since(s.startTime).Seconds()),
			"db_ok":           dbOk,
			"rabbitmq_ok":     rabbit != nil && rabbit.enabled.Load(),
			"connected_users": clientManager.CountConnected(),
			"goroutines":      runtime.NumGoroutine(),
			"mem_alloc_mb":    mem.Alloc / 1024 / 1024,
			"timestamp":       time.Now().Unix(),
		}

		code := http.StatusOK
		if !dbOk {
			code = http.StatusServiceUnavailable
			resp["success"] = false
		}
		s.Respond(w, r, code, resp)
	}
}
