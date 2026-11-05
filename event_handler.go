package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"maxapi/maxclient"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog/log"
)

// MyClient wraps the MAX client with additional metadata
type MyClient struct {
	MaxClient     *maxclient.Client
	userID        string
	token         string
	subscriptions []string
	db            *sqlx.DB
	s             *server
}

// sendToGlobalWebHook sends event data to the global webhook through the
// user's HTTP client (see callHook)
func sendToGlobalWebHook(httpClient *resty.Client, jsonData []byte, token string, userID string) {
	jsonDataStr := string(jsonData)

	instanceName := ""
	userinfo, found := userinfocache.Get(token)
	if found {
		instanceName = userinfo.(Values).Get("Name")
	}

	if *globalWebhook != "" {
		log.Info().Str("url", redactURL(*globalWebhook)).Msg("Calling global webhook")
		globalData := map[string]string{
			"jsonData":     jsonDataStr,
			"token":        token,
			"userID":       userID,
			"instanceName": instanceName,
		}
		callHook(httpClient, *globalWebhook, globalData, userID)
	}
}

// sendToUserWebHook sends event data to the user's webhook through the user's
// HTTP client (see callHook)
func sendToUserWebHook(httpClient *resty.Client, webhookurl string, path string, jsonData []byte, userID string, token string) {
	instanceName := ""
	userinfo, found := userinfocache.Get(token)
	if found {
		instanceName = userinfo.(Values).Get("Name")
	}
	data := map[string]string{
		"jsonData":     string(jsonData),
		"token":        token,
		"instanceName": instanceName,
	}

	// Neither the API token nor the event goes to the log: QRAuthorized
	// carries the MAX auth token, Sync the account's chats and contacts.
	log.Debug().Str("userID", userID).Str("instanceName", instanceName).Int("bytes", len(jsonData)).
		Msg("Data being sent to webhook")

	if webhookurl != "" {
		log.Info().Str("url", redactURL(webhookurl)).Msg("Calling user webhook")
		if path == "" {
			go callHook(httpClient, webhookurl, data, userID)
		} else {
			errChan := make(chan error, 1)
			go func() {
				err := callHookFile(webhookurl, data, userID, path)
				errChan <- err
			}()

			if err := <-errChan; err != nil {
				log.Error().Err(err).Msg("Error calling hook file")
			}
		}
	} else {
		log.Warn().Str("userid", userID).Msg("No webhook set for user")
	}
}

// updateAndGetUserSubscriptions updates and returns user subscriptions
func updateAndGetUserSubscriptions(mycli *MyClient) ([]string, error) {
	currentEvents := ""
	userinfo, found := userinfocache.Get(mycli.token)
	if found {
		currentEvents = userinfo.(Values).Get("Events")
	} else {
		if err := mycli.db.Get(&currentEvents, "SELECT events FROM users WHERE id=$1", mycli.userID); err != nil {
			log.Warn().Err(err).Str("userID", mycli.userID).Msg("Could not get events from DB")
			return nil, err
		}
	}

	eventarray := strings.Split(currentEvents, ",")
	var subscribedEvents []string
	if len(eventarray) == 1 && eventarray[0] == "" {
		subscribedEvents = []string{}
	} else {
		for _, arg := range eventarray {
			arg = strings.TrimSpace(arg)
			if arg != "" && Find(supportedEventTypes, arg) {
				subscribedEvents = append(subscribedEvents, arg)
			}
		}
	}

	mycli.subscriptions = subscribedEvents
	return subscribedEvents, nil
}

// getUserWebhookUrl returns the webhook URL of the user: the cached one,
// else the stored one. An account event (Paused, Resumed) also goes to users
// that have no session and so are not cached.
func getUserWebhookUrl(mycli *MyClient) string {
	if myuserinfo, found := userinfocache.Get(mycli.token); found {
		return myuserinfo.(Values).Get("Webhook")
	}
	var webhookurl string
	if mycli.db != nil {
		if err := mycli.db.Get(&webhookurl, "SELECT webhook FROM users WHERE id=$1", mycli.userID); err != nil {
			log.Warn().Err(err).Str("userID", mycli.userID).Msg("Could not read the user's webhook")
		}
	}
	return webhookurl
}

