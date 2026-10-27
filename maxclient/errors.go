package maxclient

import (
	"errors"
	"fmt"
)

// Error represents a MAX API error
type Error struct {
	Code    string `json:"error"`
	Message string `json:"message"`
	Title   string `json:"title,omitempty"`

	// fromServer marks errors MAX sent back in a response payload
	// (ParseError), as opposed to ones this client raised itself.
	fromServer bool
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
	ErrNotConnected    = NewError("not_connected", "WebSocket is not connected", "Connection Error")
	ErrTimeout         = NewError("timeout", "Request timed out", "Timeout Error")
	ErrAuthFailed      = NewError("auth_failed", "Authentication failed", "Auth Error")
	ErrQRExpired       = NewError("qr_expired", "QR session expired", "Auth Error")
	ErrInvalidPhone    = NewError("invalid_phone", "Invalid phone number format", "Validation Error")
	ErrUploadFailed    = NewError("upload_failed", "File upload failed", "Upload Error")
	ErrDownloadFailed  = NewError("download_failed", "File download failed", "Download Error")
	ErrInvalidResponse = NewError("invalid_response", "Invalid response from server", "Response Error")
	ErrChatNotFound    = NewError("chat_not_found", "Chat not found", "Chat Error")
	ErrUserNotFound    = NewError("user_not_found", "User not found", "User Error")
	ErrMessageNotFound = NewError("message_not_found", "Message not found", "Message Error")
)

// ServerCodeNotFound is the code MAX answers a lookup that found nothing with,
// e.g. CONTACT_INFO_BY_PHONE (46) for a number without a MAX account:
// {"error": "not.found", "title": "Не найдено", "message": "Cannot found contact by phone"}.
// It arrives as a fresh *Error, so errors.Is(err, ErrUserNotFound) does not
// match it: compare ServerErrorCode instead.
const ServerCodeNotFound = "not.found"

// Auth error codes that indicate token is expired/invalid
var authErrorCodes = map[string]bool{
	"login.token":   true, // FAIL_LOGIN_TOKEN
	"login.expired": true, // Token expired
	"auth.invalid":  true, // Invalid auth
	"auth.expired":  true, // Auth expired
}

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

	e := NewError(errorCode, message, title)
	e.fromServer = true
	return e
}

// ServerErrorCode returns the error code MAX sent back in a response payload.
// ok is false for errors this client raised itself (timeout, lost connection,
// validation): those say nothing about what MAX did with the request.
func ServerErrorCode(err error) (code string, ok bool) {
	var e *Error
	if errors.As(err, &e) && e.fromServer {
		return e.Code, true
	}
	return "", false
}

// IsError checks if the payload contains an error
func IsError(payload map[string]interface{}) bool {
	if payload == nil {
		return false
	}
	_, ok := payload["error"].(string)
	return ok
}

// LoginError is a failure of the LOGIN request (opcode 19) itself, after the
// connection and SessionInit went through. ConnectAndLogin wraps it so the
// caller can tell it from a network failure on the way: MAX refuses a login
// with an error code, but may also just drop the socket or never answer.
// Its text is that of the wrapped error.
type LoginError struct {
	Err error
}

func (e *LoginError) Error() string { return e.Err.Error() }

func (e *LoginError) Unwrap() error { return e.Err }

// IsAuthError checks if the error is an authentication error (token expired/invalid)
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}

	var e *Error
	if errors.As(err, &e) {
		return authErrorCodes[e.Code]
	}

	return false
}
