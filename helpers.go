package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"
)

// envIntDefault reads an integer env var, returning def on missing/invalid.
func envIntDefault(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// newSafeHTTPClient returns an *http.Client whose dialer refuses to connect to
// private/loopback/link-local/multicast IPs. Used for all server-initiated
// outbound calls to mitigate SSRF when the target URL is attacker-influenced.
// Set ALLOW_PRIVATE_IPS=true to disable the guard (dev/test only).
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           safeDialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if isBlockedIP(ip.IP) {
			return nil, fmt.Errorf("blocked IP %s (SSRF guard)", ip.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs resolved for %s", host)
	}
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

func isBlockedIP(ip net.IP) bool {
	if strings.EqualFold(os.Getenv("ALLOW_PRIVATE_IPS"), "true") {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, cidr := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"100.64.0.0/10", "169.254.0.0/16",
		"fc00::/7", "fe80::/10",
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil && block.Contains(ip) {
			return true
		}
	}
	return false
}

// UserSemaphoreManager bounds concurrent heavy ops per user (media, webhook
// retries). Prevents a single noisy user from starving others.
type UserSemaphoreManager struct {
	semaphores sync.Map
	lastUsed   sync.Map
	size       int
	ttl        time.Duration
}

// NewUserSemaphoreManager builds the manager and starts its janitor goroutine.
// size = max concurrent ops per user.
func NewUserSemaphoreManager(size int) *UserSemaphoreManager {
	if size <= 0 {
		size = 4
	}
	m := &UserSemaphoreManager{size: size, ttl: 10 * time.Minute}
	go m.janitor()
	return m
}

// Acquire blocks until the user has a free slot. MUST NOT be called recursively
// from the same goroutine for the same userID (would self-deadlock).
func (m *UserSemaphoreManager) Acquire(userID string) {
	ch, _ := m.semaphores.LoadOrStore(userID, make(chan struct{}, m.size))
	ch.(chan struct{}) <- struct{}{}
	m.lastUsed.Store(userID, time.Now())
}

// Release frees one slot. Safe to call after a skipped Acquire (no-op in that
// case since the channel has no outstanding token to drain).
func (m *UserSemaphoreManager) Release(userID string) {
	if ch, ok := m.semaphores.Load(userID); ok {
		select {
		case <-ch.(chan struct{}):
		default:
		}
	}
}

func (m *UserSemaphoreManager) janitor() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.lastUsed.Range(func(k, v any) bool {
			if now.Sub(v.(time.Time)) > m.ttl {
				if ch, ok := m.semaphores.Load(k); ok && len(ch.(chan struct{})) == 0 {
					m.semaphores.Delete(k)
					m.lastUsed.Delete(k)
				}
			}
			return true
		})
	}
}

func Find(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

// SafeMaxClientStatus checks MAX client connection status
func SafeMaxClientStatus(userID string) (isConnected bool) {
	client := clientManager.GetMaxClient(userID)
	if client == nil {
		log.Warn().Str("userID", userID).Msg("MaxClient not found for user")
		return false
	}
	return client.IsConnected()
}

// respondError writes a standardized error body: {success:false, error:msg, code:MACHINE_CODE}.
// Used for any error where the client should be able to branch on a stable
// machine-readable code without parsing free-form messages.
func (s *server) respondError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	s.Respond(w, r, status, map[string]interface{}{
		"success": false,
		"error":   msg,
		"code":    code,
	})
}

// Respond sends a JSON response
func (s *server) Respond(w http.ResponseWriter, r *http.Request, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")

	var response map[string]interface{}

	switch v := payload.(type) {
	case error:
		response = map[string]interface{}{
			"success": false,
			"error":   v.Error(),
		}
	case map[string]interface{}:
		response = v
	default:
		response = map[string]interface{}{
			"success": true,
			"data":    v,
		}
	}

	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode JSON response")
	}
}