// sendEventWithWebHook sends an event through webhook
func sendEventWithWebHook(mycli *MyClient, postmap map[string]interface{}, path string) {
	webhookurl := getUserWebhookUrl(mycli)

	subscribedEvents, err := updateAndGetUserSubscriptions(mycli)
	if err != nil {
		return
	}

	eventType, ok := postmap["type"].(string)
	if !ok {
		log.Error().Msg("Event type is not a string in postmap")
		return
	}

	log.Debug().
		Str("userID", mycli.userID).
		Str("eventType", eventType).
		Strs("subscribedEvents", subscribedEvents).
		Msg("Checking event subscription")

	if !checkIfSubscribedToEvent(subscribedEvents, eventType, mycli.userID) {
		return
	}

	jsonData, err := json.Marshal(postmap)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal postmap to JSON")
		return
	}

	// Taken now, not in the delivery goroutines: AuthExpired and
	// ReconnectExhausted go out right before cleanupClient drops the client.
	// A user without a session (an account event) gets a client of its own.
	httpClient := clientManager.GetHTTPClient(mycli.userID)
	if httpClient == nil && mycli.s != nil {
		httpClient = mycli.s.newWebhookClient(mycli.userID)
	}
	sendToUserWebHook(httpClient, webhookurl, path, jsonData, mycli.userID, mycli.token)
	go sendToGlobalWebHook(httpClient, jsonData, mycli.token, mycli.userID)
	go sendToGlobalRabbit(jsonData, mycli.token, mycli.userID)
}

// checkIfSubscribedToEvent checks if user is subscribed to an event type
func checkIfSubscribedToEvent(subscribedEvents []string, eventType string, userId string) bool {
	if !Find(subscribedEvents, eventType) && !Find(subscribedEvents, "All") {
		log.Warn().
			Str("type", eventType).
			Strs("subscribedEvents", subscribedEvents).
			Str("userID", userId).
			Msg("Skipping webhook. Not subscribed for this type")
		return false
	}
	return true
}

// startupLoginInterval is the mean time between two logins at startup
// (MAX_STARTUP_LOGIN_INTERVAL_MS): hundreds of accounts logging in from one
// address in the same second is the kind of burst MAX limits. Each start is
// spread by ±50%. Tests set it to 0.
var startupLoginInterval = time.Duration(envIntDefault("MAX_STARTUP_LOGIN_INTERVAL_MS", 1000)) * time.Millisecond

// startupDelay is when the n-th (0-based) login of the startup begins.
func startupDelay(n int) time.Duration {
	if startupLoginInterval <= 0 {
		return 0
	}
	base := time.Duration(n) * startupLoginInterval
	return base + time.Duration((rand.Float64()-0.5)*float64(startupLoginInterval))
}

// connectOnStartup connects all authenticated users to MAX on server startup,
// one after another, startupLoginInterval apart. A user that got a session
// in the meantime (POST /session/connect) is not logged in again.
func (s *server) connectOnStartup() {
	queued := 0
	// Connect ALL users with auth_token (not just connected=1)
	rows, err := s.db.Queryx(`SELECT id, name, token, max_user_id, webhook, events, proxy_url, 
		CASE WHEN s3_enabled THEN 'true' ELSE 'false' END AS s3_enabled, 
		media_delivery, COALESCE(history, 0) as history, auth_token, device_id 
		FROM users WHERE auth_token IS NOT NULL AND auth_token != ''`)
	if err != nil {
		log.Error().Err(err).Msg("DB Problem")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			txtid         string
			name          string
			token         string
			maxUserID     *int64
			webhook       string
			events        string
			proxyURL      string
			s3Enabled     string
			mediaDelivery string
			history       int
			authToken     *string
			deviceID      *string
		)

		err = rows.Scan(&txtid, &name, &token, &maxUserID, &webhook, &events, &proxyURL, &s3Enabled, &mediaDelivery, &history, &authToken, &deviceID)
		if err != nil {
			log.Error().Err(err).Msg("DB Problem scanning row")
			continue
		}

		if authToken == nil || *authToken == "" {
			log.Warn().Str("userID", txtid).Msg("No auth token, skipping reconnect")
			continue
		}

		log.Info().Str("userID", txtid).Str("name", name).Msg("Connect to MAX on startup")

		v := Values{map[string]string{
			"Id":            txtid,
			"Name":          name,
			"MaxUserID":     fmt.Sprintf("%d", safeInt64(maxUserID)),
			"Webhook":       webhook,
			"Token":         token,
			"Proxy":         proxyURL,
			"Events":        events,
			"S3Enabled":     s3Enabled,
			"MediaDelivery": mediaDelivery,
			"History":       fmt.Sprintf("%d", history),
		}}
		userinfocache.Set(token, v, cache.NoExpiration)

		eventarray := strings.Split(events, ",")
		var subscribedEvents []string
		if len(eventarray) == 1 && eventarray[0] == "" {
			subscribedEvents = []string{}
		} else {
			for _, arg := range eventarray {
				arg = strings.TrimSpace(arg)
				if arg != "" && Find(supportedEventTypes, arg) && !Find(subscribedEvents, arg) {
					subscribedEvents = append(subscribedEvents, arg)
				}
			}
		}

		eventstring := strings.Join(subscribedEvents, ",")
		delay := startupDelay(queued)
		queued++
		log.Info().Str("events", eventstring).Int64("maxUserID", safeInt64(maxUserID)).Dur("in", delay).Msg("Login at startup queued")

		go func(userID, authToken, deviceID, token string, subs []string, delay time.Duration) {
			if delay > 0 {
				time.Sleep(delay)
			}
			if clientManager.GetMaxClient(userID) != nil {
				log.Info().Str("userID", userID).Msg("The user has a session already; no login at startup")
				return
			}
			clientManager.NewKillChannel(userID)
			s.startClient(userID, authToken, deviceID, token, subs)
		}(txtid, *authToken, safeString(deviceID), token, subscribedEvents, delay)

		// Initialize S3 client if configured
		go func(userID string) {
			var s3Config struct {
				Enabled       bool   `db:"s3_enabled"`
				Endpoint      string `db:"s3_endpoint"`
				Region        string `db:"s3_region"`
				Bucket        string `db:"s3_bucket"`
				AccessKey     string `db:"s3_access_key"`
				SecretKey     string `db:"s3_secret_key"`
				PathStyle     bool   `db:"s3_path_style"`
				PublicURL     string `db:"s3_public_url"`
				RetentionDays int    `db:"s3_retention_days"`
			}

			err := s.db.Get(&s3Config, `
					SELECT s3_enabled, s3_endpoint, s3_region, s3_bucket, 
						   s3_access_key, s3_secret_key, s3_path_style, 
						   s3_public_url, s3_retention_days
					FROM users WHERE id = $1`, userID)

			if err != nil {
				log.Error().Err(err).Str("userID", userID).Msg("Failed to get S3 config")
				return
			}

			if s3Config.Enabled {
				config := &S3Config{
					Enabled:       s3Config.Enabled,
					Endpoint:      s3Config.Endpoint,
					Region:        s3Config.Region,
					Bucket:        s3Config.Bucket,
					AccessKey:     s3Config.AccessKey,
					SecretKey:     s3Config.SecretKey,
					PathStyle:     s3Config.PathStyle,
					PublicURL:     s3Config.PublicURL,
					RetentionDays: s3Config.RetentionDays,
				}

				err = GetS3Manager().InitializeS3Client(userID, config)
				if err != nil {
					log.Error().Err(err).Str("userID", userID).Msg("Failed to initialize S3 client on startup")
				} else {
					log.Info().Str("userID", userID).Msg("S3 client initialized on startup")
				}
			}
		}(txtid)
	}

	if err = rows.Err(); err != nil {
		log.Error().Err(err).Msg("DB Problem iterating rows")
	}
}

