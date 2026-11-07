package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
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

	// MaxSeq is the largest sequence number MAX accepts. The server parses
	// `seq` as a signed 16-bit integer: anything above 32767 makes it drop the
	// connection right after the frame, with no close code (client sees
	// "close 1006 unexpected EOF"). Once a long-lived client crossed 32767 it
	// could never reconnect again, so the counter wraps back to 1 instead.
	MaxSeq = 32767

	// Default timeouts
	DefaultTimeout    = 30 * time.Second
	WriteTimeout      = 10 * time.Second
	ReconnectDelay    = 1 * time.Second
	MaxReconnectDelay = 60 * time.Second

	// Circuit breaker
	MaxConsecutiveErrors = 10
	CircuitBreakerReset  = 60 * time.Second
)

// PingInterval is how often a session pings MAX.
const PingInterval = 30 * time.Second

// pingInterval is PingInterval, which tests shorten (SetPingInterval).
var pingInterval atomic.Int64

func init() { pingInterval.Store(int64(PingInterval)) }

// SetPingInterval sets how often clients started from now on ping MAX. For
// tests.
func SetPingInterval(d time.Duration) { pingInterval.Store(int64(d)) }

// Client represents a MAX API client
type Client struct {
	// URI is the WebSocket endpoint to dial; empty means WebSocketURI.
	// Tests point it at a local fake server.
	URI string

	// LoginTimeout bounds the wait for MAX's answer to LOGIN; zero means
	// DefaultTimeout. Tests shorten it.
	LoginTimeout time.Duration

	// Connection
	conn   *websocket.Conn
	connMu sync.RWMutex

	// writeMu serializes frame writes. gorilla/websocket supports one
	// concurrent writer and panics on a second one, which takes the whole
	// gateway, and every account's session with it, down.
	writeMu sync.Mutex

	// Authentication
	DeviceID  string
	AuthToken string
	MaxUserID int64
	Me        *Me

	// State
	seq           int32
	lastCID       int64
	isConnected   bool
	isConnectedMu sync.RWMutex

	// Pending requests, by seq
	pending   map[int]pendingRequest
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

	// Context for cancellation: of the current connection. Close cancels
	// it and Connect makes a new one, so a client may connect again.
	ctx    context.Context
	cancel context.CancelFunc

	// life is cancelled once, by Shutdown, and never renewed: a shut down
	// client never connects again (see Shutdown).
	life       context.Context
	lifeCancel context.CancelFunc
	shut       atomic.Bool

	// Logger
	Logger zerolog.Logger

	// Background tasks
	wg sync.WaitGroup
}

