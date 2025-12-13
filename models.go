package main

// Swagger model definitions for API documentation

// ========== BASE RESPONSE ==========

// ErrorResponse represents an error response
// @Description Error response format
type ErrorResponse struct {
	Success bool   `json:"success" example:"false"`
	Error   string `json:"error" example:"error message"`
}

// MessageResponse represents a simple success response with message
// @Description Simple success response with message
type MessageResponse struct {
	Success bool   `json:"success" example:"true"`
	Message string `json:"message" example:"Operation completed"`
}

// ========== AUTH RESPONSES ==========

// AuthRequestResponse represents the response for auth code request
// @Description Response after requesting SMS verification code
type AuthRequestResponse struct {
	Success   bool   `json:"success" example:"true"`
	Message   string `json:"message" example:"Verification code sent"`
	TempToken string `json:"tempToken" example:"temp_token_value"`
}

// AuthConfirmResponse represents the response for auth code confirmation
// @Description Response after confirming SMS verification code
type AuthConfirmResponse struct {
	Success              bool   `json:"success" example:"true"`
	Message              string `json:"message" example:"Login successful"`
	AuthToken            string `json:"authToken,omitempty" example:"auth_token_value"`
	RegisterToken        string `json:"registerToken,omitempty" example:"register_token_value"`
	RequiresRegistration bool   `json:"requiresRegistration" example:"false"`
}

// AuthRegisterResponse represents the response for user registration
// @Description Response after successful registration
type AuthRegisterResponse struct {
	Success   bool   `json:"success" example:"true"`
	Message   string `json:"message" example:"Registration successful"`
	AuthToken string `json:"authToken" example:"auth_token_value"`
}

// ========== SESSION RESPONSES ==========

// StatusResponse represents the connection status response
// @Description Connection and authentication status
type StatusResponse struct {
	Success       bool  `json:"success" example:"true"`
	Connected     bool  `json:"connected" example:"true"`
	Authenticated bool  `json:"authenticated" example:"true"`
	LoggedIn      bool  `json:"loggedIn" example:"true"`
	MaxUserID     int64 `json:"maxUserID" example:"123456789"`
}

// ========== CHAT RESPONSES ==========

// SendMessageResponse represents the response after sending a message
// @Description Response after sending a message
type SendMessageResponse struct {
	Success   bool  `json:"success" example:"true"`
	MessageID int64 `json:"messageId" example:"987654321"`
	ChatID    int64 `json:"chatId,omitempty" example:"123456789"`
}

// DownloadMediaResponse represents the response for downloading media
// @Description Response with downloaded media data
type DownloadMediaResponse struct {
	Success  bool   `json:"success" example:"true"`
	Data     string `json:"data" example:"base64_encoded_data"`
	MimeType string `json:"mimeType" example:"image/jpeg"`
}

// DownloadVideoResponse represents the response for downloading video
// @Description Response with downloaded video data
type DownloadVideoResponse struct {
	Success  bool   `json:"success" example:"true"`
	Data     string `json:"data" example:"base64_encoded_data"`
	MimeType string `json:"mimeType" example:"video/mp4"`
	URL      string `json:"url" example:"https://example.com/video.mp4"`
}

// ChatHistoryResponse represents the response for chat history
// @Description Response with chat history messages
type ChatHistoryResponse struct {
	Success  bool                     `json:"success" example:"true"`
	Messages []map[string]interface{} `json:"messages"`
}

// ========== USER RESPONSES ==========

// CheckUserResultItem represents a single user check result
// @Description Single user check result
type CheckUserResultItem struct {
	Phone     string `json:"phone" example:"79001234567"`
	Exists    bool   `json:"exists" example:"true"`
	MaxUserID int64  `json:"maxUserId" example:"123456789"`
	Name      string `json:"name,omitempty" example:"John Doe"`
}

// CheckUserResponse represents the response for checking users
// @Description Response with user existence check results
type CheckUserResponse struct {
	Success bool                  `json:"success" example:"true"`
	Users   []CheckUserResultItem `json:"users"`
}

// UserInfoResponse represents the response for getting user info
// @Description Response with user information
type UserInfoResponse struct {
	Success bool                   `json:"success" example:"true"`
	User    map[string]interface{} `json:"user"`
}

// ContactsResponse represents the response for getting contacts
// @Description Response with list of contacts
type ContactsResponse struct {
	Success  bool                     `json:"success" example:"true"`
	Contacts []map[string]interface{} `json:"contacts"`
	Count    int                      `json:"count" example:"42"`
}

// ========== GROUP RESPONSES ==========

// GroupChatResponse represents the response with group/chat info
// @Description Response with group or chat information
type GroupChatResponse struct {
	Success bool                   `json:"success" example:"true"`
	Chat    map[string]interface{} `json:"chat"`
}

// InviteLinkResponse represents the response with invite link
// @Description Response with group invite link
type InviteLinkResponse struct {
	Success    bool   `json:"success" example:"true"`
	InviteLink string `json:"inviteLink" example:"https://max.ru/join/abc123"`
}

// ========== WEBHOOK RESPONSES ==========

// WebhookResponse represents the response for webhook operations
// @Description Response with webhook URL
type WebhookResponse struct {
	Success bool   `json:"success" example:"true"`
	Webhook string `json:"webhook" example:"https://example.com/webhook"`
}

// ========== ADMIN RESPONSES ==========

// AddUserResponse represents the response for adding a user
// @Description Response after creating a new user
type AddUserResponse struct {
	Success bool   `json:"success" example:"true"`
	ID      string `json:"id" example:"a7e5dd6b-8b3e-4035-ba87-3f96a0e3f5c0"`
	Token   string `json:"token" example:"abc123def456"`
	Name    string `json:"name" example:"John Doe"`
}

// ListUsersResponse represents the response for listing users
// @Description Response with list of users
type ListUsersResponse struct {
	Success bool           `json:"success" example:"true"`
	Data    []UserResponse `json:"data"`
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