// ensureHTTPClient registers a resty HTTP client for the user if one is not
// already registered. Idempotent so every session path (QR auth, reconnect on
// startup, /session/connect) can call it before the first webhook fires —
// mirrors wuzapi, where HTTPClient is set up before the QR loop emits events.
func (s *server) ensureHTTPClient(userID string) {
	if clientManager.GetHTTPClient(userID) != nil {
		return
	}
	clientManager.SetHTTPClient(userID, s.newWebhookClient(userID))
}

// newWebhookClient builds the HTTP client the user's webhooks go through,
// with the user's proxy.
func (s *server) newWebhookClient(userID string) *resty.Client {
	httpClient := resty.New()
	httpClient.SetRedirectPolicy(resty.FlexibleRedirectPolicy(15))
	httpClient.SetTimeout(30 * time.Second)
	httpClient.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	httpClient.OnError(func(req *resty.Request, err error) {
		if v, ok := err.(*resty.ResponseError); ok {
			log.Debug().Str("response", v.Response.String()).Msg("resty error")
			log.Error().Err(v.Err).Msg("resty error")
		}
	})

	var proxyURL string
	if err := s.db.Get(&proxyURL, "SELECT COALESCE(proxy_url, '') FROM users WHERE id=$1", userID); err == nil && proxyURL != "" {
		httpClient.SetProxy(proxyURL)
	}
	return httpClient
}

// buildSyncPostmap assembles the Sync event: the session is logged in. It
// carries the account and its name and photo, never MAX's login answer: that
// holds the account's chats with their last messages, which a session event
// has no business carrying (C10). The chats are GET /chat/list, people GET
// /user/info, the profile GET /session/status.
func buildSyncPostmap(reconnect bool, client *maxclient.Client) map[string]interface{} {
	name, photo := accountProfile(client)
	return map[string]interface{}{
		"type":      "Sync",
		"reconnect": reconnect,
		"maxUserID": client.MaxUserID,
		"name":      name,
		"photo":     photo,
	}
}

// accountProfile is the logged-in account's name and photo.
func accountProfile(client *maxclient.Client) (name, photo string) {
	if client == nil || client.Me == nil {
		return "", ""
	}
	return maxclient.DisplayName(client.Me.Names), client.Me.BaseURL
}