// NewClient creates a new MAX client
func NewClient(deviceID string, logger zerolog.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	life, lifeCancel := context.WithCancel(context.Background())
	return &Client{
		DeviceID:    deviceID,
		pending:     make(map[int]pendingRequest),
		fileWaiters: make(map[int64]chan *Response),
		users:       make(map[int64]*User),
		ctx:         ctx,
		cancel:      cancel,
		life:        life,
		lifeCancel:  lifeCancel,
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
	if c.shut.Load() {
		return ErrClientShutDown
	}
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

	uri := c.URI
	if uri == "" {
		uri = WebSocketURI
	}

	c.Logger.Info().Str("uri", uri).Msg("Connecting to MAX WebSocket")

	dialer := websocket.Dialer{
		HandshakeTimeout: DefaultTimeout,
		WriteBufferSize:  wsWriteBufferSize,
	}

	header := http.Header{}
	header.Set("Origin", WebSocketOrigin)
	header.Set("User-Agent", WebUserAgent)

	// The dial ends at Shutdown: a client shut down while it dials never
	// gets a connection.
	conn, _, err := dialer.DialContext(c.life, uri, header)
	if err != nil {
		if c.shut.Load() {
			return ErrClientShutDown
		}
		c.Logger.Error().Err(err).Msg("Failed to connect to WebSocket")
		return err
	}
	if c.shut.Load() {
		conn.Close()
		return ErrClientShutDown
	}

	c.conn = conn
	c.setConnected(true)

	// Every connection is a fresh sequence, the same way the web client does
	// it. Carrying the counter over from the previous socket is what used to
	// push a reconnecting client past MaxSeq and lock it out for good.
	atomic.StoreInt32(&c.seq, 0)

	// Start receive loop
	c.wg.Add(1)
	go c.receiveLoop(c.ctx)

	c.Logger.Info().Msg("WebSocket connected")
	return nil
}

// Close closes the client connection and waits for its goroutines. The
// client may Connect again afterwards. It must not run on the client's own
// receive goroutine (an event handler): it would wait for itself. Shutdown
// may.
func (c *Client) Close() error {
	c.Logger.Info().Msg("Closing client")

	c.connMu.Lock()
	c.cancel()
	c.setConnected(false)
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.connMu.Unlock()

		// Wait for goroutines to finish
		c.wg.Wait()

		// Clear pending requests
		c.pendingMu.Lock()
		for seq, p := range c.pending {
			close(p.ch)
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

// Shutdown ends the client for good: the connection is closed, a dial in
// progress is abandoned, requests waiting for an answer fail, and the ping
// and receive goroutines end. Every later Connect answers ErrClientShutDown
// and every request ErrNotConnected, so no frame reaches MAX once Shutdown
// has returned. It never waits for the client's goroutines, so it is safe
// anywhere, an event handler included, and more than once.
func (c *Client) Shutdown() {
	if c.shut.Swap(true) {
		return
	}
	c.Logger.Info().Msg("Shutting the client down")
	c.lifeCancel()

	c.connMu.Lock()
	c.cancel()
	c.setConnected(false)
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()

	c.pendingMu.Lock()
	for seq, p := range c.pending {
		close(p.ch)
		delete(c.pending, seq)
	}
	c.pendingMu.Unlock()
}

// IsShutDown reports whether Shutdown was called.
func (c *Client) IsShutDown() bool {
	return c.shut.Load()
}

// nextSeq returns the next sequence number, wrapping back to 1 once MaxSeq is
// reached. A connection that stays up long enough (one frame every 30s ping
// takes ~11 days) would otherwise walk past what MAX can parse.
func (c *Client) nextSeq() int {
	for {
		cur := atomic.LoadInt32(&c.seq)
		next := cur + 1
		if next > MaxSeq {
			next = 1
		}
		if atomic.CompareAndSwapInt32(&c.seq, cur, next) {
			return int(next)
		}
	}
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
	c.pending[seq] = pendingRequest{ch: respCh, opcode: int(opcode)}
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
	if len(msgBytes) > wsWriteBufferSize {
		return nil, ErrFrameTooLarge
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
	connCtx := c.ctx
	err = c.writeFrame(msgBytes)
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
			// The payload goes to the log with every token masked: an error
			// answer may echo the request.
			c.Logger.Error().
				Err(err).
				Int("opcode", resp.Opcode).
				Int("seq", resp.Seq).
				Str("payload", payloadForLog(resp.Payload)).
				Msg("Server returned error")
			return resp, err
		}

		return resp, nil
	case <-time.After(timeout):
		return nil, ErrTimeout
	case <-connCtx.Done():
		return nil, ErrNotConnected
	case <-c.life.Done():
		return nil, ErrNotConnected
	}
}

// wsWriteBufferSize is the WebSocket's write buffer, and so the longest
// request written as one frame. gorilla/websocket splits a longer message
// into continuation frames, and MAX closes the connection on them: live on
// 2026-11-05 every request over the default 4 KiB (a text of 4000 Latin
// letters) dropped the session. 64 KiB holds the longest request the gateway
// makes (4000 UTF-16 units of text, escaped, and 10 attachments); a longer
// one is refused with ErrFrameTooLarge, never written.
const wsWriteBufferSize = 64 << 10

// writeFrame writes one frame to the connection. Every writer goes through
// it: requests from HTTP handlers and the ping goroutine run concurrently.
// The deadline keeps a stuck write from holding writeMu, and with it every
// other request, forever. The caller holds connMu for reading.
func (c *Client) writeFrame(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// receiveLoop handles incoming WebSocket messages
func (c *Client) receiveLoop(ctx context.Context) {
	defer c.wg.Done()

	for {
		select {
		case <-ctx.Done():
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
			switch {
			case isExpectedCloseErr(err, ctx):
				c.Logger.Debug().Err(err).Msg("WebSocket closed")
			case websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway):
				c.Logger.Info().Msg("WebSocket closed normally")
			default:
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
		var frame struct {
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(message, &frame) == nil {
			resp.RawPayload = frame.Payload
		}

		c.Logger.Debug().
			Int("seq", resp.Seq).
			Int("opcode", resp.Opcode).
			Msg("Received message")

		// A frame MAX sends of its own accord has cmd 0, an answer cmd 1 (or
		// 3, an error) with the seq and opcode of the request. MAX numbers
		// its own frames by a counter of its own, so a seq alone may be a
		// pending request's: such a push used to be taken for the answer.
		if resp.Cmd == cmdRequest {
			c.handleNotification(&resp)
			continue
		}
		c.pendingMu.RLock()
		p, ok := c.pending[resp.Seq]
		c.pendingMu.RUnlock()
		if !ok || p.opcode != resp.Opcode {
			c.Logger.Debug().Int("seq", resp.Seq).Int("opcode", resp.Opcode).Msg("An answer no request waits for")
			continue
		}
		select {
		case p.ch <- &resp:
		default:
		}
	}
}

// pendingRequest is a request waiting for MAX's answer.
type pendingRequest struct {
	ch     chan *Response
	opcode int
}

// cmdRequest is the cmd of a frame that asks: every request of the client,
// and every frame MAX pushes. MAX answers with 1, or 3 for an error.
const cmdRequest = 0

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
		c.logPushedFrame(resp, "MAX told the client to reconnect")
	case OpLogout:
		event.Type = "LoggedOut"
		c.logPushedFrame(resp, "MAX ended the session")
	default:
		event.Type = "Unknown"
		c.logUnknownFrame(resp)
	}

	if c.eventHandler != nil {
		c.eventHandler(event)
	}
}

// maxLoggedPayload caps the payload logPushedFrame writes, in bytes.
const maxLoggedPayload = 2048

// logPushedFrame logs, at Info, a frame MAX pushed on its own: its payload is
// the only record of why MAX dropped or ended a session. The payload is
// truncated to maxLoggedPayload bytes, and the value of every key naming a
// token is masked (a login token travels as "token", QR confirmation hands it
// out under "tokenAttrs").
func (c *Client) logPushedFrame(resp *Response, msg string) {
	c.Logger.Info().
		Int("opcode", resp.Opcode).
		Int("seq", resp.Seq).
		Str("payload", payloadForLog(resp.Payload)).
		Msg(msg)
}

// logUnknownFrame logs, at Info, a frame of an opcode the client does not
// handle: its opcode and the names of its payload keys, never the values.
// Unknown opcodes carry chat content too (drafts, for one), and the log is no
// place for the owner's correspondence.
func (c *Client) logUnknownFrame(resp *Response) {
	keys := make([]string, 0, len(resp.Payload))
	for k := range resp.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c.Logger.Info().
		Int("opcode", resp.Opcode).
		Int("seq", resp.Seq).
		Strs("payloadKeys", keys).
		Msg("Unknown frame from MAX")
}

// payloadForLog renders a frame payload for the log: tokens masked, at most
// maxLoggedPayload bytes.
func payloadForLog(payload map[string]interface{}) string {
	data, err := json.Marshal(maskTokens(payload))
	if err != nil {
		return "(unencodable payload)"
	}
	if len(data) > maxLoggedPayload {
		return strings.ToValidUTF8(string(data[:maxLoggedPayload]), "") + "...(truncated)"
	}
	return string(data)
}

// maskTokens returns a copy of v in which the value under every key that
// contains "token" or "password", in any case, is replaced by "***".
func maskTokens(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, val := range x {
			if lower := strings.ToLower(k); strings.Contains(lower, "token") || strings.Contains(lower, "password") {
				out[k] = "***"
			} else {
				out[k] = maskTokens(val)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, val := range x {
			out[i] = maskTokens(val)
		}
		return out
	}
	return v
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
	c.connMu.RLock()
	ctx := c.ctx
	c.connMu.RUnlock()
	interval := time.Duration(pingInterval.Load())
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-c.life.Done():
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

// isExpectedCloseErr reports whether the read error is a consequence of our own
// Close() or a graceful EOF. Such errors are noisy — we want Debug-level logging
// rather than Error.
func isExpectedCloseErr(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Fast path when Close() was called: ctx is cancelled before the read unwinds.
	if ctx != nil {
		select {
		case <-ctx.Done():
			return true
		default:
		}
	}
	// Stringly-typed fallback for older wrapped errors that don't implement Is.
	return strings.Contains(err.Error(), "use of closed network connection")
}
