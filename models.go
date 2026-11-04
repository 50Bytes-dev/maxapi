package main

// Request and response shapes of the API. static/api/spec.yml is the
// schema; TestSpecMatchesRoutes keeps its operations in step with routes.go.

// ========== BASE RESPONSE ==========

// ErrorResponse represents an error response
type ErrorResponse struct {
	Success bool   `json:"success" example:"false"`
	Error   string `json:"error" example:"error message"`
	Code    string `json:"code,omitempty" example:"INVALID_INPUT"`
}

// MessageResponse represents a simple success response with message
type MessageResponse struct {
	Success bool   `json:"success" example:"true"`
	Message string `json:"message" example:"Operation completed"`
}

// ========== SESSION RESPONSES ==========

// StatusResponse represents the connection status response
type StatusResponse struct {
	Success       bool   `json:"success" example:"true"`
	Connected     bool   `json:"connected" example:"true"`
	Authenticated bool   `json:"authenticated" example:"true"`
	LoggedIn      bool   `json:"loggedIn" example:"true"`
	MaxUserID     int64  `json:"maxUserID" example:"123456789"`
	Paused        bool   `json:"paused" example:"false"`
	PauseReason   string `json:"pauseReason,omitempty" example:"spam"`
	UpstreamCode  string `json:"upstreamCode,omitempty" example:"error.user.restricted.send"`
}

// ResumeResponse is returned by POST /session/resume.
type ResumeResponse struct {
	Success   bool  `json:"success" example:"true"`
	MaxUserID int64 `json:"maxUserID" example:"123456789"`
}

// ========== CHAT RESPONSES ==========

// SendMessageResponse represents the response after sending a message
type SendMessageResponse struct {
	Success   bool  `json:"success" example:"true"`
	MessageID int64 `json:"messageId" example:"987654321"`
	ChatID    int64 `json:"chatId,omitempty" example:"123456789"`
}

// DownloadMediaResponse represents the response for downloading media
type DownloadMediaResponse struct {
	Success  bool   `json:"success" example:"true"`
	Data     string `json:"data" example:"base64_encoded_data"`
	MimeType string `json:"mimeType" example:"image/jpeg"`
}

// DownloadVideoResponse represents the response for downloading video
type DownloadVideoResponse struct {
	Success  bool   `json:"success" example:"true"`
	Data     string `json:"data" example:"base64_encoded_data"`
	MimeType string `json:"mimeType" example:"video/mp4"`
	URL      string `json:"url" example:"https://example.com/video.mp4"`
}

// ChatHistoryResponse represents the response for chat history
type ChatHistoryResponse struct {
	Success  bool                     `json:"success" example:"true"`
	Messages []map[string]interface{} `json:"messages"`
}

// ========== USER RESPONSES ==========

// CheckUserResultItem represents a single user check result
type CheckUserResultItem struct {
	Phone     string `json:"phone" example:"79001234567"`
	Exists    bool   `json:"exists" example:"true"`
	MaxUserID int64  `json:"maxUserId" example:"123456789"`
	Name      string `json:"name,omitempty" example:"John Doe"`
}

// CheckUserResponse represents the response for checking users
type CheckUserResponse struct {
	Success bool                  `json:"success" example:"true"`
	Users   []CheckUserResultItem `json:"users"`
}

// UserInfoResponse represents the response for getting user info
type UserInfoResponse struct {
	Success bool                     `json:"success" example:"true"`
	Users   []map[string]interface{} `json:"users"`
	Count   int                      `json:"count" example:"1"`
}

// ContactsResponse represents the response for getting contacts
type ContactsResponse struct {
	Success  bool                     `json:"success" example:"true"`
	Contacts []map[string]interface{} `json:"contacts"`
	Count    int                      `json:"count" example:"42"`
}

// ========== GROUP RESPONSES ==========

// GroupChatResponse represents the response with group/chat info
type GroupChatResponse struct {
	Success bool                   `json:"success" example:"true"`
	Chat    map[string]interface{} `json:"chat"`
}

// InviteLinkResponse represents the response with invite link
type InviteLinkResponse struct {
	Success    bool   `json:"success" example:"true"`
	InviteLink string `json:"inviteLink" example:"https://max.ru/join/abc123"`
}

