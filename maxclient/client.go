package maxclient

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	// WebSocket endpoint
	WebSocketURI    = "wss://ws-api.oneme.ru/websocket"
	WebSocketOrigin = "https://web.max.ru"

	// Protocol version for WebSocket
	ProtocolVersion = 11

	// Default timeouts
	DefaultTimeout    = 30 * time.Second
	PingInterval      = 30 * time.Second
	ReconnectDelay    = 1 * time.Second
	MaxReconnectDelay = 60 * time.Second

	// Circuit breaker
	MaxConsecutiveErrors = 10
	CircuitBreakerReset  = 60 * time.Second
)

// Client represents a MAX API client
type Client struct {
	// Connection
	conn   *websocket.Conn
	connMu sync.RWMutex

	// Authentication
	DeviceID  string
	AuthToken string
	MaxUserID int64
	Me        *Me

	// State
	seq           int32
	isConnected   bool
	isConnectedMu sync.RWMutex

	// Pending requests
	pending   map[int]chan *Response
	pendingMu sync.RWMutex

	// File upload waiters
	fileWaiters   map[int64]chan *Response
	fileWaitersMu sync.Mutex

	// User cache
	users   map[int64]*User
	usersMu sync.RWMutex

	// Event handling
	eventHandler func(Event)

	// Circuit breaker
	errorCount       int
	lastErrorTime    time.Time
	circuitBreakerMu sync.Mutex

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Logger
	Logger zerolog.Logger

	// Background tasks
	wg sync.WaitGroup
}

// NewClient creates a new MAX client
func NewClient(deviceID string, logger zerolog.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		DeviceID:    deviceID,
		pending:     make(map[int]chan *Response),
		fileWaiters: make(map[int64]chan *Response),
		users:       make(map[int64]*User),
		ctx:         ctx,
		cancel:      cancel,
		Logger:      logger,
	}
}

// SetEventHandler sets the event handler for notifications
func (c *Client) SetEventHandler(handler func(Event)) {
	c.eventHandler = handler
}

// IsConnected returns whether the client is connected
func (c *Client) IsConnected() bool {
	c.isConnectedMu.RLock()
	defer c.isConnectedMu.RUnlock()
	return c.isConnected
}

// setConnected sets the connection status
func (c *Client) setConnected(connected bool) {
	c.isConnectedMu.Lock()
	defer c.isConnectedMu.Unlock()
	c.isConnected = connected
}

// Connect establishes a WebSocket connection to the MAX server
func (c *Client) Connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	// If there's a dead connection (conn exists but not connected), close it first
	if c.conn != nil && !c.IsConnected() {
		c.Logger.Info().Msg("Closing dead connection before reconnect")
		c.conn.Close()
		c.conn = nil
	}

	if c.conn != nil {
		return nil // Already connected
	}

	// Create new context if the old one was cancelled (after Close())
	select {
	case <-c.ctx.Done():
		c.ctx, c.cancel = context.WithCancel(context.Background())
		c.Logger.Debug().Msg("Created new context for reconnect")
	default:
	}

	c.Logger.Info().Str("uri", WebSocketURI).Msg("Connecting to MAX WebSocket")

	dialer := websocket.Dialer{
		HandshakeTimeout: DefaultTimeout,
	}

	header := http.Header{}
	header.Set("Origin", WebSocketOrigin)
	header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	conn, _, err := dialer.Dial(WebSocketURI, header)
	if err != nil {
		c.Logger.Error().Err(err).Msg("Failed to connect to WebSocket")
		return err
	}

	c.conn = conn
	c.setConnected(true)

	// Start receive loop
	c.wg.Add(1)
	go c.receiveLoop()

	c.Logger.Info().Msg("WebSocket connected")
	return nil
}