// storeLogin records a login of userID: connected, the MAX account, and its
// name and photo for the status without a session.
func (s *server) storeLogin(userID string, client *maxclient.Client) {
	name, photo := accountProfile(client)
	if _, err := s.db.Exec("UPDATE users SET connected=1, max_user_id=$1, max_name=$2, max_photo=$3 WHERE id=$4",
		client.MaxUserID, name, photo, userID); err != nil {
		log.Error().Err(err).Msg("Failed to update connected status")
	}
}

// Machine reasons of AuthExpired (C9), shared with vkapi. MAX's own text goes
// to the log only.
const (
	ReasonTokenRevoked  = "token_revoked"  // MAX refused the stored login; it is erased, a QR login is needed
	ReasonPageBlocked   = "page_blocked"   // MAX blocked the account (login.blocked); the login is erased
	ReasonRefreshFailed = "refresh_failed" // MAX refused logging in again; the login is kept, POST /session/connect tries again
	ReasonLoggedOut     = "logged_out"     // MAX ended the session; the login is erased, the user stays
)

// authExpiredReason tells the reason of an auth error that erases the login.
func authExpiredReason(err error) string {
	if code, ok := maxclient.ServerErrorCode(err); ok && code == maxclient.CodeLoginBlocked {
		return ReasonPageBlocked
	}
	return ReasonTokenRevoked
}

// initClient performs the one-shot connect+login and registers a fresh
// MaxClient / MyClient / HTTPClient for the user. On success it returns the
// live objects and the raw syncData from the server. On failure it performs
// all cleanup itself (AuthExpired webhook, DB reset, manager purge) and
// returns the original error so the caller can distinguish success without
// having to repeat that bookkeeping.
func (s *server) initClient(userID, authToken, deviceID, token string, subscriptions []string) (*maxclient.Client, *MyClient, map[string]interface{}, error) {
	log.Info().Str("userid", userID).Msg("Starting WebSocket connection to MAX")

	if deviceID == "" {
		deviceID = uuid.New().String()
		if _, err := s.db.Exec("UPDATE users SET device_id=$1 WHERE id=$2", deviceID, userID); err != nil {
			log.Error().Err(err).Msg("Failed to save device ID")
		}
	}

	logger := log.With().Str("userID", userID).Logger()
	client := maxclient.NewClient(deviceID, logger)
	clientManager.SetMaxClient(userID, client)

	mycli := &MyClient{
		MaxClient:     client,
		userID:        userID,
		token:         token,
		subscriptions: subscriptions,
		db:            s.db,
		s:             s,
	}
	clientManager.SetMyClient(userID, mycli)

	client.SetEventHandler(func(event maxclient.Event) {
		mycli.handleEvent(event)
	})

	s.ensureHTTPClient(userID)

	syncData, err := client.ConnectAndLogin(authToken, nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to MAX")
		if maxclient.IsAuthError(err) {
			reason := authExpiredReason(err)
			log.Warn().Err(err).Str("userID", userID).Str("reason", reason).Msg("MAX refused the stored login; erasing it")
			if _, dbErr := s.db.Exec("UPDATE users SET auth_token=NULL, connected=0 WHERE id=$1", userID); dbErr != nil {
				log.Error().Err(dbErr).Msg("Failed to clear auth token")
			}
			sendEventWithWebHook(mycli, map[string]interface{}{
				"type":   "AuthExpired",
				"reason": reason,
			}, "")
		}
		cleanupClient(userID)
		return nil, nil, nil, err
	}

	s.storeLogin(userID, client)

	log.Info().Int64("maxUserID", client.MaxUserID).Msg("Connected to MAX")
	return client, mycli, syncData, nil
}

// startClient is the goroutine entry point for /session/connect when a stored
// auth_token is already available. It runs initClient, fires the initial Sync
// webhook, then enters the supervised reconnect loop.
func (s *server) startClient(userID string, authToken string, deviceID string, token string, subscriptions []string) {
	client, mycli, _, err := s.initClient(userID, authToken, deviceID, token, subscriptions)
	if err != nil {
		return
	}

	sendEventWithWebHook(mycli, buildSyncPostmap(false, client), "")
	s.runClientLoop(userID, authToken, client, mycli)
}

// reconnectDelayUnit is the unit of the -maxconnectdelay and -maxconnectcap
// values; tests shrink it.
var reconnectDelayUnit = time.Second

// reconnectBackoff returns the delay for the Nth reconnect attempt (1-indexed).
// Doubles per attempt from -maxconnectdelay up to -maxconnectcap seconds, then
// spreads by ±20% so that accounts dropped together (a MAX outage, a gateway
// restart) do not all log in again at the same moment.
func reconnectBackoff(attempt int) time.Duration {
	base := defaultConnectRetryDelay
	if connectRetryDelay != nil && *connectRetryDelay > 0 {
		base = *connectRetryDelay
	}
	cap := defaultConnectRetryCap
	if connectRetryCap != nil && *connectRetryCap > 0 {
		cap = *connectRetryCap
	}
	shift := uint(attempt - 1)
	if shift > 20 {
		shift = 20
	}
	secs := base << shift
	if secs <= 0 || secs > cap {
		secs = cap
	}
	return time.Duration(float64(time.Duration(secs)*reconnectDelayUnit) * (0.8 + 0.4*rand.Float64()))
}