// ========== WEBHOOK RESPONSES ==========

// WebhookResponse represents the response for webhook operations
type WebhookResponse struct {
	Success bool   `json:"success" example:"true"`
	Webhook string `json:"webhook" example:"https://example.com/webhook"`
}

// ========== ADMIN RESPONSES ==========

// AddUserResponse represents the response for adding a user
type AddUserResponse struct {
	Success bool   `json:"success" example:"true"`
	ID      string `json:"id" example:"a7e5dd6b-8b3e-4035-ba87-3f96a0e3f5c0"`
	Token   string `json:"token" example:"abc123def456"`
	Name    string `json:"name" example:"John Doe"`
}

// ListUsersResponse represents the response for listing users
type ListUsersResponse struct {
	Success bool           `json:"success" example:"true"`
	Data    []UserResponse `json:"data"`
}

// QRCodeResponse is returned by GET /session/qr.
type QRCodeResponse struct {
	Success bool   `json:"success" example:"true"`
	Qrcode  string `json:"qrcode" example:"data:image/png;base64,..."`
}

// ConnectBody represents the request body for connect
type ConnectBody struct {
	Subscribe []string `json:"subscribe" example:"Message,ReadReceipt"`
	Immediate bool     `json:"immediate" example:"false"`
}

// ResumeBody is the optional body of POST /session/resume. captchaKey is
// part of the VK gateway's contract; MAX pauses have no captcha, so it is
// read and ignored.
type ResumeBody struct {
	CaptchaKey string `json:"captchaKey,omitempty" example:""`
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
	UserIDs []int64 `json:"userIds"`
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

// UserResponse represents a user row returned by GET /admin/users.
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

// UserDetail is the enriched shape returned by GET /admin/users/{userid}.
// Secrets (auth_token, s3 keys, proxy password) are never returned raw:
// `auth_configured` / `proxy_configured` expose presence as a boolean and
// `proxy_url` is emitted with the password replaced by ***.
type UserDetail struct {
	ID              string   `json:"id" example:"a7e5dd6b-8b3e-4035-ba87-3f96a0e3f5c0"`
	Name            string   `json:"name" example:"John Doe"`
	Token           string   `json:"token" example:"abc123def456"`
	Webhook         string   `json:"webhook" example:"https://example.com/webhook"`
	MaxUserID       int64    `json:"max_user_id" example:"123456789"`
	DeviceID        string   `json:"device_id" example:"6e8ed92f-..."`
	Connected       bool     `json:"connected" example:"true"`
	ConnectedFlag   bool     `json:"connected_flag" example:"true"`
	AuthConfigured  bool     `json:"auth_configured" example:"true"`
	Events          []string `json:"events" example:"Message,ReadReceipt"`
	ProxyConfigured bool     `json:"proxy_configured" example:"false"`
	ProxyURL        string   `json:"proxy_url" example:"http://user:***@proxy.internal:3128"`
	History         int      `json:"history" example:"100"`
	S3Enabled       bool     `json:"s3_enabled" example:"false"`
	S3Endpoint      string   `json:"s3_endpoint" example:""`
	S3Region        string   `json:"s3_region" example:""`
	S3Bucket        string   `json:"s3_bucket" example:""`
	MediaDelivery   string   `json:"media_delivery" example:"base64"`
}

// UserDetailResponse wraps UserDetail in the standard response envelope.
type UserDetailResponse struct {
	Success bool       `json:"success" example:"true"`
	Data    UserDetail `json:"data"`
}

// HealthResponse reports liveness + readiness metadata.
type HealthResponse struct {
	Success        bool   `json:"success" example:"true"`
	Status         string `json:"status" example:"ok"`
	Version        string `json:"version" example:"1.5.1"`
	UptimeSeconds  int64  `json:"uptime_seconds" example:"12345"`
	DBOk           bool   `json:"db_ok" example:"true"`
	RabbitMQOk     bool   `json:"rabbitmq_ok" example:"true"`
	ConnectedUsers int    `json:"connected_users" example:"42"`
	Goroutines     int    `json:"goroutines" example:"87"`
	MemAllocMB     uint64 `json:"mem_alloc_mb" example:"67"`
	Timestamp      int64  `json:"timestamp" example:"1700000000"`
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
