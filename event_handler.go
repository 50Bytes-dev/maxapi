package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// sendToGlobalWebHook sends event data to the global webhook
func sendToGlobalWebHook(jsonData []byte, token string, userID string) {
	jsonDataStr := string(jsonData)

	instanceName := ""
	userinfo, found := userinfocache.Get(token)
	if found {
		instanceName = userinfo.(Values).Get("Name")
	}

	if *globalWebhook != "" {
		log.Info().Str("url", *globalWebhook).Msg("Calling global webhook")
		globalData := map[string]string{
			"jsonData":     jsonDataStr,
			"token":        token,
			"userID":       userID,
			"instanceName": instanceName,
		}
		callHook(*globalWebhook, globalData, userID)
	}
}

// sendToUserWebHook sends event data to the user's webhook
func sendToUserWebHook(webhookurl string, path string, jsonData []byte, userID string, token string) {
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

	log.Debug().Interface("webhookData", data).Msg("Data being sent to webhook")

	if webhookurl != "" {
		log.Info().Str("url", webhookurl).Msg("Calling user webhook")
		if path == "" {
			go callHook(webhookurl, data, userID)
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

// getUserWebhookUrl returns the webhook URL for a user
func getUserWebhookUrl(token string) string {
	webhookurl := ""
	myuserinfo, found := userinfocache.Get(token)
	if !found {
		log.Warn().Str("token", token).Msg("Could not call webhook as there is no user for this token")
	} else {
		webhookurl = myuserinfo.(Values).Get("Webhook")
	}
	return webhookurl
}

// sendEventWithWebHook sends an event through webhook
func sendEventWithWebHook(mycli *MyClient, postmap map[string]interface{}, path string) {
	webhookurl := getUserWebhookUrl(mycli.token)

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

	sendToUserWebHook(webhookurl, path, jsonData, mycli.userID, mycli.token)
	go sendToGlobalWebHook(jsonData, mycli.token, mycli.userID)
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

// connectOnStartup connects all authenticated users to MAX on server startup
func (s *server) connectOnStartup() {
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

		log.Info().Str("token", token).Msg("Connect to MAX on startup")

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
		log.Info().Str("events", eventstring).Int64("maxUserID", safeInt64(maxUserID)).Msg("Attempt to connect")

		killchannel[txtid] = make(chan bool)
		go s.startClient(txtid, *authToken, safeString(deviceID), token, subscribedEvents)

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

// startClient starts a MAX client for a user
func (s *server) startClient(userID string, authToken string, deviceID string, token string, subscriptions []string) {
	log.Info().Str("userid", userID).Msg("Starting WebSocket connection to MAX")

	// Create or use existing device ID
	if deviceID == "" {
		deviceID = uuid.New().String()
		// Save device ID to database
		_, err := s.db.Exec("UPDATE users SET device_id=$1 WHERE id=$2", deviceID, userID)
		if err != nil {
			log.Error().Err(err).Msg("Failed to save device ID")
		}
	}

	// Create MAX client
	logger := log.With().Str("userID", userID).Logger()
	client := maxclient.NewClient(deviceID, logger)

	clientManager.SetMaxClient(userID, client)

	// Create MyClient wrapper
	mycli := &MyClient{
		MaxClient:     client,
		userID:        userID,
		token:         token,
		subscriptions: subscriptions,
		db:            s.db,
		s:             s,
	}
	clientManager.SetMyClient(userID, mycli)

	// Set up event handler
	client.SetEventHandler(func(event maxclient.Event) {
		mycli.handleEvent(event)
	})

	// Create HTTP client
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

	// Set proxy if defined
	var proxyURL string
	err := s.db.Get(&proxyURL, "SELECT proxy_url FROM users WHERE id=$1", userID)
	if err == nil && proxyURL != "" {
		httpClient.SetProxy(proxyURL)
	}
	clientManager.SetHTTPClient(userID, httpClient)

	// Connect and login
	syncData, err := client.ConnectAndLogin(authToken, nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to connect to MAX")
		cleanupClient(userID)
		return
	}

	// Update connected status
	_, err = s.db.Exec("UPDATE users SET connected=1, max_user_id=$1 WHERE id=$2", client.MaxUserID, userID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to update connected status")
	}

	// Send Sync event with raw data from MAX server
	postmap := map[string]interface{}{
		"type":      "Sync",
		"reconnect": false,
		"maxUserID": client.MaxUserID,
	}
	// Merge raw sync data into postmap (preserves all fields from MAX server)
	for key, value := range syncData {
		if key != "type" { // Don't override type
			postmap[key] = value
		}
	}
	sendEventWithWebHook(mycli, postmap, "")

	log.Info().Int64("maxUserID", client.MaxUserID).Msg("Connected to MAX")

	// Keep connection alive with auto-reconnect
	reconnectAttempts := 0
	maxReconnectAttempts := 120
	reconnectDelay := 5 * time.Second

	for {
		select {
		case <-killchannel[userID]:
			log.Info().Str("userid", userID).Msg("Received kill signal")
			client.Disconnect()
			cleanupClient(userID)
			_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID)
			if err != nil {
				log.Error().Err(err).Msg("Failed to update disconnected status")
			}
			return
		default:
			if !client.IsConnected() {
				reconnectAttempts++

				if reconnectAttempts > maxReconnectAttempts {
					log.Error().Str("userid", userID).Int("attempts", reconnectAttempts).Msg("Max reconnect attempts reached, giving up")
					cleanupClient(userID)

					postmap := map[string]interface{}{
						"type":   "Disconnected",
						"reason": "max_reconnect_attempts",
					}
					sendEventWithWebHook(mycli, postmap, "")

					_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", userID)
					if err != nil {
						log.Error().Err(err).Msg("Failed to update disconnected status")
					}
					return
				}

				log.Warn().Str("userid", userID).Int("attempt", reconnectAttempts).Int("max", maxReconnectAttempts).Msg("Connection lost, attempting reconnect...")

				// Send reconnecting event (only every 10 attempts to avoid spam)
				if reconnectAttempts == 1 || reconnectAttempts%10 == 0 {
					postmap := map[string]interface{}{
						"type":    "Reconnecting",
						"attempt": reconnectAttempts,
						"max":     maxReconnectAttempts,
					}
					sendEventWithWebHook(mycli, postmap, "")
				}

				time.Sleep(reconnectDelay)

				// Try to reconnect using Sync (not Login) since user is already authenticated
				syncData, err := client.ConnectAndSync(nil)
				if err != nil {
					log.Error().Err(err).Int("attempt", reconnectAttempts).Msg("Reconnect failed")
					continue
				}

				// Reconnect successful
				log.Info().Str("userid", userID).Int("attempts", reconnectAttempts).Msg("Reconnected successfully")
				reconnectAttempts = 0

				// Update connected status
				_, err = s.db.Exec("UPDATE users SET connected=1, max_user_id=$1 WHERE id=$2", client.MaxUserID, userID)
				if err != nil {
					log.Error().Err(err).Msg("Failed to update connected status")
				}

				// Send Sync event with raw data from MAX server
				postmap := map[string]interface{}{
					"type":      "Sync",
					"reconnect": true,
					"maxUserID": client.MaxUserID,
				}
				// Merge raw sync data into postmap (preserves all fields from MAX server)
				for key, value := range syncData {
					if key != "type" { // Don't override type
						postmap[key] = value
					}
				}
				sendEventWithWebHook(mycli, postmap, "")
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
	delete(killchannel, userID)
}

// safeDeleteUser deletes a user safely, idempotent for repeated calls
func (s *server) safeDeleteUser(userID string, sendWebhook bool) {
	log.Info().Str("userID", userID).Bool("sendWebhook", sendWebhook).Msg("Safe delete user")

	// 1. Check if user exists in DB
	var exists bool
	err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)", userID).Scan(&exists)
	if err != nil {
		log.Error().Err(err).Str("userID", userID).Msg("Failed to check user existence")
	}
	if !exists {
		log.Info().Str("userID", userID).Msg("User already deleted from DB")
		// Still cleanup clients just in case
		cleanupClient(userID)
		return
	}

	// 2. Send webhook if needed (before deleting, while mycli still exists)
	if sendWebhook {
		mycli := clientManager.GetMyClient(userID)
		if mycli != nil {
			postmap := map[string]interface{}{"type": "LoggedOut"}
			sendEventWithWebHook(mycli, postmap, "")
		}
	}

	// 3. Delete from DB
	_, err = s.db.Exec("DELETE FROM users WHERE id=$1", userID)
	if err != nil {
		log.Error().Err(err).Str("userID", userID).Msg("Failed to delete user from DB")
	} else {
		log.Info().Str("userID", userID).Msg("User deleted from DB")
	}

	// 4. Cleanup clients (idempotent)
	cleanupClient(userID)

	// 5. Non-blocking signal to killchannel
	if ch := killchannel[userID]; ch != nil {
		select {
		case ch <- true:
		default:
		}
	}
}

// handleEvent handles MAX events and sends webhooks
func (mycli *MyClient) handleEvent(event maxclient.Event) {
	postmap := make(map[string]interface{})
	postmap["type"] = event.Type
	postmap["opcode"] = int(event.Opcode)
	postmap["event"] = event.Payload
	path := ""

	switch event.Type {
	case maxclient.EventTypeMessage:
		mycli.handleMessageEvent(event, postmap)
	case maxclient.EventTypeMessageEdit:
		postmap["type"] = "MessageEdit"
	case maxclient.EventTypeMessageDelete:
		postmap["type"] = "MessageDelete"
	case maxclient.EventTypeReadReceipt:
		postmap["type"] = "ReadReceipt"
	case maxclient.EventTypeChatUpdate:
		postmap["type"] = "ChatUpdate"
	case maxclient.EventTypeTyping:
		postmap["type"] = "Typing"
	case maxclient.EventTypeReactionChange:
		postmap["type"] = "ReactionChange"
	case maxclient.EventTypeContactUpdate:
		postmap["type"] = "ContactUpdate"
	case maxclient.EventTypePresenceUpdate:
		postmap["type"] = "PresenceUpdate"
	case maxclient.EventTypeDisconnected:
		postmap["type"] = "Disconnected"
		log.Info().Str("userID", mycli.userID).Msg("Received disconnect notification")
	case "LoggedOut":
		log.Info().Str("userID", mycli.userID).Msg("Received LoggedOut event from MAX")
		mycli.s.safeDeleteUser(mycli.userID, true)
		return // Don't continue processing
	default:
		log.Debug().Str("type", event.Type).Msg("Unhandled event type")
		return
	}

	sendEventWithWebHook(mycli, postmap, path)
}

// handleMessageEvent handles incoming message events
func (mycli *MyClient) handleMessageEvent(event maxclient.Event, postmap map[string]interface{}) {
	msgEvent, err := maxclient.ParseMessageEvent(event.Payload)
	if err != nil {
		log.Error().Err(err).Msg("Failed to parse message event")
		return
	}

	if msgEvent.Message == nil {
		return
	}

	msg := msgEvent.Message
	log.Info().
		Int64("chatId", msg.ChatID).
		Int64("sender", msg.Sender).
		Str("text", truncateString(msg.Text, 50)).
		Msg("Message received")

	// Process media attachments
	if len(msg.Attaches) > 0 && !*skipMedia {
		mycli.processAttachments(msg, postmap)
	}

	// Save to history if enabled
	var historyLimit int
	userinfo, found := userinfocache.Get(mycli.token)
	if found {
		historyStr := userinfo.(Values).Get("History")
		historyLimit, _ = strconv.Atoi(historyStr)
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

	userinfo, found := userinfocache.Get(mycli.token)
	if !found {
		err := mycli.db.Get(&s3Config, "SELECT CASE WHEN s3_enabled = 1 THEN 'true' ELSE 'false' END AS s3_enabled, media_delivery FROM users WHERE id = $1", mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get S3 config from DB")
			s3Config.Enabled = "false"
			s3Config.MediaDelivery = "base64"
		}
	} else {
		s3Config.Enabled = userinfo.(Values).Get("S3Enabled")
		s3Config.MediaDelivery = userinfo.(Values).Get("MediaDelivery")
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

// downloadMedia downloads media from URL
func downloadMedia(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return data, nil
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

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
