package main

// Swagger model definitions for API documentation

// Response represents a standard API response
// @Description Standard API response format
type Response struct {
	Code    int         `json:"code" example:"200"`
	Success bool        `json:"success" example:"true"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// AuthRequestBody represents the request body for SMS code request
type AuthRequestBody struct {
	Phone    string `json:"phone" example:"79001234567"`
	Language string `json:"language" example:"ru"`
}

// AuthConfirmBody represents the request body for SMS code confirmation
type AuthConfirmBody struct {
	Code string `json:"code" example:"123456"`
}

// AuthRegisterBody represents the request body for user registration
type AuthRegisterBody struct {
	FirstName string `json:"firstName" example:"John"`
	LastName  string `json:"lastName" example:"Doe"`
}

// ConnectBody represents the request body for connect
type ConnectBody struct {
	Subscribe []string `json:"subscribe" example:"Message,ReadReceipt"`
	Immediate bool     `json:"immediate" example:"false"`
}

// MessageBody represents the request body for sending a text message
type MessageBody struct {
	ChatID  int64  `json:"chatId" example:"123456789"`
	Phone   string `json:"phone" example:"79001234567"`
	Text    string `json:"text" example:"Hello, World!"`
	ReplyTo int64  `json:"replyTo" example:"0"`
	Notify  bool   `json:"notify" example:"true"`
}

// EditMessageBody represents the request body for editing a message
type EditMessageBody struct {
	ChatID    int64  `json:"chatId" example:"123456789"`
	MessageID int64  `json:"messageId" example:"987654321"`
	Text      string `json:"text" example:"Updated message"`
}

// MarkReadBody represents the request body for marking messages as read
type MarkReadBody struct {
	ChatID    int64 `json:"chatId" example:"123456789"`
	MessageID int64 `json:"messageId" example:"987654321"`
}

// DeleteMessageBody represents the request body for deleting messages
type DeleteMessageBody struct {
	ChatID     int64   `json:"chatId" example:"123456789"`
	MessageIDs []int64 `json:"messageIds"`
	ForMe      bool    `json:"forMe" example:"false"`
}

// ImageBody represents the request body for sending an image
type ImageBody struct {
	ChatID  int64  `json:"chatId" example:"123456789"`
	Phone   string `json:"phone" example:"79001234567"`
	Image   string `json:"image" example:"data:image/jpeg;base64,..."`
	Caption string `json:"caption" example:"Image caption"`
	Notify  bool   `json:"notify" example:"true"`
}

// DocumentBody represents the request body for sending a document
type DocumentBody struct {
	ChatID   int64  `json:"chatId" example:"123456789"`
	Phone    string `json:"phone" example:"79001234567"`
	Document string `json:"document" example:"data:application/pdf;base64,..."`
	FileName string `json:"fileName" example:"document.pdf"`
	Caption  string `json:"caption" example:"Document caption"`
	Notify   bool   `json:"notify" example:"true"`
}

// AudioBody represents the request body for sending audio
type AudioBody struct {
	ChatID   int64  `json:"chatId" example:"123456789"`
	Phone    string `json:"phone" example:"79001234567"`
	Audio    string `json:"audio" example:"data:audio/mp3;base64,..."`
	FileName string `json:"fileName" example:"audio.mp3"`
	Notify   bool   `json:"notify" example:"true"`
}

// VideoBody represents the request body for sending a video
type VideoBody struct {
	ChatID   int64  `json:"chatId" example:"123456789"`
	Phone    string `json:"phone" example:"79001234567"`
	Video    string `json:"video" example:"data:video/mp4;base64,..."`
	Caption  string `json:"caption" example:"Video caption"`
	FileName string `json:"fileName" example:"video.mp4"`
	Notify   bool   `json:"notify" example:"true"`
}

// CheckUserBody represents the request body for checking users
type CheckUserBody struct {
	Phone []string `json:"phone"`
}

// UserInfoBody represents the request body for getting user info
type UserInfoBody struct {
	UserID int64 `json:"userId" example:"123456789"`
}

// PresenceBody represents the request body for sending presence
type PresenceBody struct {
	ChatID int64 `json:"chatId" example:"123456789"`
}

// CreateGroupBody represents the request body for creating a group
type CreateGroupBody struct {
	Name         string  `json:"name" example:"My Group"`
	Participants []int64 `json:"participants"`
}

// GroupInfoBody represents the request body for group operations
type GroupInfoBody struct {
	ChatID int64 `json:"chatId" example:"123456789"`
}

// GroupJoinBody represents the request body for joining a group
type GroupJoinBody struct {
	Link string `json:"link" example:"https://max.ru/join/abc123"`
}

// UpdateParticipantsBody represents the request body for updating group participants
type UpdateParticipantsBody struct {
	ChatID    int64   `json:"chatId" example:"123456789"`
	UserIDs   []int64 `json:"userIds"`
	Operation string  `json:"operation" example:"add" enums:"add,remove"`
}

// GroupNameBody represents the request body for setting group name
type GroupNameBody struct {
	ChatID int64  `json:"chatId" example:"123456789"`
	Name   string `json:"name" example:"New Group Name"`
}

// GroupTopicBody represents the request body for setting group topic
type GroupTopicBody struct {
	ChatID int64  `json:"chatId" example:"123456789"`
	Topic  string `json:"topic" example:"Group description"`
}

// WebhookBody represents the request body for setting webhook
type WebhookBody struct {
	Webhook string `json:"webhook" example:"https://example.com/webhook"`
}

// ChatHistoryBody represents the request body for getting chat history
type ChatHistoryBody struct {
	ChatID   int64 `json:"chatId" example:"123456789"`
	Count    int   `json:"count" example:"50"`
	FromTime int64 `json:"fromTime" example:"0"`
}

// ReactBody represents the request body for adding a reaction
type ReactBody struct {
	ChatID    int64  `json:"chatId" example:"123456789"`
	MessageID string `json:"messageId" example:"987654321"`
	Reaction  string `json:"reaction" example:"👍"`
}

// DownloadBody represents the request body for downloading media
type DownloadBody struct {
	URL string `json:"url" example:"https://example.com/image.jpg"`
}

// DownloadFileBody represents the request body for downloading files
type DownloadFileBody struct {
	ChatID    int64 `json:"chatId" example:"123456789"`
	MessageID int64 `json:"messageId" example:"987654321"`
	FileID    int64 `json:"fileId" example:"111222333"`
	VideoID   int64 `json:"videoId" example:"111222333"`
}

// UserResponse represents a user in the system
type UserResponse struct {
	ID            string `json:"id" example:"a7e5dd6b-8b3e-4035-ba87-3f96a0e3f5c0"`
	Name          string `json:"name" example:"John Doe"`
	Token         string `json:"token" example:"abc123def456"`
	MaxUserID     *int64 `json:"maxUserId" example:"123456789"`
	Webhook       string `json:"webhook" example:"https://example.com/webhook"`
	Events        string `json:"events" example:"All"`
	Connected     int    `json:"connected" example:"1"`
	Authenticated bool   `json:"authenticated" example:"true"`
}

// AddUserBody represents the request body for adding a user
type AddUserBody struct {
	Name    string `json:"name" example:"John Doe"`
	Webhook string `json:"webhook" example:"https://example.com/webhook"`
	Events  string `json:"events" example:"All"`
}

// EditUserBody represents the request body for editing a user
type EditUserBody struct {
	Name    string `json:"name" example:"John Doe"`
	Webhook string `json:"webhook" example:"https://example.com/webhook"`
	Events  string `json:"events" example:"All"`
}
