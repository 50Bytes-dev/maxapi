package main

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"
)

// rabbitManager owns the RabbitMQ connection and channel. It transparently
// reconnects on link loss so that callers (Publish) only ever see a ready
// channel. The manager is intentionally coarse-grained — a single channel
// shared under RWMutex — because event publishing is infrequent per user and
// amqp091 channels are goroutine-safe for basic Publish.
type rabbitManager struct {
	url            string
	queue          string
	exchange       string
	conn           *amqp091.Connection
	channel        *amqp091.Channel
	mu             sync.RWMutex
	enabled        atomic.Bool
	done           chan struct{}
	reconnectDelay time.Duration
	maxAttempts    int
}

var rabbit *rabbitManager

// Publish errors surface to callers so callers can decide whether to log or
// drop. We never block the event path waiting for reconnect.
var errRabbitDisabled = errors.New("rabbitmq disabled")

// InitRabbitMQ wires up the manager from RABBITMQ_URL. Missing URL → disabled,
// not an error. Called once from init(); safe to call again after config
// changes (a subsequent call replaces the manager).
func InitRabbitMQ() {
	rabbitURL := os.Getenv("RABBITMQ_URL")
	queue := os.Getenv("RABBITMQ_QUEUE")
	if queue == "" {
		queue = "max_events"
	}
	exchange := os.Getenv("RABBITMQ_EXCHANGE")

	r := &rabbitManager{
		url:            rabbitURL,
		queue:          queue,
		exchange:       exchange,
		done:           make(chan struct{}),
		reconnectDelay: time.Duration(rabbitRetryDelaySec()) * time.Second,
		maxAttempts:    rabbitRetryMaxAttempts(),
	}
	rabbit = r

	if rabbitURL == "" {
		log.Info().Msg("RABBITMQ_URL is not set. RabbitMQ publishing disabled.")
		return
	}

	if err := r.connect(); err != nil {
		log.Error().Err(err).Msg("Could not connect to RabbitMQ after retries, publishing disabled")
		return
	}
	go r.watchReconnect()
	log.Info().Str("queue", r.queue).Str("exchange", r.exchange).Msg("RabbitMQ connection established.")
}

func rabbitRetryDelaySec() int {
	if rabbitRetryDelay != nil && *rabbitRetryDelay > 0 {
		return *rabbitRetryDelay
	}
	return 5
}

func rabbitRetryMaxAttempts() int {
	if rabbitRetryCount != nil && *rabbitRetryCount > 0 {
		return *rabbitRetryCount
	}
	return 10
}

// connect dials with linear backoff up to maxAttempts. On success the
// enabled flag flips to true so Publish starts accepting messages. Caller
// holds no lock; connect takes its own write lock.
func (r *rabbitManager) connect() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		conn, err := amqp091.Dial(r.url)
		if err == nil {
			ch, chErr := conn.Channel()
			if chErr != nil {
				_ = conn.Close()
				log.Warn().Err(chErr).Int("attempt", attempt).Msg("RabbitMQ channel open failed")
			} else {
				r.conn = conn
				r.channel = ch
				if derr := r.declareTopologyLocked(); derr != nil {
					log.Warn().Err(derr).Msg("RabbitMQ topology declaration failed (non-fatal)")
				}
				r.enabled.Store(true)
				return nil
			}
		} else {
			log.Warn().Err(err).Int("attempt", attempt).Int("max", r.maxAttempts).Msg("RabbitMQ dial failed")
		}
		wait := time.Duration(attempt) * r.reconnectDelay
		select {
		case <-r.done:
			return errors.New("rabbitmq manager stopped")
		case <-time.After(wait):
		}
	}
	r.enabled.Store(false)
	return errors.New("rabbitmq reconnect exhausted")
}

// declareTopologyLocked declares the queue (and exchange when configured) on
// the current channel. Called under r.mu write lock.
func (r *rabbitManager) declareTopologyLocked() error {
	if r.channel == nil {
		return errors.New("channel not open")
	}
	if _, err := r.channel.QueueDeclare(r.queue, true, false, false, false, nil); err != nil {
		return err
	}
	if r.exchange != "" {
		if err := r.channel.ExchangeDeclare(r.exchange, "topic", true, false, false, false, nil); err != nil {
			return err
		}
	}
	return nil
}

