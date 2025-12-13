package main

// List of supported event types for MAX Messenger
var supportedEventTypes = []string{
	// Messages
	"Message",       // NOTIF_MESSAGE (128) - new/incoming message
	"MessageEdit",   // NOTIF_MESSAGE + status=EDITED
	"MessageDelete", // NOTIF_MESSAGE + status=REMOVED

	// Read receipts
	"ReadReceipt", // NOTIF_MARK (130)

	// Connection
	"Connected",    // Successful LOGIN (deprecated, use Sync)
	"Disconnected", // WebSocket closed / RECONNECT (3)
	"Reconnecting", // Attempting to reconnect
	"Sync",         // Synchronization data on connect/reconnect
	"LoggedOut",    // Session terminated (from MAX app or API)

	// Authentication
	"AuthCodeSent", // Auth code sent (new)

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
