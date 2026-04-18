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
	"github.com/vincent-petithory/dataurl"
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
	client, mycli, syncData, err := s.initClient(userID, authToken, row.DeviceID, row.Token, subscribedEvents)
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
	sendEventWithWebHook(mycli, buildSyncPostmap(false, client.MaxUserID, syncData), "")

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
// @Summary Open a MAX session (QR or token-based)
// @Description Kicks off a QR auth session when no auth token is stored, or reconnects with saved credentials. Poll GET /session/qr for the rendered QR code.
// @Tags Session
// @Accept json
// @Produce json
// @Param request body ConnectBody true "Connection options"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /session/connect [post]
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
			s.Respond(w, r, http.StatusOK, map[string]interface{}{
				"success":          true,
				"details":          "Already connected to MAX",
				"events":           eventstring,
				"alreadyConnected": true,
			})
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
// @Summary Get current QR code
// @Description Returns the QR code stored server-side for an in-progress auth session.
// @Tags Session
// @Produce json
// @Success 200 {object} QRCodeResponse
// @Failure 500 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /session/qr [get]
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
// @Summary Disconnect from MAX / cancel QR session
// @Description Closes connection to MAX servers or cancels an in-progress QR auth session.
// @Tags Session
// @Produce json
// @Success 200 {object} MessageResponse
// @Security ApiKeyAuth
// @Router /session/disconnect [post]
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

// Logout logs out from MAX and deletes user
// @Summary Logout from MAX
// @Description Logs out from MAX and deletes the user from the system
// @Tags Session
// @Produce json
// @Success 200 {object} MessageResponse
// @Security ApiKeyAuth
// @Router /session/logout [post]
func (s *server) Logout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		client := clientManager.GetMaxClient(txtid)
		if client != nil && client.IsConnected() {
			client.Logout() // Sends opcode 20, server may send LoggedOut back
		}

		// Clear cache before delete
		userinfocache.Delete(token)

		// Delete user immediately, don't wait for LoggedOut event
		// sendWebhook=false because LoggedOut event will send it (if received)
		s.safeDeleteUser(txtid, false)

		response := map[string]interface{}{
			"success": true,
			"message": "Logged out",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GetStatus returns connection status
// @Summary Get connection status
// @Description Returns connection and authentication status
// @Tags Session
// @Produce json
// @Success 200 {object} StatusResponse
// @Security ApiKeyAuth
// @Router /session/status [get]
func (s *server) GetStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		connected := clientManager.IsConnected(txtid)

		var maxUserID int64
		client := clientManager.GetMaxClient(txtid)
		if client != nil && client.Me != nil {
			maxUserID = client.MaxUserID
		}

		// Check if user has auth_token (authenticated)
		var authToken string
		s.db.QueryRow("SELECT COALESCE(auth_token, '') FROM users WHERE id=$1", txtid).Scan(&authToken)
		authenticated := authToken != ""

		response := map[string]interface{}{
			"success":       true,
			"connected":     connected,
			"authenticated": authenticated,
			"loggedIn":      connected && authenticated,
			"maxUserID":     maxUserID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// RequestSync reconnects and returns fresh sync data
// @Summary Request sync
// @Description Reconnects to MAX server and returns fresh profile, chats, contacts data. Also sends Sync event to webhook
// @Tags Session
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} ErrorResponse "No auth token"
// @Failure 500 {object} ErrorResponse "Sync failed"
// @Security ApiKeyAuth
// @Router /session/sync [post]
func (s *server) RequestSync() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		// Get auth token and device ID from DB
		var authToken, deviceID string
		err := s.db.QueryRow("SELECT auth_token, device_id FROM users WHERE id=$1", txtid).Scan(&authToken, &deviceID)
		if err != nil || authToken == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no auth token found, please authenticate first"))
			return
		}

		// Stop existing client goroutine and disconnect
		clientManager.SendKill(txtid)
		oldClient := clientManager.GetMaxClient(txtid)
		if oldClient != nil {
			oldClient.Disconnect()
		}
		// Small delay to let old goroutine clean up
		time.Sleep(100 * time.Millisecond)

		// Create new client and connect
		logger := log.With().Str("userID", txtid).Logger()
		client := maxclient.NewClient(deviceID, logger)

		syncData, err := client.ConnectAndLogin(authToken, nil)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("sync failed: %v", err))
			return
		}

		// Update client manager
		clientManager.SetMaxClient(txtid, client)

		// Update MyClient wrapper
		mycli := clientManager.GetMyClient(txtid)
		if mycli != nil {
			mycli.MaxClient = client
		} else {
			// Create new MyClient if not exists
			mycli = &MyClient{
				MaxClient:     client,
				userID:        txtid,
				token:         token,
				subscriptions: []string{},
				db:            s.db,
				s:             s,
			}
			clientManager.SetMyClient(txtid, mycli)
		}

		// Set event handler
		client.SetEventHandler(func(event maxclient.Event) {
			mycli.handleEvent(event)
		})

		// Update DB
		_, err = s.db.Exec("UPDATE users SET connected=1, max_user_id=$1 WHERE id=$2", client.MaxUserID, txtid)
		if err != nil {
			log.Error().Err(err).Msg("Failed to update connected status")
		}

		// Create new kill channel and start background goroutine for reconnects
		clientManager.NewKillChannel(txtid)
		go s.maintainConnection(txtid, authToken, deviceID, token, mycli)

		// Build response with raw sync data
		response := map[string]interface{}{
			"success":   true,
			"maxUserID": client.MaxUserID,
		}
		for key, value := range syncData {
			response[key] = value
		}

		// Send Sync event to webhook
		postmap := map[string]interface{}{
			"type":      "Sync",
			"reconnect": false,
			"manual":    true,
			"maxUserID": client.MaxUserID,
		}
		for key, value := range syncData {
			if key != "type" {
				postmap[key] = value
			}
		}
		sendEventWithWebHook(mycli, postmap, "")

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== MESSAGE ENDPOINTS ==========