// maxLoginRefusals is how many reconnect logins in a row MAX may refuse
// before the loop stops. Logging in again and again after MAX ended a
// session is what gets an account blocked (PyMax #106). Network failures on
// the way to the login neither count nor reset the count: they say nothing
// about the account. A successful login resets it.
const maxLoginRefusals = 3

// loginRefusal reports whether a failed reconnect login counts as MAX
// refusing it, and why: an error code of MAX's own, on SessionInit or on
// LOGIN, or LOGIN unanswered or losing its socket once the connection and
// SessionInit went through, which is how MAX may refuse too. A network
// failure before LOGIN is no refusal. Auth errors are told apart before.
func loginRefusal(err error) (reason string, refused bool) {
	if code, ok := maxclient.ServerErrorCode(err); ok {
		return code, true
	}
	var loginErr *maxclient.LoginError
	if errors.As(err, &loginErr) {
		switch {
		case errors.Is(err, maxclient.ErrTimeout):
			return "login timed out", true
		case errors.Is(err, maxclient.ErrNotConnected):
			return "login connection lost", true
		}
	}
	return "", false
}

// stopAfterLoginFailure handles a reconnect login of client that failed with
// err and reports whether the reconnect loop must stop:
//   - on an auth error (authErrorCodes) the token is dead and is erased at
//     once, as before;
//   - on the maxLoginRefusals-th refusal in a row (loginRefusal) the token is
//     kept: a partial MAX failure would otherwise erase every account's token
//     within minutes and send every owner through a new QR login, itself a
//     risk signal. The next attempt waits for POST /session/connect, that is
//     for the owner.
//
// login.flood (MAX: too many logins) stops the loop at once, the token
// kept, as the third refusal does: the web client stops at it too.
//
// Either stop marks the user disconnected, closes the client, sends
// AuthExpired with a machine reason (token_revoked or page_blocked for an
// auth error, refresh_failed for refusals; MAX's text goes to the log) and
// drops the user's clients. On anything else, a network failure, the loop
// retries with backoff.
func (s *server) stopAfterLoginFailure(userID string, client *maxclient.Client, mycli *MyClient, err error, refusals *int) bool {
	var reason string
	if maxclient.IsAuthError(err) {
		reason = authExpiredReason(err)
		log.Warn().Err(err).Str("userID", userID).Str("reason", reason).Msg("MAX refused the login during reconnect; erasing it")
		if _, dbErr := s.db.Exec("UPDATE users SET auth_token=NULL, connected=0 WHERE id=$1", userID); dbErr != nil {
			log.Error().Err(dbErr).Msg("Failed to clear auth token")
		}
	} else {
		why, refused := loginRefusal(err)
		if !refused {
			return false
		}
		*refusals++
		if code, _ := maxclient.ServerErrorCode(err); code == maxclient.CodeLoginFlood {
			*refusals = maxLoginRefusals
		}
		if *refusals < maxLoginRefusals {
			log.Warn().Str("userID", userID).Str("reason", why).Int("refusals", *refusals).
				Msg("MAX refused the reconnect login")
			return false
		}
		log.Warn().Str("userID", userID).Str("reason", why).Int("refusals", *refusals).
			Msg("MAX refused the reconnect login too many times in a row; stopping until POST /session/connect, the token is kept")
		if _, dbErr := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID); dbErr != nil {
			log.Error().Err(dbErr).Msg("Failed to update disconnected status")
		}
		reason = ReasonRefreshFailed
	}

	client.Close()
	sendEventWithWebHook(mycli, map[string]interface{}{
		"type":   "AuthExpired",
		"reason": reason,
	}, "")
	cleanupClient(userID)
	return true
}

// reconnectLimit returns the configured max attempts (0 means infinite).
func reconnectLimit() int {
	if connectMaxRetries == nil {
		return defaultConnectMaxRetries
	}
	return *connectMaxRetries
}

