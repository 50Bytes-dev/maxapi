package main

// Tunables for MAX WebSocket reconnect loop. Overridable via -maxconnect* flags
// or MAX_CONNECT_* environment variables.
const (
	defaultConnectRetryDelay = 5   // seconds, first backoff wait
	defaultConnectRetryCap   = 300 // seconds, max backoff wait per attempt
	defaultConnectMaxRetries = 0   // 0 = retry forever
)

// Standardised error codes returned in JSON error responses. Consumers should
// branch on `code`; `error` is a human-readable description that may evolve.
const (
	ErrInvalidInput    = "INVALID_INPUT"
	ErrNotConnected    = "NOT_CONNECTED"
	ErrInternalFailure = "INTERNAL_ERROR"
	ErrAuthExpired     = "AUTH_EXPIRED"
	ErrNotFound        = "NOT_FOUND"
	ErrForbidden       = "FORBIDDEN"
	ErrUnauthorized    = "UNAUTHORIZED"
)

// List of supported event types for MAX Messenger
var supportedEventTypes = []string{
	// Messages
	"Message",       // NOTIF_MESSAGE (128) - new/incoming message
	"MessageEdit",   // NOTIF_MESSAGE + status=EDITED
	"MessageDelete", // NOTIF_MESSAGE + status=REMOVED

	// Read receipts
	"ReadReceipt", // NOTIF_MARK (130)

	// Connection
	"Connected",          // Successful LOGIN (deprecated, use Sync)
	"Disconnected",       // WebSocket closed / RECONNECT (3)
	"Reconnecting",       // Attempting to reconnect
	"ReconnectExhausted", // Gave up after configured retry cap
	"Sync",               // Synchronization data on connect/reconnect
	"LoggedOut",          // Session terminated (from MAX app or API)

	// Authentication
	"QRGenerated",  // QR session created or refreshed, qrLink issued (carries refreshed code too)
	"QRScanned",    // User scanned QR in mobile app (loginAvailable=true)
	"QRAuthorized", // Auth token received; server auto-starts MAX session (Sync follows)
	"QRExpired",    // Terminal — QR refresh budget exhausted or session abandoned
	"AuthExpired",  // Auth token expired/invalid - need to re-authenticate

	// Chats and groups
	"ChatUpdate", // NOTIF_CHAT (135)
	"Typing",     // NOTIF_TYPING (129)

	// Reactions
	"ReactionChange", // NOTIF_MSG_REACTIONS_CHANGED (155)

	// Contacts
	"ContactUpdate",  // NOTIF_CONTACT (131)
	"PresenceUpdate", // NOTIF_PRESENCE (132)

	// Files
	"FileReady", // NOTIF_ATTACH (136)

	// Synchronization
	"HistorySync", // After CHAT_HISTORY

	// Special - receives all events
	"All",
}

// Map for quick validation
var eventTypeMap map[string]bool

func init() {
	eventTypeMap = make(map[string]bool)
	for _, eventType := range supportedEventTypes {
		eventTypeMap[eventType] = true
	}
}

// Auxiliary function to validate event type
func isValidEventType(eventType string) bool {
	return eventTypeMap[eventType]
}
