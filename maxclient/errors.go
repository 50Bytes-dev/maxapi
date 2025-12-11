package maxclient

import (
	"fmt"
)

// Error represents a MAX API error
type Error struct {
	Code    string `json:"error"`
	Message string `json:"message"`
	Title   string `json:"title,omitempty"`
}

func (e *Error) Error() string {
	if e.Title != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Title, e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError creates a new Error
func NewError(code, message, title string) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Title:   title,
	}
}

// Common errors
var (
	ErrNotConnected      = NewError("not_connected", "WebSocket is not connected", "Connection Error")
	ErrTimeout           = NewError("timeout", "Request timed out", "Timeout Error")
	ErrAuthFailed        = NewError("auth_failed", "Authentication failed", "Auth Error")
	ErrInvalidPhone      = NewError("invalid_phone", "Invalid phone number format", "Validation Error")
	ErrInvalidCode       = NewError("invalid_code", "Invalid verification code", "Validation Error")
	ErrRegistrationRequired = NewError("registration_required", "User registration is required", "Auth Error")
	ErrUploadFailed      = NewError("upload_failed", "File upload failed", "Upload Error")
	ErrDownloadFailed    = NewError("download_failed", "File download failed", "Download Error")
	ErrInvalidResponse   = NewError("invalid_response", "Invalid response from server", "Response Error")
	ErrChatNotFound      = NewError("chat_not_found", "Chat not found", "Chat Error")
	ErrUserNotFound      = NewError("user_not_found", "User not found", "User Error")
	ErrMessageNotFound   = NewError("message_not_found", "Message not found", "Message Error")
)

// ParseError parses an error from response payload
func ParseError(payload map[string]interface{}) error {
	if payload == nil {
		return nil
	}
	
	errorCode, ok := payload["error"].(string)
	if !ok || errorCode == "" {
		return nil
	}
	
	message, _ := payload["message"].(string)
	title, _ := payload["title"].(string)
	
	return NewError(errorCode, message, title)
}

// IsError checks if the payload contains an error
func IsError(payload map[string]interface{}) bool {
	if payload == nil {
		return false
	}
	_, ok := payload["error"].(string)
	return ok
}