// runClientLoop supervises the connected client: detects drops, backs off and
// reconnects, and exits on a kill signal or when the client is replaced by a
// newer startClient invocation. Intended to run in its own goroutine.
func (s *server) runClientLoop(userID string, authToken string, client *maxclient.Client, mycli *MyClient) {
	reconnectAttempts := 0
	maxReconnectAttempts := reconnectLimit()
	loginRefusals := 0

	killCh := clientManager.GetKillChannel(userID)
	if killCh == nil {
		// Every start installs the kill channel before the loop; none means
		// the user was stopped (endLogin, a delete) before the loop began.
		log.Info().Str("userid", userID).Msg("Stopped before the loop began")
		client.Disconnect()
		return
	}
	for {
		select {
		case <-killCh:
			log.Info().Str("userid", userID).Msg("Received kill signal")
			client.Disconnect()
			cleanupClient(userID)
			_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID)
			if err != nil {
				log.Error().Err(err).Msg("Failed to update disconnected status")
			}
			return
		default:
			// Check if this client was replaced by another goroutine (e.g., RequestSync)
			currentClient := clientManager.GetMaxClient(userID)
			if currentClient != nil && currentClient != client {
				log.Info().Str("userid", userID).Msg("Client replaced, exiting startClient goroutine")
				return
			}

			if !client.IsConnected() {
				reconnectAttempts++

				if maxReconnectAttempts > 0 && reconnectAttempts > maxReconnectAttempts {
					log.Error().Str("userid", userID).Int("attempts", reconnectAttempts).Msg("Max reconnect attempts reached, giving up")

					postmap := map[string]interface{}{
						"type":     "ReconnectExhausted",
						"attempts": reconnectAttempts - 1,
						"reason":   "max_reconnect_attempts",
					}
					sendEventWithWebHook(mycli, postmap, "")
					cleanupClient(userID)

					_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID)
					if err != nil {
						log.Error().Err(err).Msg("Failed to update disconnected status")
					}
					return
				}

				backoff := reconnectBackoff(reconnectAttempts)
				log.Warn().Str("userid", userID).Int("attempt", reconnectAttempts).Int("max", maxReconnectAttempts).Dur("backoff", backoff).Msg("Connection lost, attempting reconnect...")

				// Send reconnecting event (only every 10 attempts to avoid spam)
				if reconnectAttempts == 1 || reconnectAttempts%10 == 0 {
					postmap := map[string]interface{}{
						"type":    "Reconnecting",
						"attempt": reconnectAttempts,
						"max":     maxReconnectAttempts,
					}
					sendEventWithWebHook(mycli, postmap, "")
				}

				select {
				case <-killCh:
					log.Info().Str("userid", userID).Msg("Kill signal during reconnect backoff")
					client.Disconnect()
					cleanupClient(userID)
					_, _ = s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID)
					return
				case <-time.After(backoff):
				}

				// Check again if client was replaced during the delay
				currentClient := clientManager.GetMaxClient(userID)
				if currentClient != nil && currentClient != client {
					log.Info().Str("userid", userID).Msg("Client replaced during reconnect delay, exiting")
					return
				}

				// Close old dead connection before creating new one
				client.Close()

				// Reconnect using Login (Sync opcode 21 has server-side bugs)
				_, err := client.ConnectAndLogin(authToken, nil)
				if err != nil {
					log.Error().Err(err).Int("attempt", reconnectAttempts).Msg("Reconnect failed")
					if s.stopAfterLoginFailure(userID, client, mycli, err, &loginRefusals) {
						return
					}
					continue
				}

				// Reconnect successful
				log.Info().Str("userid", userID).Int("attempts", reconnectAttempts).Msg("Reconnected successfully")
				reconnectAttempts = 0
				loginRefusals = 0

				s.storeLogin(userID, client)
				sendEventWithWebHook(mycli, buildSyncPostmap(true, client), "")
			} else {
				// Reset reconnect counter on successful connection
				reconnectAttempts = 0
			}
			time.Sleep(1 * time.Second)
		}
	}
}

// cleanupClient removes client from managers
func cleanupClient(userID string) {
	clientManager.DeleteMaxClient(userID)
	clientManager.DeleteMyClient(userID)
	clientManager.DeleteHTTPClient(userID)
	clientManager.DeleteKillChannel(userID)
}

// endLogin erases userID's MAX login and stops its session, keeping the
// gateway user: its token, webhook, subscription, settings and lookups. A
// new login is a QR login through POST /session/connect. It may run on the
// MaxClient's own receive goroutine (MAX's LoggedOut push), so it never
// closes the client itself: the kill signal makes the user's loop goroutine
// do it.
func (s *server) endLogin(userID string) {
	clientManager.SendKill(userID)
	if _, err := s.db.Exec("UPDATE users SET auth_token=NULL, connected=0 WHERE id=$1", userID); err != nil {
		log.Error().Err(err).Str("userID", userID).Msg("Failed to erase the login")
	}
	cleanupClient(userID)
}