func isHTTPURL(input string) bool {
	parsed, err := url.ParseRequestURI(input)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

func fetchURLBytes(resourceURL string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", resourceURL, nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := globalHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	limitedBody := http.MaxBytesReader(nil, resp.Body, 10*1024*1024)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, "", err
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	return data, contentType, nil
}

// Update entry in User map
func updateUserInfo(values interface{}, field string, value string) interface{} {
	log.Debug().Str("field", field).Str("value", value).Msg("User info updated")
	values.(Values).m[field] = value
	return values
}

// webhookMaxRetries returns how many total attempts a webhook should make.
// Retries are disabled → single attempt; otherwise honour the configured count.
func webhookMaxRetries() int {
	if webhookRetryEnabled == nil || !*webhookRetryEnabled {
		return 1
	}
	if webhookRetryCount == nil || *webhookRetryCount <= 0 {
		return 1
	}
	return *webhookRetryCount
}

func webhookRetryDelay(attempt int) time.Duration {
	base := 30
	if webhookRetryDelaySeconds != nil && *webhookRetryDelaySeconds > 0 {
		base = *webhookRetryDelaySeconds
	}
	shift := uint(attempt - 1)
	if shift > 8 {
		shift = 8
	}
	return time.Duration(base) * time.Second * time.Duration(1<<shift)
}

// shouldRetryStatus — retry only on transient errors: network errors handled by
// caller (err != nil), 5xx from remote, and request timeout. 4xx is a client
// contract failure; retry would just spam the subscriber.
func shouldRetryStatus(code int) bool {
	return code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
}

// enrichWebhookPayload parses the raw jsonData string from the event pipeline
// and returns a body map augmented with identity/context fields. Mirrors the
// enrichment applied to RabbitMQ publishing so downstream consumers can trust
// a single schema regardless of transport.
func enrichWebhookPayload(payload map[string]string, userID string) map[string]interface{} {
	var body map[string]interface{}
	if jsonStr, ok := payload["jsonData"]; ok {
		if err := json.Unmarshal([]byte(jsonStr), &body); err != nil {
			body = nil
		}
	}
	if body == nil {
		body = map[string]interface{}{}
		for k, v := range payload {
			if k == "token" || k == "jsonData" {
				continue
			}
			body[k] = v
		}
	}
	body["userID"] = userID
	body["instanceID"] = userID
	if token, ok := payload["token"]; ok && token != "" {
		if userinfo, found := userinfocache.Get(token); found {
			body["instanceName"] = userinfo.(Values).Get("Name")
		}
	}
	if _, has := body["instanceName"]; !has {
		if name, ok := payload["instanceName"]; ok {
			body["instanceName"] = name
		}
	}
	body["eventTimestamp"] = time.Now().Unix()
	delete(body, "token")
	return body
}

// callHook delivers an event-style webhook. Retries on 5xx / network errors
// with exponential backoff bounded by webhookRetry* flags. Payload never
// contains the user's API token — identity comes from userID/instanceID/
// instanceName fields (see enrichWebhookPayload).
func callHook(myurl string, payload map[string]string, id string) {
	log.Info().Str("url", myurl).Msg("Sending POST to client " + id)

	client := clientManager.GetHTTPClient(id)
	if client == nil {
		log.Warn().Str("userID", id).Msg("HTTP client not found, skipping webhook")
		return
	}

	format := os.Getenv("WEBHOOK_FORMAT")
	body := enrichWebhookPayload(payload, id)

	var formData map[string]string
	if format != "json" {
		formData = make(map[string]string, len(body))
		for k, v := range body {
			formData[k] = stringifyForForm(v)
		}
	}

	maxAttempts := webhookMaxRetries()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(webhookRetryDelay(attempt))
		}

		req := client.R()
		var (
			resp *resty.Response
			err  error
		)
		if format == "json" {
			resp, err = req.SetHeader("Content-Type", "application/json").SetBody(body).Post(myurl)
		} else {
			resp, err = req.SetFormData(formData).Post(myurl)
		}

		if err != nil {
			log.Warn().Err(err).Str("url", myurl).Int("attempt", attempt).Msg("Webhook delivery failed (network)")
			continue
		}
		code := resp.StatusCode()
		if !shouldRetryStatus(code) {
			log.Info().Str("url", myurl).Int("status", code).Int("attempt", attempt).Msg("Webhook delivered")
			return
		}
		log.Warn().Str("url", myurl).Int("status", code).Int("attempt", attempt).Msg("Webhook delivery returned retriable status")
	}
	log.Error().Str("url", myurl).Int("attempts", maxAttempts).Str("userID", id).Msg("Webhook delivery exhausted retries")
}

