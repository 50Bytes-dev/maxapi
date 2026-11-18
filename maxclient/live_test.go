package maxclient

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// TestLiveMAX talks to the real MAX. It runs only when MAXAPI_LIVE=1:
//
//   - always: connect to WebSocketURI and SESSION_INIT (no account);
//   - MAXAPI_LIVE_TOKEN (and MAXAPI_LIVE_DEVICE, the deviceId the token was
//     issued to): LOGIN with that saved token;
//   - MAXAPI_LIVE_CHAT as well: send a text and a photo (opcode 80) there.
//
// Only the owner's test account, in a chat the owner names (see
// docs/research/max-live-checks-2026-11.md).
func TestLiveMAX(t *testing.T) {
	if os.Getenv("MAXAPI_LIVE") != "1" {
		t.Skip("MAXAPI_LIVE=1 runs the checks against the real MAX")
	}
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel)
	device := os.Getenv("MAXAPI_LIVE_DEVICE")
	if device == "" {
		device = uuid.NewString()
	}
	c := NewClient(device, logger)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect %s: %v", WebSocketURI, err)
	}
	defer c.Shutdown()
	if err := c.SessionInit(nil); err != nil {
		t.Fatalf("SESSION_INIT: %v", err)
	}
	t.Log("SESSION_INIT answered")

	token := os.Getenv("MAXAPI_LIVE_TOKEN")
	if token == "" {
		// No account: a QR login track (opcode 288), which simply expires.
		qr, err := c.RequestQRCode()
		if err != nil {
			t.Fatalf("QR start: %v", err)
		}
		if qr.QRLink == "" || qr.ExpiresAt == 0 {
			t.Fatalf("QR start: %+v", qr)
		}
		t.Logf("QR start: ttl %d, polling %d", qr.TTL, qr.PollingInterval)
		return
	}
	if _, err := c.Login(token); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	if c.Me == nil || c.Me.ID == 0 {
		t.Fatal("LOGIN answered without the profile")
	}
	t.Logf("LOGIN: user %d", c.Me.ID)

	chat := os.Getenv("MAXAPI_LIVE_CHAT")
	if chat == "" {
		return
	}
	chatID, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		t.Fatalf("MAXAPI_LIVE_CHAT: %v", err)
	}
	msg, err := c.SendMessage(SendMessageOptions{ChatID: chatID, Text: "maxapi v10 live check " + time.Now().Format(time.RFC3339), Notify: true})
	if err != nil {
		t.Fatalf("send text: %v", err)
	}
	if msg == nil || msg.ID == "" || msg.ID == "0" {
		t.Fatalf("send text: no message id (%+v)", msg)
	}
	t.Logf("text sent: message %s", msg.ID)

	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for x := 0; x < 128; x++ {
		for y := 0; y < 128; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 2), uint8(y * 2), 160, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	photo, err := c.UploadPhotoWithin(buf.Bytes(), "check.png", "image/png", DefaultTimeout, time.Minute)
	if err != nil {
		t.Fatalf("upload photo (opcode 80): %v", err)
	}
	t.Logf("photo uploaded: %d", photo.PhotoID)
	pmsg, err := c.SendMessage(SendMessageOptions{
		ChatID: chatID, Text: "maxapi v10 photo check", Notify: true,
		Attachments: []Attachment{{Type: AttachTypePhoto, PhotoToken: photo.Token}},
	})
	if err != nil {
		t.Fatalf("send photo: %v", err)
	}
	t.Logf("photo sent: message %s", pmsg.ID)
}