// SendMessage sends a text message
// @Summary Send text message
// @Description Sends a text message to a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body MessageBody true "Message data"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse "Not connected"
// @Security ApiKeyAuth
// @Router /chat/send/text [post]
func (s *server) SendMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg MessageBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		// Get chat ID (chatId=0 is valid for "Favorites/Saved Messages")
		chatID := msg.ChatID

		// If phone provided and no explicit chatId, search by phone
		if msg.Phone != "" && chatID == 0 {
			user, err := client.SearchByPhone(msg.Phone)
			if err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("user not found: %v", err))
				return
			}
			chatID = maxclient.GetDialogID(client.MaxUserID, user.ID)
		}

		result, err := client.SendMessage(maxclient.SendMessageOptions{
			ChatID:  chatID,
			Text:    msg.Text,
			ReplyTo: msg.ReplyTo,
			Notify:  msg.Notify,
		})

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("send failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":   true,
			"messageId": result.ID,
			"chatId":    chatID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SendEditMessage edits an existing message
// @Summary Edit message
// @Description Edits an existing message
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body EditMessageBody true "Edit data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/send/edit [post]
func (s *server) SendEditMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg EditMessageBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if msg.ChatID == 0 || msg.MessageID == 0 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("chatId and messageId are required"))
			return
		}

		_, err := client.EditMessage(msg.ChatID, msg.MessageID, msg.Text, nil)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("edit failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Message edited",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// MarkRead marks messages as read
// @Summary Mark messages as read
// @Description Marks messages as read in a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body MarkReadBody true "Mark read data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/markread [post]
func (s *server) MarkRead() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg MarkReadBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if msg.ChatID == 0 || msg.MessageID == 0 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("chatId and messageId are required"))
			return
		}

		err := client.MarkRead(msg.ChatID, msg.MessageID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("mark read failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Marked as read",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DeleteMessage deletes messages
// @Summary Delete messages
// @Description Deletes messages from a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DeleteMessageBody true "Delete data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/delete [post]
func (s *server) DeleteMessage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg DeleteMessageBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		err := client.DeleteMessage(msg.ChatID, msg.MessageIDs, msg.ForMe)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("delete failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Messages deleted",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== MEDIA ENDPOINTS ==========

// SendImage sends an image message
// @Summary Send image
// @Description Sends an image message to a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body ImageBody true "Image data"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/send/image [post]
func (s *server) SendImage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg ImageBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chatID := msg.ChatID
		if msg.Phone != "" && chatID == 0 {
			user, err := client.SearchByPhone(msg.Phone)
			if err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("user not found: %v", err))
				return
			}
			chatID = maxclient.GetDialogID(client.MaxUserID, user.ID)
		}

		// Decode image
		imageData, filename, err := decodeMediaData(msg.Image, "image.jpg")
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid image data: %v", err))
			return
		}

		result, err := client.SendMessageWithPhoto(chatID, msg.Caption, imageData, filename, msg.Notify)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("send failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":   true,
			"messageId": result.ID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SendDocument sends a document
// @Summary Send document
// @Description Sends a document to a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DocumentBody true "Document data"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/send/document [post]
func (s *server) SendDocument() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg DocumentBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chatID := msg.ChatID
		if msg.Phone != "" && chatID == 0 {
			user, err := client.SearchByPhone(msg.Phone)
			if err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("user not found: %v", err))
				return
			}
			chatID = maxclient.GetDialogID(client.MaxUserID, user.ID)
		}

		filename := msg.FileName
		if filename == "" {
			filename = "document"
		}

		docData, _, err := decodeMediaData(msg.Document, filename)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid document data: %v", err))
			return
		}

		result, err := client.SendMessageWithFile(chatID, msg.Caption, docData, filename, msg.Notify)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("send failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":   true,
			"messageId": result.ID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SendAudio sends an audio file
// @Summary Send audio
// @Description Sends an audio file to a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body AudioBody true "Audio data"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/send/audio [post]
func (s *server) SendAudio() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg AudioBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chatID := msg.ChatID
		if msg.Phone != "" && chatID == 0 {
			user, err := client.SearchByPhone(msg.Phone)
			if err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("user not found: %v", err))
				return
			}
			chatID = maxclient.GetDialogID(client.MaxUserID, user.ID)
		}

		filename := msg.FileName
		if filename == "" {
			filename = "audio.mp3"
		}

		audioData, _, err := decodeMediaData(msg.Audio, filename)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid audio data: %v", err))
			return
		}

		result, err := client.SendMessageWithFile(chatID, "", audioData, filename, msg.Notify)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("send failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":   true,
			"messageId": result.ID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SendVideo sends a video
// @Summary Send video
// @Description Sends a video to a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body VideoBody true "Video data"
// @Success 200 {object} SendMessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/send/video [post]
func (s *server) SendVideo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg VideoBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chatID := msg.ChatID
		if msg.Phone != "" && chatID == 0 {
			user, err := client.SearchByPhone(msg.Phone)
			if err != nil {
				s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("user not found: %v", err))
				return
			}
			chatID = maxclient.GetDialogID(client.MaxUserID, user.ID)
		}

		filename := msg.FileName
		if filename == "" {
			filename = "video.mp4"
		}

		videoData, _, err := decodeMediaData(msg.Video, filename)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("invalid video data: %v", err))
			return
		}

		result, err := client.SendMessageWithVideo(chatID, msg.Caption, videoData, filename, msg.Notify)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("send failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":   true,
			"messageId": result.ID,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DownloadImage downloads an image
// @Summary Download image
// @Description Downloads an image from URL
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DownloadBody true "URL"
// @Success 200 {object} DownloadMediaResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/downloadimage [post]
func (s *server) DownloadImage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		decoder := json.NewDecoder(r.Body)
		var msg DownloadBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if msg.URL == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("url is required"))
			return
		}

		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(txtid)
			defer userSemaphoreManager.Release(txtid)
		}

		data, err := downloadMedia(msg.URL)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("download failed: %v", err))
			return
		}

		mimeType := http.DetectContentType(data)

		response := map[string]interface{}{
			"success":  true,
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": mimeType,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DownloadDocument downloads a document by fileId
// @Summary Download document
// @Description Downloads a document by file ID
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DownloadFileBody true "File info"
// @Success 200 {object} DownloadMediaResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/downloaddocument [post]
func (s *server) DownloadDocument() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg DownloadFileBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(txtid)
			defer userSemaphoreManager.Release(txtid)
		}

		fileInfo, err := client.GetFileDownloadURL(msg.ChatID, msg.MessageID, msg.FileID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("get download url failed: %v", err))
			return
		}

		data, err := client.DownloadFile(fileInfo.URL)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("download failed: %v", err))
			return
		}

		mimeType := http.DetectContentType(data)

		response := map[string]interface{}{
			"success":  true,
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": mimeType,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DownloadVideo downloads a video by videoId
// @Summary Download video
// @Description Downloads a video by video ID
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DownloadFileBody true "Video info"
// @Success 200 {object} DownloadVideoResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/downloadvideo [post]
func (s *server) DownloadVideo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg DownloadFileBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(txtid)
			defer userSemaphoreManager.Release(txtid)
		}

		videoInfo, err := client.GetVideoDownloadURL(msg.ChatID, msg.MessageID, msg.VideoID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("get download url failed: %v", err))
			return
		}

		data, err := client.DownloadFile(videoInfo.URL)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("download failed: %v", err))
			return
		}

		mimeType := http.DetectContentType(data)

		response := map[string]interface{}{
			"success":  true,
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": mimeType,
			"url":      videoInfo.URL,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DownloadAudio downloads audio by fileId (same as document)
// @Summary Download audio
// @Description Downloads audio by file ID
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body DownloadBody true "URL"
// @Success 200 {object} DownloadMediaResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/downloadaudio [post]
func (s *server) DownloadAudio() http.HandlerFunc {
	return s.DownloadImage()
}

// ========== USER ENDPOINTS ==========

// CheckUser checks if a phone number exists in MAX
// @Summary Check user existence
// @Description Checks if phone numbers exist in MAX
// @Tags User
// @Accept json
// @Produce json
// @Param request body CheckUserBody true "Phone numbers"
// @Success 200 {object} CheckUserResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /user/check [post]
func (s *server) CheckUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg CheckUserBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		results := make([]map[string]interface{}, 0)

		for _, phone := range msg.Phone {
			user, err := client.SearchByPhone(phone)
			result := map[string]interface{}{
				"phone":     phone,
				"exists":    false,
				"maxUserId": int64(0),
			}
			if err == nil && user != nil {
				result["exists"] = true
				result["maxUserId"] = user.ID
				if len(user.Names) > 0 {
					result["name"] = user.Names[0].Name
				}
			}
			results = append(results, result)
		}

		response := map[string]interface{}{
			"success": true,
			"users":   results,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GetContacts returns all contacts
// @Summary Get contacts
// @Description Returns all contacts from MAX
// @Tags User
// @Produce json
// @Success 200 {object} ContactsResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /user/contacts [get]
func (s *server) GetContacts() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		// Direct request to MAX without caching
		contacts, err := client.GetContacts()
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to get contacts: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":  true,
			"contacts": contacts,
			"count":    len(contacts),
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GetUser gets user info by ID or multiple IDs
// @Summary Get user info
// @Description Gets user information by MAX user ID. Supports single userId or batch request with userIds array (max 100)
// @Tags User
// @Accept json
// @Produce json
// @Param request body UserInfoBody true "User ID or IDs array"
// @Success 200 {object} UserInfoResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /user/info [post]
func (s *server) GetUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg UserInfoBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if len(msg.UserIDs) == 0 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("userIds is required"))
			return
		}

		users, err := client.GetUsers(msg.UserIDs)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("failed to get users: %v", err))
			return
		}

		// Convert to response format with avatar URLs
		usersResponse := make([]map[string]interface{}, 0, len(users))
		for _, user := range users {
			usersResponse = append(usersResponse, map[string]interface{}{
				"id":            user.ID,
				"accountStatus": user.AccountStatus,
				"names":         user.Names,
				"options":       user.Options,
				"baseUrl":       user.BaseURL,
				"baseRawUrl":    user.BaseRawURL,
				"photoId":       user.PhotoID,
				"description":   user.Description,
				"gender":        user.Gender,
				"link":          user.Link,
				"updateTime":    user.UpdateTime,
				"webApp":        user.WebApp,
				"avatarUrl":     maxclient.GetUserAvatarURL(&user),
			})
		}

		response := map[string]interface{}{
			"success": true,
			"users":   usersResponse,
			"count":   len(usersResponse),
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SendPresence sets presence status
// @Summary Send presence
// @Description Sends typing indicator to a chat
// @Tags User
// @Accept json
// @Produce json
// @Param request body PresenceBody true "Chat ID"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /user/presence [post]
func (s *server) SendPresence() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg PresenceBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		// Send typing indicator
		err := client.SendTyping(msg.ChatID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("presence failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Presence sent",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== GROUP ENDPOINTS ==========

// CreateGroup creates a new group
// @Summary Create group
// @Description Creates a new group with specified participants
// @Tags Group
// @Accept json
// @Produce json
// @Param request body CreateGroupBody true "Group data"
// @Success 200 {object} GroupChatResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/create [post]
func (s *server) CreateGroup() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg CreateGroupBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chat, _, err := client.CreateGroup(msg.Name, msg.Participants, true)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("create group failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"chat":    chat,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GetGroupInfo gets group info
// @Summary Get group info
// @Description Gets group information by chat ID
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupInfoBody true "Chat ID"
// @Success 200 {object} GroupChatResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/info [post]
func (s *server) GetGroupInfo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupInfoBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chat, err := client.GetChat(msg.ChatID)
		if err != nil {
			s.Respond(w, r, http.StatusNotFound, fmt.Errorf("chat not found: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"chat":    chat,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GetGroupInviteLink gets group invite link
// @Summary Get group invite link
// @Description Gets invite link for a group
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupInfoBody true "Chat ID"
// @Success 200 {object} InviteLinkResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/invitelink [post]
func (s *server) GetGroupInviteLink() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupInfoBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chat, err := client.GetChat(msg.ChatID)
		if err != nil {
			s.Respond(w, r, http.StatusNotFound, fmt.Errorf("chat not found: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":    true,
			"inviteLink": chat.Link,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GroupJoin joins a group via invite link
// @Summary Join group
// @Description Joins a group via invite link
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupJoinBody true "Invite link"
// @Success 200 {object} GroupChatResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/join [post]
func (s *server) GroupJoin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupJoinBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		chat, err := client.JoinGroup(msg.Link)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("join failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"chat":    chat,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// GroupLeave leaves a group
// @Summary Leave group
// @Description Leaves a group
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupInfoBody true "Chat ID"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/leave [post]
func (s *server) GroupLeave() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupInfoBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		err := client.LeaveChat(msg.ChatID)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("leave failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Left group",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// UpdateGroupParticipants adds or removes group members
// @Summary Update group participants
// @Description Adds or removes participants from a group
// @Tags Group
// @Accept json
// @Produce json
// @Param request body UpdateParticipantsBody true "Participants data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/updateparticipants [post]
func (s *server) UpdateGroupParticipants() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg UpdateParticipantsBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		var err error
		if msg.Operation == "add" {
			_, err = client.AddGroupMembers(msg.ChatID, msg.UserIDs, true)
		} else {
			_, err = client.RemoveGroupMembers(msg.ChatID, msg.UserIDs, 0)
		}

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("update failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Participants updated",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SetGroupName sets group name
// @Summary Set group name
// @Description Sets the name of a group
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupNameBody true "Group name"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/name [post]
func (s *server) SetGroupName() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupNameBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		_, err := client.UpdateChatProfile(msg.ChatID, msg.Name, "")
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("update failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Group name updated",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// SetGroupTopic sets group description
// @Summary Set group topic
// @Description Sets the topic/description of a group
// @Tags Group
// @Accept json
// @Produce json
// @Param request body GroupTopicBody true "Group topic"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /group/topic [post]
func (s *server) SetGroupTopic() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg GroupTopicBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		_, err := client.UpdateChatProfile(msg.ChatID, "", msg.Topic)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("update failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Group topic updated",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== WEBHOOK ENDPOINTS ==========

// GetWebhook returns current webhook
// @Summary Get webhook
// @Description Returns current webhook URL
// @Tags Webhook
// @Produce json
// @Success 200 {object} WebhookResponse
// @Security ApiKeyAuth
// @Router /webhook [get]
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
// @Summary Set webhook
// @Description Sets webhook URL for receiving events
// @Tags Webhook
// @Accept json
// @Produce json
// @Param request body WebhookBody true "Webhook URL"
// @Success 200 {object} WebhookResponse
// @Failure 400 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /webhook [post]
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
// @Summary Update webhook
// @Description Updates webhook URL
// @Tags Webhook
// @Accept json
// @Produce json
// @Param request body WebhookBody true "Webhook URL"
// @Success 200 {object} WebhookResponse
// @Failure 400 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /webhook [put]
func (s *server) UpdateWebhook() http.HandlerFunc {
	return s.SetWebhook()
}

// DeleteWebhook removes webhook
// @Summary Delete webhook
// @Description Removes the webhook URL
// @Tags Webhook
// @Produce json
// @Success 200 {object} MessageResponse
// @Security ApiKeyAuth
// @Router /webhook [delete]
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

// ========== CHAT HISTORY ENDPOINTS ==========

// GetChatHistory gets chat history
// @Summary Get chat history
// @Description Gets message history for a chat
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body ChatHistoryBody true "History parameters"
// @Success 200 {object} ChatHistoryResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/history [post]
func (s *server) GetChatHistory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg ChatHistoryBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		count := msg.Count
		if count == 0 {
			count = 50
		}

		messages, err := client.GetChatHistory(msg.ChatID, msg.FromTime, 0, count)
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("get history failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success":  true,
			"messages": messages,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== REACTIONS ==========

// React adds reaction to message
// @Summary Add reaction
// @Description Adds or removes a reaction to a message
// @Tags Chat
// @Accept json
// @Produce json
// @Param request body ReactBody true "Reaction data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /chat/react [post]
func (s *server) React() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		client := clientManager.GetMaxClient(txtid)
		if client == nil || !client.IsConnected() {
			s.Respond(w, r, http.StatusServiceUnavailable, errors.New("not connected"))
			return
		}

		decoder := json.NewDecoder(r.Body)
		var msg ReactBody
		if err := decoder.Decode(&msg); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		var err error
		if msg.Reaction == "" {
			_, err = client.RemoveReaction(msg.ChatID, msg.MessageID)
		} else {
			_, err = client.AddReaction(msg.ChatID, msg.MessageID, msg.Reaction)
		}

		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("react failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Reaction updated",
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
// @Summary List all users
// @Description Returns every user registered in the system.
// @Tags Admin
// @Produce json
// @Success 200 {object} ListUsersResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users [get]
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
// @Summary Get user by ID
// @Description Returns a single user by their ID. Secrets (auth_token, s3 keys,
// @Description proxy password) are never returned raw — see UserDetail.
// @Tags Admin
// @Produce json
// @Param userid path string true "User ID"
// @Success 200 {object} UserDetailResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users/{userid} [get]
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
// @Summary Create user
// @Description Creates a new user in the system
// @Tags Admin
// @Accept json
// @Produce json
// @Param request body AddUserBody true "User data"
// @Success 200 {object} AddUserResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users [post]
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
// @Summary Update user
// @Description Updates an existing user
// @Tags Admin
// @Accept json
// @Produce json
// @Param userid path string true "User ID"
// @Param request body EditUserBody true "User data"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users/{userid} [put]
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

		response := map[string]interface{}{
			"success": true,
			"message": "User updated",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// DeleteUser deletes a user
// @Summary Delete user
// @Description Deletes a user from the system
// @Tags Admin
// @Produce json
// @Param userid path string true "User ID"
// @Success 200 {object} MessageResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users/{userid} [delete]
func (s *server) DeleteUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		userID := vars["userid"]

		// Disconnect if connected (non-blocking send)
		if !clientManager.SendKill(userID) {
			clientManager.DeleteKillChannel(userID)
		}

		_, err := s.db.Exec("DELETE FROM users WHERE id=$1", userID)
		if err != nil {
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

func decodeMediaData(data string, defaultName string) ([]byte, string, error) {
	filename := defaultName

	// Check if it's a data URL
	if strings.HasPrefix(data, "data:") {
		dataURL, err := dataurl.DecodeString(data)
		if err != nil {
			return nil, "", err
		}
		return dataURL.Data, filename, nil
	}

	// Check if it's a URL
	if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
		resp, err := http.Get(data)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()

		fileData, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", err
		}
		return fileData, filename, nil
	}

	// Assume it's base64
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, "", err
	}
	return decoded, filename, nil
}

// GetHealth reports liveness/readiness for monitoring. Returns 200 with
// `"status":"ok"` when DB pings; 503 with `"status":"degraded"` otherwise.
// Intentionally unauthenticated so load balancers and orchestrators can probe.
// @Summary Health check
// @Description Liveness + readiness snapshot. Returns 200 when DB pings, 503 otherwise.
// @Tags Health
// @Produce json
// @Success 200 {object} HealthResponse
// @Failure 503 {object} HealthResponse
// @Router /health [get]
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