// Close closes the client connection
func (c *Client) Close() error {
	c.Logger.Info().Msg("Closing client")

	c.cancel()
	c.setConnected(false)

	c.connMu.Lock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.connMu.Unlock()

		// Wait for goroutines to finish
		c.wg.Wait()

		// Clear pending requests
		c.pendingMu.Lock()
		for seq, ch := range c.pending {
			close(ch)
			delete(c.pending, seq)
		}
		c.pendingMu.Unlock()

		return err
	}
	c.connMu.Unlock()

	return nil
}

// Disconnect disconnects without clearing auth data
func (c *Client) Disconnect() error {
	return c.Close()
}

// nextSeq returns the next sequence number
func (c *Client) nextSeq() int {
	return int(atomic.AddInt32(&c.seq, 1))
}

// sendAndWait sends a message and waits for response
func (c *Client) sendAndWait(opcode Opcode, payload interface{}) (*Response, error) {
	return c.sendAndWaitWithTimeout(opcode, payload, DefaultTimeout)
}

// sendAndWaitWithTimeout sends a message and waits for response with custom timeout
func (c *Client) sendAndWaitWithTimeout(opcode Opcode, payload interface{}, timeout time.Duration) (*Response, error) {
	if !c.IsConnected() {
		return nil, ErrNotConnected
	}

	seq := c.nextSeq()

	// Create response channel
	respCh := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[seq] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, seq)
		c.pendingMu.Unlock()
	}()

	// Build message
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	msg := BaseMessage{
		Ver:     ProtocolVersion,
		Cmd:     0,
		Seq:     seq,
		Opcode:  int(opcode),
		Payload: payloadBytes,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	c.Logger.Debug().
		Int("seq", seq).
		Int("opcode", int(opcode)).
		Msg("Sending message")

	// Send message
	c.connMu.RLock()
	if c.conn == nil {
		c.connMu.RUnlock()
		return nil, ErrNotConnected
	}
	err = c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.connMu.RUnlock()

	if err != nil {
		c.Logger.Error().Err(err).Int("seq", seq).Msg("Failed to send message")
		return nil, err
	}

	// Wait for response
	select {
	case resp := <-respCh:
		if resp == nil {
			return nil, ErrNotConnected
		}

		// Check for error in response
		if err := ParseError(resp.Payload); err != nil {
			// Log detailed error information for debugging
			c.Logger.Error().
				Err(err).
				Int("opcode", resp.Opcode).
				Int("seq", resp.Seq).
				Interface("payload", resp.Payload).
				Msg("Server returned error")
			return resp, err
		}

		return resp, nil
	case <-time.After(timeout):
		return nil, ErrTimeout
	case <-c.ctx.Done():
		return nil, ErrNotConnected
	}
}

// receiveLoop handles incoming WebSocket messages
func (c *Client) receiveLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.connMu.RLock()
		conn := c.conn
		c.connMu.RUnlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.Logger.Info().Msg("WebSocket closed normally")
			} else {
				c.Logger.Error().Err(err).Msg("WebSocket read error")
			}
			c.setConnected(false)
			return
		}

		var resp Response
		if err := json.Unmarshal(message, &resp); err != nil {
			c.Logger.Warn().Err(err).Msg("Failed to parse message")
			continue
		}

		c.Logger.Debug().
			Int("seq", resp.Seq).
			Int("opcode", resp.Opcode).
			Msg("Received message")

		// Check if this is a response to a pending request
		c.pendingMu.RLock()
		respCh, ok := c.pending[resp.Seq]
		c.pendingMu.RUnlock()

		if ok {
			select {
			case respCh <- &resp:
			default:
			}
		} else {
			// Handle notification
			c.handleNotification(&resp)
		}
	}
}

