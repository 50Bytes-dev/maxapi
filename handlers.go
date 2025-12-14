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
	"strings"
	"sync"
	"time"

	"maxapi/maxclient"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog/log"
	"github.com/vincent-petithory/dataurl"
)

// authTimeouts stores timers for auto-closing auth sessions after 5 minutes
var authTimeouts = make(map[string]*time.Timer)
var authTimeoutsMu sync.Mutex

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

// ========== AUTH ENDPOINTS ==========

// AuthRequest handles SMS code request
// @Summary Request SMS verification code
// @Description Sends an SMS verification code to the specified phone number
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body AuthRequestBody true "Phone number and language"
// @Success 200 {object} AuthRequestResponse
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /session/auth/request [post]
func (s *server) AuthRequest() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		decoder := json.NewDecoder(r.Body)
		var body AuthRequestBody
		if err := decoder.Decode(&body); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if body.Phone == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("phone number is required"))
			return
		}

		// Create device ID if not exists
		deviceID := uuid.New().String()

		// Create temporary MAX client for auth
		logger := log.With().Str("userID", txtid).Logger()
		client := maxclient.NewClient(deviceID, logger)

		if err := client.Connect(); err != nil {
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("connection failed: %v", err))
			return
		}

		if err := client.SessionInit(nil); err != nil {
			client.Close()
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("session init failed: %v", err))
			return
		}

		tempToken, err := client.RequestAuthCode(body.Phone, body.Language)
		if err != nil {
			client.Close()
			s.Respond(w, r, http.StatusInternalServerError, fmt.Errorf("auth request failed: %v", err))
			return
		}

		// Store temp token and device ID
		_, err = s.db.Exec("UPDATE users SET temp_token=$1, device_id=$2 WHERE id=$3", tempToken, deviceID, txtid)
		if err != nil {
			log.Error().Err(err).Msg("Failed to store temp token")
		}

		// Store client temporarily for auth flow
		clientManager.SetMaxClient(txtid, client)

		// Start ping loop to keep connection alive during auth flow
		client.StartPingLoop()

		// Set 5-minute timeout to auto-close auth session
		authTimeoutsMu.Lock()
		if oldTimer := authTimeouts[txtid]; oldTimer != nil {
			oldTimer.Stop()
		}
		authTimeouts[txtid] = time.AfterFunc(5*time.Minute, func() {
			log.Info().Str("userID", txtid).Msg("Auth session timed out after 5 minutes")
			if c := clientManager.GetMaxClient(txtid); c != nil {
				c.Close()
				clientManager.DeleteMaxClient(txtid)
			}
			authTimeoutsMu.Lock()
			delete(authTimeouts, txtid)
			authTimeoutsMu.Unlock()
		})
		authTimeoutsMu.Unlock()

		// Send webhook event
		if mycli := clientManager.GetMyClient(txtid); mycli != nil {
			postmap := map[string]interface{}{
				"type":  "AuthCodeSent",
				"phone": body.Phone,
			}
			sendEventWithWebHook(mycli, postmap, "")
		}

		response := map[string]interface{}{
			"success":   true,
			"message":   "Verification code sent",
			"tempToken": tempToken,
		}

		// Update cache
		v := updateUserInfo(r.Context().Value("userinfo"), "TempToken", tempToken)
		userinfocache.Set(token, v, cache.NoExpiration)

		s.Respond(w, r, http.StatusOK, response)
	}
}

