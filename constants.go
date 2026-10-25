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

// Delivery outcome codes of POST /chat/send/text. Unlike the generic codes
// above they tell the caller whether the text may have reached the recipient,
// so it can retry without sending the same message twice:
//   - nothing was sent: not_connected, invalid_target, address_not_found,
//     lookup_failed, send_rejected;
//   - outcome unknown, MAX may have delivered it: send_failed.
//
// They are lowercase because they follow an external delivery contract. The
// contract also reserves address_refused (recipient's privacy settings or a
// block); MAX's error code for that is unknown, so it is not emitted yet.
const (
	CodeNotConnected    = "not_connected"     // no live MAX session
	CodeInvalidTarget   = "invalid_target"    // neither chatId nor phone given
	CodeAddressNotFound = "address_not_found" // phone malformed or unknown to MAX
	CodeLookupFailed    = "lookup_failed"     // phone lookup timed out or failed
	CodeSendRejected    = "send_rejected"     // MAX answered the send with an error
	CodeSendFailed      = "send_failed"       // no answer to the send, or session lost after it
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