// handleNotification handles server-initiated notifications
func (c *Client) handleNotification(resp *Response) {
	opcode := Opcode(resp.Opcode)

	// Handle file upload notifications
	if opcode == OpNotifAttach {
		c.handleFileAttachNotification(resp)
		return
	}

	// Build event
	event := Event{
		Opcode:  opcode,
		Payload: resp.Payload,
	}

	// Determine event type
	switch opcode {
	case OpNotifMessage:
		event.Type = c.determineMessageEventType(resp.Payload)
	case OpNotifMark:
		event.Type = "ReadReceipt"
	case OpNotifChat:
		event.Type = "ChatUpdate"
	case OpNotifTyping:
		event.Type = "Typing"
	case OpNotifMsgReactionsChanged:
		event.Type = "ReactionChange"
	case OpNotifContact:
		event.Type = "ContactUpdate"
	case OpNotifPresence:
		event.Type = "PresenceUpdate"
	case OpReconnect:
		event.Type = "Disconnected"
	case OpLogout:
		event.Type = "LoggedOut"
	default:
		event.Type = "Unknown"
	}

	if c.eventHandler != nil {
		c.eventHandler(event)
	}
}

// determineMessageEventType determines the type of message event
func (c *Client) determineMessageEventType(payload map[string]interface{}) string {
	message, ok := payload["message"].(map[string]interface{})
	if !ok {
		message = payload
	}

	status, _ := message["status"].(string)
	switch MessageStatus(status) {
	case MessageStatusEdited:
		return "MessageEdit"
	case MessageStatusRemoved:
		return "MessageDelete"
	default:
		return "Message"
	}
}

// handleFileAttachNotification handles file/video upload completion notifications
func (c *Client) handleFileAttachNotification(resp *Response) {
	c.fileWaitersMu.Lock()
	defer c.fileWaitersMu.Unlock()

	// Check for fileId
	if fileID, ok := resp.Payload["fileId"].(float64); ok {
		if ch, exists := c.fileWaiters[int64(fileID)]; exists {
			select {
			case ch <- resp:
			default:
			}
			delete(c.fileWaiters, int64(fileID))
			c.Logger.Debug().Int64("fileId", int64(fileID)).Msg("File upload completed")
		}
	}

	// Check for videoId
	if videoID, ok := resp.Payload["videoId"].(float64); ok {
		if ch, exists := c.fileWaiters[int64(videoID)]; exists {
			select {
			case ch <- resp:
			default:
			}
			delete(c.fileWaiters, int64(videoID))
			c.Logger.Debug().Int64("videoId", int64(videoID)).Msg("Video upload completed")
		}
	}
}

// registerFileWaiter registers a waiter for file upload completion
func (c *Client) registerFileWaiter(id int64) chan *Response {
	c.fileWaitersMu.Lock()
	defer c.fileWaitersMu.Unlock()

	ch := make(chan *Response, 1)
	c.fileWaiters[id] = ch
	return ch
}

// unregisterFileWaiter removes a file waiter
func (c *Client) unregisterFileWaiter(id int64) {
	c.fileWaitersMu.Lock()
	defer c.fileWaitersMu.Unlock()
	delete(c.fileWaiters, id)
}

// StartPingLoop starts the ping loop to keep connection alive
func (c *Client) StartPingLoop() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		ticker := time.NewTicker(PingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if !c.IsConnected() {
					return
				}

				_, err := c.sendAndWait(OpPing, map[string]interface{}{
					"interactive": true,
				})
				if err != nil {
					c.Logger.Warn().Err(err).Msg("Ping failed")
				} else {
					c.Logger.Debug().Msg("Ping successful")
				}
			}
		}
	}()
}

// GetCachedUser returns a user from cache
func (c *Client) GetCachedUser(userID int64) *User {
	c.usersMu.RLock()
	defer c.usersMu.RUnlock()
	return c.users[userID]
}

// cacheUser adds a user to cache
func (c *Client) cacheUser(user *User) {
	if user == nil {
		return
	}
	c.usersMu.Lock()
	defer c.usersMu.Unlock()
	c.users[user.ID] = user
}

// GetDialogID calculates the dialog ID between two users
func GetDialogID(userID1, userID2 int64) int64 {
	return userID1 ^ userID2
}