// AuthConfirm handles SMS code verification
// @Summary Confirm SMS verification code
// @Description Verifies the SMS code and returns auth token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body AuthConfirmBody true "SMS code"
// @Success 200 {object} AuthConfirmResponse
// @Failure 400 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /session/auth/confirm [post]
func (s *server) AuthConfirm() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		// Cancel auth timeout
		authTimeoutsMu.Lock()
		if timer := authTimeouts[txtid]; timer != nil {
			timer.Stop()
			delete(authTimeouts, txtid)
		}
		authTimeoutsMu.Unlock()

		decoder := json.NewDecoder(r.Body)
		var body AuthConfirmBody
		if err := decoder.Decode(&body); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if body.Code == "" || len(body.Code) != 6 {
			s.Respond(w, r, http.StatusBadRequest, errors.New("valid 6-digit code is required"))
			return
		}

		// Get temp token from DB
		var tempToken string
		if err := s.db.Get(&tempToken, "SELECT temp_token FROM users WHERE id=$1", txtid); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no pending auth request"))
			return
		}

		client := clientManager.GetMaxClient(txtid)
		if client == nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no active auth session"))
			return
		}

		authToken, registerToken, err := client.SubmitAuthCode(body.Code, tempToken)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("code verification failed: %v", err))
			return
		}

		response := map[string]interface{}{
			"success": true,
		}

		if authToken != "" {
			// Existing user - save auth token
			_, err = s.db.Exec("UPDATE users SET auth_token=$1, temp_token='' WHERE id=$2", authToken, txtid)
			if err != nil {
				log.Error().Err(err).Msg("Failed to save auth token")
			}

			// Close the temporary auth client so /session/connect can create a proper one
			client.Close()
			clientManager.DeleteMaxClient(txtid)

			response["message"] = "Login successful"
			response["authToken"] = authToken
			response["requiresRegistration"] = false

			v := updateUserInfo(r.Context().Value("userinfo"), "AuthToken", authToken)
			userinfocache.Set(token, v, cache.NoExpiration)
		} else if registerToken != "" {
			// New user - needs registration (keep client open for registration)
			_, err = s.db.Exec("UPDATE users SET temp_token=$1 WHERE id=$2", registerToken, txtid)
			if err != nil {
				log.Error().Err(err).Msg("Failed to save register token")
			}

			response["message"] = "Registration required"
			response["registerToken"] = registerToken
			response["requiresRegistration"] = true
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// AuthRegister handles new user registration
// @Summary Register new user
// @Description Registers a new user with first and last name
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body AuthRegisterBody true "User registration data"
// @Success 200 {object} AuthRegisterResponse
// @Failure 400 {object} ErrorResponse
// @Security ApiKeyAuth
// @Router /session/auth/register [post]
func (s *server) AuthRegister() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")
		token := r.Context().Value("userinfo").(Values).Get("Token")

		// Cancel auth timeout
		authTimeoutsMu.Lock()
		if timer := authTimeouts[txtid]; timer != nil {
			timer.Stop()
			delete(authTimeouts, txtid)
		}
		authTimeoutsMu.Unlock()

		decoder := json.NewDecoder(r.Body)
		var body AuthRegisterBody
		if err := decoder.Decode(&body); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("could not decode payload"))
			return
		}

		if body.FirstName == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("firstName is required"))
			return
		}

		// Get register token from DB
		var registerToken string
		if err := s.db.Get(&registerToken, "SELECT temp_token FROM users WHERE id=$1", txtid); err != nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no pending registration"))
			return
		}

		client := clientManager.GetMaxClient(txtid)
		if client == nil {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no active auth session"))
			return
		}

		authToken, err := client.Register(body.FirstName, body.LastName, registerToken)
		if err != nil {
			s.Respond(w, r, http.StatusBadRequest, fmt.Errorf("registration failed: %v", err))
			return
		}

		// Save auth token
		_, err = s.db.Exec("UPDATE users SET auth_token=$1, temp_token='' WHERE id=$2", authToken, txtid)
		if err != nil {
			log.Error().Err(err).Msg("Failed to save auth token")
		}

		// Close the temporary auth client so /session/connect can create a proper one
		client.Close()
		clientManager.DeleteMaxClient(txtid)

		v := updateUserInfo(r.Context().Value("userinfo"), "AuthToken", authToken)
		userinfocache.Set(token, v, cache.NoExpiration)

		response := map[string]interface{}{
			"success":   true,
			"message":   "Registration successful",
			"authToken": authToken,
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// ========== SESSION ENDPOINTS ==========

// Connect connects to MAX with saved auth token
// @Summary Connect to MAX servers
// @Description Initiates connection to MAX servers using saved auth token
// @Tags Session
// @Accept json
// @Produce json
// @Param request body ConnectBody true "Connection options"
// @Success 200 {object} MessageResponse
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse "Already connected"
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

		// Check if already connected
		if clientManager.IsConnected(txtid) {
			s.Respond(w, r, http.StatusConflict, errors.New("already connected"))
			return
		}

		// Get auth token from DB
		var authToken, deviceID string
		err := s.db.QueryRow("SELECT auth_token, device_id FROM users WHERE id=$1", txtid).Scan(&authToken, &deviceID)
		if err != nil || authToken == "" {
			s.Respond(w, r, http.StatusBadRequest, errors.New("no auth token found, please authenticate first"))
			return
		}

		// Process subscriptions
		var subscribedEvents []string
		for _, arg := range t.Subscribe {
			if Find(supportedEventTypes, arg) && !Find(subscribedEvents, arg) {
				subscribedEvents = append(subscribedEvents, arg)
			}
		}

		eventstring := strings.Join(subscribedEvents, ",")
		_, err = s.db.Exec("UPDATE users SET events=$1 WHERE id=$2", eventstring, txtid)
		if err != nil {
			log.Warn().Err(err).Msg("Could not set events in users table")
		}

		v := updateUserInfo(r.Context().Value("userinfo"), "Events", eventstring)
		userinfocache.Set(token, v, cache.NoExpiration)

		log.Info().Str("userID", txtid).Msg("Connecting to MAX")
		killchannel[txtid] = make(chan bool)
		go s.startClient(txtid, authToken, deviceID, token, subscribedEvents)

		if !t.Immediate {
			time.Sleep(5 * time.Second)

			if !clientManager.IsConnected(txtid) {
				s.Respond(w, r, http.StatusInternalServerError, errors.New("failed to connect"))
				return
			}
		}

		response := map[string]interface{}{
			"success": true,
			"message": "Connected to MAX",
		}

		s.Respond(w, r, http.StatusOK, response)
	}
}

