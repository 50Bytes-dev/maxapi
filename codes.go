package main

import (
	"strings"

	"maxapi/maxclient"
)

// The codes of the contract the gateway shares with vkapi (API.md, "Codes").
// Branch on them, never on the error text. They are lowercase because the
// contract is shared; the uppercase ones in constants.go are generic.
const (
	// Delivery outcome of a send or a delete. Only send_failed may have
	// reached MAX; every other one did not.
	CodeNotConnected       = "not_connected"       // 503: no live MAX session, or MAX did not answer before anything went out
	CodeInvalidTarget      = "invalid_target"      // 400: no chat and no phone
	CodeAddressNotFound    = "address_not_found"   // 404: no such user or chat
	CodeAddressRefused     = "address_refused"     // 403: the chat does not let the account write
	CodeLookupFailed       = "lookup_failed"       // 503: the phone lookup failed
	CodeSendRejected       = "send_rejected"       // 422: MAX refused, or the account is paused
	CodeMessageRejected    = "message_rejected"    // 422: the message itself is refused; it will not go to this chat as it is
	CodeAttachmentRejected = "attachment_rejected" // 422: an attachment is refused (expired, wrong, gone); upload it again
	CodeSendFailed         = "send_failed"         // 500: the outcome is unknown

	// The account is paused: any other call that would reach MAX.
	CodePagePaused = "page_paused" // 409

	// Calls other than a send.
	CodeUpstreamError = "upstream_error" // 502: MAX refused the call
	CodeInvalidImage  = "invalid_image"  // 400: not a JPEG, PNG, GIF or WebP image
	CodeFetchFailed   = "fetch_failed"   // 422: the url could not be fetched
	CodeUploadFailed  = "upload_failed"  // 502: MAX did not take the upload; it may be tried again
)

// MAX error codes the delivery codes are told by. Only these are known for
// certain (the MAX web client handles them by name); the markers below are
// read from codes as MAX spells them and are to be confirmed live.
var (
	// A chat that does not let the account write (the web client's
	// "no access to the chat" and "no access to the channel").
	refusedChatCodes = map[string]bool{"chat.denied": true, "channel.denied": true, "chat.write.denied": true}
	// No such chat or user.
	notFoundCodes = map[string]bool{maxclient.ServerCodeNotFound: true, "chat.not.found": true, maxclient.CodeUserNotFound: true}
	// Markers of a refusal of the message itself: its text.
	messageCodeMarkers = []string{"too.long", "length", "text", "empty"}
	// Markers of a refusal of an attachment: an expired, wrong or deleted
	// photo token.
	attachmentCodeMarkers = []string{"attach", "photo", "token", "file", "video", "upload"}
)

// sendRefusal tells the delivery code of a send MAX refused with code, when
// the code is no stop signal (pauseCode decides that first). withAttachments
// says whether the message carried attachments: only then is an attachment
// marker read as such.
func sendRefusal(code string, withAttachments bool) (httpStatus int, outcome string) {
	lower := strings.ToLower(code)
	switch {
	case refusedChatCodes[lower]:
		return 403, CodeAddressRefused
	case withAttachments && containsAny(lower, attachmentCodeMarkers):
		return 422, CodeAttachmentRejected
	case notFoundCodes[lower]:
		return 404, CodeAddressNotFound
	case containsAny(lower, messageCodeMarkers):
		return 422, CodeMessageRejected
	}
	return 422, CodeSendRejected
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