// stringifyForForm converts enriched payload values to their form-urlencoded
// representation. Complex structures (e.g. parsed event payloads) get JSON
// encoded so the subscriber can still round-trip them.
func stringifyForForm(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	}
}

// callHookFile delivers a multipart webhook for events with an attached file.
// Enriches the form data the same way as callHook and shares the retry policy.
// Guarded by userSemaphoreManager so a slow subscriber can't consume unbounded
// file descriptors for a single user.
func callHookFile(myurl string, payload map[string]string, id string, file string) error {
	log.Info().Str("file", file).Str("url", myurl).Msg("Sending POST")

	client := clientManager.GetHTTPClient(id)
	if client == nil {
		log.Warn().Str("userID", id).Msg("HTTP client not found, skipping webhook")
		return fmt.Errorf("HTTP client not found for user %s", id)
	}

	if userSemaphoreManager != nil {
		userSemaphoreManager.Acquire(id)
		defer userSemaphoreManager.Release(id)
	}

	body := enrichWebhookPayload(payload, id)
	finalPayload := make(map[string]string, len(body)+1)
	for k, v := range body {
		finalPayload[k] = stringifyForForm(v)
	}
	finalPayload["file"] = file

	maxAttempts := webhookMaxRetries()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(webhookRetryDelay(attempt))
		}

		resp, err := client.R().
			SetFiles(map[string]string{"file": file}).
			SetFormData(finalPayload).
			Post(myurl)

		if err != nil {
			lastErr = err
			log.Warn().Err(err).Str("url", myurl).Int("attempt", attempt).Msg("Webhook file delivery failed (network)")
			continue
		}
		code := resp.StatusCode()
		if !shouldRetryStatus(code) {
			log.Info().Str("url", myurl).Int("status", code).Int("attempt", attempt).Msg("Webhook file delivered")
			return nil
		}
		lastErr = fmt.Errorf("retriable status %d", code)
		log.Warn().Str("url", myurl).Int("status", code).Int("attempt", attempt).Msg("Webhook file delivery returned retriable status")
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("unknown webhook failure")
	}
	return fmt.Errorf("webhook file delivery exhausted retries: %w", lastErr)
}

func (s *server) respondWithJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Error().Err(err).Msg("Failed to encode JSON response")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// ProcessOutgoingMedia handles media processing for outgoing messages with S3 support
func ProcessOutgoingMedia(userID string, contactJID string, messageID string, data []byte, mimeType string, fileName string, db *sqlx.DB) (map[string]interface{}, error) {
	if userSemaphoreManager != nil {
		userSemaphoreManager.Acquire(userID)
		defer userSemaphoreManager.Release(userID)
	}

	// Check if S3 is enabled for this user
	var s3Config struct {
		Enabled       bool   `db:"s3_enabled"`
		MediaDelivery string `db:"media_delivery"`
	}
	err := db.Get(&s3Config, "SELECT s3_enabled, media_delivery FROM users WHERE id = $1", userID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get S3 config")
		s3Config.Enabled = false
		s3Config.MediaDelivery = "base64"
	}

	// Process S3 upload if enabled
	if s3Config.Enabled && (s3Config.MediaDelivery == "s3" || s3Config.MediaDelivery == "both") {
		// Process S3 upload (outgoing messages are always in outbox)
		s3Data, err := GetS3Manager().ProcessMediaForS3(
			context.Background(),
			userID,
			contactJID,
			messageID,
			data,
			mimeType,
			fileName,
			false, // isIncoming = false for sent messages
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to upload media to S3")
			// Continue even if S3 upload fails
		} else {
			return s3Data, nil
		}
	}

	return nil, nil
}