// Disconnect disconnects from MAX
// @Summary Disconnect from MAX servers
// @Description Closes connection to MAX servers
// @Tags Session
// @Produce json
// @Success 200 {object} MessageResponse
// @Security ApiKeyAuth
// @Router /session/disconnect [post]
func (s *server) Disconnect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		if ch := killchannel[txtid]; ch != nil {
			select {
			case ch <- true:
				// Signal sent successfully
			default:
				// Channel not ready, clean up anyway
				delete(killchannel, txtid)
			}
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
		if ch := killchannel[txtid]; ch != nil {
			select {
			case ch <- true:
			default:
			}
		}
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
		killchannel[txtid] = make(chan bool)
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
				"id":          user.ID,
				"names":       user.Names,
				"avatarUrl":   maxclient.GetUserAvatarURL(&user),
				"description": user.Description,
				"photoId":     user.PhotoID,
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

// ListUsers lists all users
// @Summary List all users
// @Description Returns a list of all users in the system
// @Tags Admin
// @Produce json
// @Success 200 {object} ListUsersResponse
// @Failure 500 {object} ErrorResponse
// @Security AdminAuth
// @Router /admin/users [get]
func (s *server) ListUsers() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type UserRow struct {
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

		var users []UserRow
		err := s.db.Select(&users, "SELECT id, name, token, max_user_id, webhook, events, connected, COALESCE(auth_token, '') as auth_token FROM users ORDER BY id")
		if err != nil {
			s.Respond(w, r, http.StatusInternalServerError, err)
			return
		}

		// Set authenticated based on auth_token
		for i := range users {
			users[i].Authenticated = users[i].AuthToken != ""
		}

		s.Respond(w, r, http.StatusOK, users)
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
		if ch := killchannel[userID]; ch != nil {
			select {
			case ch <- true:
				// Signal sent successfully
			default:
				// Channel not ready, clean up anyway
				delete(killchannel, userID)
			}
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