// removeUser deletes gateway user userID: its row (its phone lookups go with
// it; a pause of its MAX account stays, for the next gateway user logged into
// it, see pauses.go), its running loop and clients, and its cached user info,
// which is what lets its token in: without dropping it the deleted token went
// on authenticating until a restart. The loop, clients and cache go even when
// the row could not be deleted: the next request reads the row again. An
// unknown userID is no error.
func (s *server) removeUser(userID string) error {
	token := s.tokenForUser(userID)
	_, err := s.db.Exec("DELETE FROM users WHERE id=$1", userID)

	// Non-blocking signal to any running loop goroutine, before cleanupClient
	// drops the kill channel: a signal sent after it reaches no loop, which
	// then goes on reconnecting with the deleted user's token. This may run
	// on the MaxClient's own receive goroutine, so the client is never closed
	// here: the kill signal makes the user's loop goroutine do it.
	clientManager.SendKill(userID)
	cleanupClient(userID)
	if token != "" {
		userinfocache.Delete(token)
	}
	return err
}

// refreshCachedUser sets fields of the cached user info of token, when it is
// cached. A copy is stored: requests and events read the cached map
// concurrently.
func refreshCachedUser(token string, fields map[string]string) {
	if token == "" {
		return
	}
	cached, ok := userinfocache.Get(token)
	if !ok {
		return
	}
	old, ok := cached.(Values)
	if !ok {
		return
	}
	m := make(map[string]string, len(old.m)+len(fields))
	for k, v := range old.m {
		m[k] = v
	}
	for k, v := range fields {
		m[k] = v
	}
	userinfocache.Set(token, Values{m}, cache.NoExpiration)
}

// gatewayEvents are the MAX notifications passed on as webhook events.
var gatewayEvents = map[string]bool{
	maxclient.EventTypeMessage:        true,
	maxclient.EventTypeMessageEdit:    true,
	maxclient.EventTypeMessageDelete:  true,
	maxclient.EventTypeReadReceipt:    true,
	maxclient.EventTypeChatUpdate:     true,
	maxclient.EventTypeTyping:         true,
	maxclient.EventTypeReactionChange: true,
	maxclient.EventTypeContactUpdate:  true,
	maxclient.EventTypePresenceUpdate: true,
	maxclient.EventTypeDisconnected:   true,
}

// subscribedTo reports whether the user is subscribed to eventType or All.
func (mycli *MyClient) subscribedTo(eventType string) bool {
	subs, err := updateAndGetUserSubscriptions(mycli)
	if err != nil {
		return false
	}
	return Find(subs, eventType) || Find(subs, "All")
}

// handleEvent passes a MAX notification on as a webhook event. The gateway
// does only the work the user subscribed to (M11): an event the user is not
// subscribed to is dropped before it is parsed, and the media of a message
// is only fetched for a Message subscriber.
func (mycli *MyClient) handleEvent(event maxclient.Event) {
	if event.Type == "LoggedOut" {
		// MAX ended the session (from the MAX app, or MAX itself): the login
		// is erased, the user stays, and the owner logs in by QR again (C9).
		log.Info().Str("userID", mycli.userID).Msg("MAX ended the session; erasing the login")
		sendEventWithWebHook(mycli, map[string]interface{}{"type": "AuthExpired", "reason": ReasonLoggedOut}, "")
		mycli.s.endLogin(mycli.userID)
		return
	}
	if !gatewayEvents[event.Type] {
		log.Debug().Str("type", event.Type).Msg("Unhandled event type")
		return
	}
	if event.Type == maxclient.EventTypeDisconnected {
		log.Info().Str("userID", mycli.userID).Msg("Received disconnect notification")
	}
	subscribed := mycli.subscribedTo(event.Type)

	postmap := map[string]interface{}{
		"type":   event.Type,
		"opcode": int(event.Opcode),
		"event":  event.Payload,
	}
	switch event.Type {
	case maxclient.EventTypeMessage:
		mycli.handleMessageEvent(event, postmap, subscribed)
	case maxclient.EventTypeChatUpdate:
		postmap["event"] = withoutLastMessage(event.Payload)
	}
	if !subscribed {
		return
	}
	sendEventWithWebHook(mycli, postmap, "")
}

// withoutLastMessage returns a copy of a ChatUpdate payload without the
// chat's lastMessage: a chat event carries no message's text (C10); Message
// does.
func withoutLastMessage(payload map[string]interface{}) map[string]interface{} {
	chat, ok := payload["chat"].(map[string]interface{})
	if !ok {
		return payload
	}
	if _, has := chat["lastMessage"]; !has {
		return payload
	}
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	c := make(map[string]interface{}, len(chat))
	for k, v := range chat {
		if k != "lastMessage" {
			c[k] = v
		}
	}
	out["chat"] = c
	return out
}