// watchReconnect listens on the connection's NotifyClose channel and triggers
// a reconnect loop. Exits on shutdown. Runs as a dedicated goroutine.
func (r *rabbitManager) watchReconnect() {
	for {
		r.mu.RLock()
		conn := r.conn
		r.mu.RUnlock()
		if conn == nil {
			return
		}
		notify := conn.NotifyClose(make(chan *amqp091.Error, 1))
		select {
		case <-r.done:
			return
		case err := <-notify:
			if err != nil {
				log.Warn().Str("reason", err.Reason).Int("code", err.Code).Msg("RabbitMQ connection closed, reconnecting")
			} else {
				log.Warn().Msg("RabbitMQ connection closed (nil error), reconnecting")
			}
			r.enabled.Store(false)
			if cerr := r.connect(); cerr != nil {
				log.Error().Err(cerr).Msg("RabbitMQ reconnect failed, publishing stays disabled until next attempt")
				return
			}
			log.Info().Msg("RabbitMQ reconnected")
		}
	}
}

// Publish sends a persistent message. When an exchange is configured (topic),
// the event type is used as the routing key and queueOverride is ignored.
// Returns errRabbitDisabled when the link isn't ready — callers treat this
// as a non-fatal drop.
func (r *rabbitManager) Publish(body []byte, routingKey string, headers amqp091.Table, queueOverride ...string) error {
	if r == nil || !r.enabled.Load() {
		return errRabbitDisabled
	}
	r.mu.RLock()
	ch := r.channel
	exchange := r.exchange
	queue := r.queue
	r.mu.RUnlock()
	if ch == nil {
		return errRabbitDisabled
	}

	rk := routingKey
	if exchange == "" {
		rk = queue
		if len(queueOverride) > 0 && queueOverride[0] != "" {
			rk = queueOverride[0]
		}
	}

	pub := amqp091.Publishing{
		Headers:      headers,
		DeliveryMode: amqp091.Persistent,
		MessageId:    uuid.New().String(),
		Timestamp:    time.Now(),
		ContentType:  "application/json",
		Body:         body,
	}

	return ch.Publish(exchange, rk, false, false, pub)
}

// Close tears down the manager for graceful shutdown.
func (r *rabbitManager) Close() {
	if r == nil {
		return
	}
	select {
	case <-r.done:
		return
	default:
		close(r.done)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channel != nil {
		_ = r.channel.Close()
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.enabled.Store(false)
}

// PublishToRabbit is the legacy helper retained for callers that don't need
// routing keys or headers. New call sites should use rabbit.Publish directly.
func PublishToRabbit(data []byte, queueOverride ...string) error {
	if rabbit == nil {
		return errRabbitDisabled
	}
	return rabbit.Publish(data, "", nil, queueOverride...)
}

// sendToGlobalRabbit enriches the raw event JSON with instance identity,
// timestamp, and server host, then publishes to RabbitMQ. The enriched fields
// match the webhook payload so downstream consumers share a single schema.
func sendToGlobalRabbit(jsonData []byte, token string, userID string, queueName ...string) {
	if rabbit == nil || !rabbit.enabled.Load() {
		log.Debug().Msg("RabbitMQ publishing is disabled, not sending message")
		return
	}

	instanceName := ""
	if userinfo, found := userinfocache.Get(token); found {
		instanceName = userinfo.(Values).Get("Name")
	}

	var data map[string]interface{}
	if err := json.Unmarshal(jsonData, &data); err != nil {
		log.Error().Err(err).Msg("Failed to unmarshal event JSON for RabbitMQ")
		return
	}

	hostname, _ := os.Hostname()
	data["userID"] = userID
	data["instanceID"] = userID
	data["instanceName"] = instanceName
	data["eventTimestamp"] = time.Now().Unix()
	data["serverHost"] = hostname
	delete(data, "token")

	eventType, _ := data["type"].(string)

	enriched, err := json.Marshal(data)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal enriched event for RabbitMQ")
		return
	}

	headers := amqp091.Table{
		"x-instance-id": userID,
		"x-event-type":  eventType,
	}

	if err := rabbit.Publish(enriched, eventType, headers, queueName...); err != nil && !errors.Is(err, errRabbitDisabled) {
		log.Error().Err(err).Str("eventType", eventType).Msg("Failed to publish to RabbitMQ")
	}
}