// handleMessageEvent handles an incoming message: its media for a Message
// subscriber, its text for the user's message history when that is on.
// Nothing of the message goes to the log but its ids.
func (mycli *MyClient) handleMessageEvent(event maxclient.Event, postmap map[string]interface{}, subscribed bool) {
	var historyLimit int
	if userinfo, found := userinfocache.Get(mycli.token); found {
		historyLimit, _ = strconv.Atoi(userinfo.(Values).Get("History"))
	}
	if !subscribed && historyLimit <= 0 {
		return
	}

	msgEvent, err := maxclient.ParseMessageEvent(event.Payload)
	if err != nil {
		log.Error().Err(err).Msg("Failed to parse message event")
		return
	}
	if msgEvent.Message == nil {
		return
	}

	msg := msgEvent.Message
	log.Debug().
		Str("userID", mycli.userID).
		Int64("chatId", msg.ChatID).
		Str("messageId", msg.ID).
		Int("attaches", len(msg.Attaches)).
		Msg("Message received")

	if subscribed && len(msg.Attaches) > 0 && !*skipMedia {
		mycli.processAttachments(msg, postmap)
	}

	if historyLimit > 0 && msg.Text != "" {
		err := mycli.s.saveMessageToHistory(
			mycli.userID,
			fmt.Sprintf("%d", msg.ChatID),
			fmt.Sprintf("%d", msg.Sender),
			msg.ID,
			string(msg.Type),
			msg.Text,
			"",
			"",
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to save message to history")
		} else {
			err = mycli.s.trimMessageHistory(mycli.userID, fmt.Sprintf("%d", msg.ChatID), historyLimit)
			if err != nil {
				log.Error().Err(err).Msg("Failed to trim message history")
			}
		}
	}
}

// processAttachments processes media attachments in a message
func (mycli *MyClient) processAttachments(msg *maxclient.Message, postmap map[string]interface{}) {
	var s3Config struct {
		Enabled       string `db:"s3_enabled"`
		MediaDelivery string `db:"media_delivery"`
	}

	// The cached user info has the media settings only when the startup
	// cached it; a user cached by a request has not, and reads them stored.
	userinfo, found := userinfocache.Get(mycli.token)
	if found && userinfo.(Values).Get("MediaDelivery") != "" {
		s3Config.Enabled = userinfo.(Values).Get("S3Enabled")
		s3Config.MediaDelivery = userinfo.(Values).Get("MediaDelivery")
	} else {
		err := mycli.db.Get(&s3Config, "SELECT CASE WHEN s3_enabled THEN 'true' ELSE 'false' END AS s3_enabled, COALESCE(media_delivery, 'base64') AS media_delivery FROM users WHERE id = $1", mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get S3 config from DB")
			s3Config.Enabled = "false"
			s3Config.MediaDelivery = "base64"
		}
	}

	for _, attach := range msg.Attaches {
		switch attach.Type {
		case maxclient.AttachTypePhoto:
			// Photo has direct URL
			if attach.BaseURL != "" {
				postmap["mediaUrl"] = attach.BaseURL
				postmap["mediaType"] = "image"

				if s3Config.Enabled == "true" || s3Config.MediaDelivery == "base64" {
					data, err := downloadMedia(attach.BaseURL)
					if err != nil {
						log.Error().Err(err).Msg("Failed to download photo")
						continue
					}

					if s3Config.Enabled == "true" && (s3Config.MediaDelivery == "s3" || s3Config.MediaDelivery == "both") {
						s3Data, err := GetS3Manager().ProcessMediaForS3(
							context.Background(),
							mycli.userID,
							fmt.Sprintf("%d", msg.ChatID),
							msg.ID,
							data,
							"image/jpeg",
							fmt.Sprintf("%d.jpg", attach.PhotoID),
							msg.Sender != mycli.MaxClient.MaxUserID,
						)
						if err != nil {
							log.Error().Err(err).Msg("Failed to upload to S3")
						} else {
							postmap["s3"] = s3Data
						}
					}

					if s3Config.MediaDelivery == "base64" || s3Config.MediaDelivery == "both" {
						postmap["base64"] = base64.StdEncoding.EncodeToString(data)
						postmap["mimeType"] = "image/jpeg"
					}
				}
			}

		case maxclient.AttachTypeVideo:
			// Video requires API call to get download URL
			postmap["mediaType"] = "video"
			postmap["videoId"] = attach.VideoID
			if attach.Token != "" {
				postmap["videoToken"] = attach.Token
			}

		case maxclient.AttachTypeFile:
			postmap["mediaType"] = "file"
			postmap["fileId"] = attach.FileID
			postmap["fileName"] = attach.Name
			postmap["fileSize"] = attach.Size

		case maxclient.AttachTypeAudio:
			postmap["mediaType"] = "audio"
			postmap["audioId"] = attach.AudioID
			if attach.URL != "" {
				postmap["audioUrl"] = attach.URL
			}
		}
	}
}

// fileToBase64 converts a file to base64
func fileToBase64(filepath string) (string, string, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return "", "", err
	}
	mimeType := http.DetectContentType(data)
	return base64.StdEncoding.EncodeToString(data), mimeType, nil
}

// Helper functions
func safeInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func safeString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
