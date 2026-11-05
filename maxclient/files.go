package maxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"time"
)

// UploadedPhoto is a photo MAX holds after PHOTO_UPLOAD: Token attaches it
// to a message, PhotoID is MAX's id of it.
type UploadedPhoto struct {
	PhotoID int64
	Token   string
}

// UploadError is a failure of the HTTP upload of a file to the address MAX
// gave out: MAX did not take the file. Nothing was sent, and the upload may
// be tried again.
type UploadError struct {
	Reason string
	Err    error
}

func (e *UploadError) Error() string {
	if e.Err == nil {
		return "upload failed: " + e.Reason
	}
	return "upload failed: " + e.Reason + ": " + e.Err.Error()
}

func (e *UploadError) Unwrap() error { return e.Err }

// uploadHTTPClient posts files to MAX's upload addresses. Each call bounds
// itself with its own context.
var uploadHTTPClient = &http.Client{}

// UploadPhotoWithin uploads a photo the way the web client does: PHOTO_UPLOAD
// (opcode 80, {count: 1}) for an upload address, waiting at most
// requestTimeout for it, then a multipart POST of the file there, at most
// uploadTimeout. MAX answers the POST with {"photos": {"<photoId>": {"token":
// ...}}}. A MAX error on opcode 80 comes back as it is (ServerErrorCode);
// every failure of the POST is an *UploadError.
func (c *Client) UploadPhotoWithin(data []byte, filename, contentType string, requestTimeout, uploadTimeout time.Duration) (*UploadedPhoto, error) {
	c.Logger.Info().Int("bytes", len(data)).Msg("Requesting photo upload URL")

	resp, err := c.sendAndWaitWithTimeout(OpPhotoUpload, map[string]interface{}{"count": 1}, requestTimeout)
	if err != nil {
		return nil, err
	}
	url, ok := resp.Payload["url"].(string)
	if !ok || url == "" {
		return nil, &UploadError{Reason: "MAX gave no upload address"}
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filepath.Base(filename)))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, &UploadError{Reason: "the request could not be built", Err: err}
	}
	if _, err := part.Write(data); err != nil {
		return nil, &UploadError{Reason: "the request could not be built", Err: err}
	}
	if err := writer.Close(); err != nil {
		return nil, &UploadError{Reason: "the request could not be built", Err: err}
	}

	ctx, cancel := context.WithTimeout(c.ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, &UploadError{Reason: "MAX gave a malformed upload address", Err: err}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	httpResp, err := uploadHTTPClient.Do(req)
	if err != nil {
		return nil, &UploadError{Reason: "the upload address did not answer", Err: err}
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return nil, &UploadError{Reason: fmt.Sprintf("the upload address answered HTTP %d", httpResp.StatusCode)}
	}

	var result struct {
		Photos map[string]struct {
			Token string `json:"token"`
		} `json:"photos"`
	}
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, &UploadError{Reason: "the upload answer is unreadable", Err: err}
	}
	for id, photo := range result.Photos {
		if photo.Token == "" {
			continue
		}
		photoID, _ := strconv.ParseInt(id, 10, 64)
		c.Logger.Info().Int64("photoId", photoID).Msg("Photo uploaded")
		return &UploadedPhoto{PhotoID: photoID, Token: photo.Token}, nil
	}
	return nil, &UploadError{Reason: "the upload answer has no photo token"}
}

// UploadFile uploads a file and returns the attachment for sending
func (c *Client) UploadFile(data []byte, filename string) (*Attachment, error) {
	// Request upload URL
	payload := map[string]interface{}{
		"count": 1,
	}

	c.Logger.Info().Str("filename", filename).Msg("Requesting file upload URL")

	resp, err := c.sendAndWait(OpFileUpload, payload)
	if err != nil {
		return nil, err
	}

	info, ok := resp.Payload["info"].([]interface{})
	if !ok || len(info) == 0 {
		return nil, NewError("no_upload_info", "No upload info in response", "Upload Error")
	}

	uploadInfo, ok := info[0].(map[string]interface{})
	if !ok {
		return nil, NewError("invalid_upload_info", "Invalid upload info format", "Upload Error")
	}

	url, _ := uploadInfo["url"].(string)
	fileID, _ := uploadInfo["fileId"].(float64)

	if url == "" || fileID == 0 {
		return nil, NewError("no_upload_url", "No upload URL or file ID", "Upload Error")
	}

	// Register waiter for file processing completion
	waiterCh := c.registerFileWaiter(int64(fileID))
	defer c.unregisterFileWaiter(int64(fileID))

	// Upload file via HTTP POST
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(filename)))
	req.Header.Set("Content-Range", fmt.Sprintf("0-%d/%d", len(data)-1, len(data)))

	client := &http.Client{Timeout: DefaultTimeout}
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, NewError("upload_failed", fmt.Sprintf("Upload failed with status %d", httpResp.StatusCode), "Upload Error")
	}

	// Wait for file processing notification
	select {
	case <-waiterCh:
		c.Logger.Info().Int64("fileId", int64(fileID)).Msg("File processed")
	case <-time.After(DefaultTimeout):
		c.Logger.Warn().Int64("fileId", int64(fileID)).Msg("Timeout waiting for file processing")
	}

	return &Attachment{
		Type:   AttachTypeFile,
		FileID: int64(fileID),
		Name:   filename,
		Size:   int64(len(data)),
	}, nil
}

// UploadVideo uploads a video and returns the attachment for sending
func (c *Client) UploadVideo(data []byte, filename string) (*Attachment, error) {
	// Request upload URL
	payload := map[string]interface{}{
		"count": 1,
	}

	c.Logger.Info().Str("filename", filename).Msg("Requesting video upload URL")

	resp, err := c.sendAndWait(OpVideoUpload, payload)
	if err != nil {
		return nil, err
	}

	info, ok := resp.Payload["info"].([]interface{})
	if !ok || len(info) == 0 {
		return nil, NewError("no_upload_info", "No upload info in response", "Upload Error")
	}

	uploadInfo, ok := info[0].(map[string]interface{})
	if !ok {
		return nil, NewError("invalid_upload_info", "Invalid upload info format", "Upload Error")
	}

	url, _ := uploadInfo["url"].(string)
	videoID, _ := uploadInfo["videoId"].(float64)
	token, _ := uploadInfo["token"].(string)

	if url == "" || videoID == 0 {
		return nil, NewError("no_upload_url", "No upload URL or video ID", "Upload Error")
	}

	// Register waiter for video processing completion
	waiterCh := c.registerFileWaiter(int64(videoID))
	defer c.unregisterFileWaiter(int64(videoID))

	// Upload video via HTTP POST
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(filename)))
	req.Header.Set("Content-Range", fmt.Sprintf("0-%d/%d", len(data)-1, len(data)))

	client := &http.Client{Timeout: 120 * time.Second} // Longer timeout for videos
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, NewError("upload_failed", fmt.Sprintf("Upload failed with status %d", httpResp.StatusCode), "Upload Error")
	}

	// Wait for video processing notification
	select {
	case <-waiterCh:
		c.Logger.Info().Int64("videoId", int64(videoID)).Msg("Video processed")
	case <-time.After(120 * time.Second):
		c.Logger.Warn().Int64("videoId", int64(videoID)).Msg("Timeout waiting for video processing")
	}

	return &Attachment{
		Type:    AttachTypeVideo,
		VideoID: int64(videoID),
		Token:   token,
	}, nil
}

// UploadAudio uploads an audio file (treated as FILE type in MAX)
func (c *Client) UploadAudio(data []byte, filename string) (*Attachment, error) {
	// Audio is uploaded as file in MAX
	return c.UploadFile(data, filename)
}

// GetVideoDownloadURL gets the download URL for a video
func (c *Client) GetVideoDownloadURL(chatID int64, messageID int64, videoID int64) (*VideoRequest, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
		"videoId":   videoID,
	}

	c.Logger.Info().Int64("videoId", videoID).Msg("Getting video download URL")

	resp, err := c.sendAndWait(OpVideoPlay, payload)
	if err != nil {
		return nil, err
	}

	result := &VideoRequest{}

	// Parse response - URL key is dynamic
	for key, value := range resp.Payload {
		switch key {
		case "EXTERNAL":
			result.External, _ = value.(string)
		case "cache":
			result.Cache, _ = value.(bool)
		default:
			// The actual URL has a dynamic key
			if url, ok := value.(string); ok && key != "error" && key != "message" {
				result.URL = url
			}
		}
	}

	if result.URL == "" {
		return nil, NewError("no_video_url", "No video URL in response", "Download Error")
	}

	return result, nil
}

// GetFileDownloadURL gets the download URL for a file
func (c *Client) GetFileDownloadURL(chatID int64, messageID int64, fileID int64) (*FileRequest, error) {
	payload := map[string]interface{}{
		"chatId":    chatID,
		"messageId": messageID,
		"fileId":    fileID,
	}

	c.Logger.Info().Int64("fileId", fileID).Msg("Getting file download URL")

	resp, err := c.sendAndWait(OpFileDownload, payload)
	if err != nil {
		return nil, err
	}

	respBytes, _ := json.Marshal(resp.Payload)
	var result FileRequest
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, err
	}

	if result.URL == "" {
		return nil, NewError("no_file_url", "No file URL in response", "Download Error")
	}

	return &result, nil
}

// DownloadFile downloads a file from a URL
func (c *Client) DownloadFile(url string) ([]byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, NewError("download_failed", fmt.Sprintf("Download failed with status %d", resp.StatusCode), "Download Error")
	}

	return io.ReadAll(resp.Body)
}

// SendMessageWithFile sends a message with a file attachment
func (c *Client) SendMessageWithFile(chatID int64, text string, fileData []byte, filename string, notify bool) (*Message, error) {
	attachment, err := c.UploadFile(fileData, filename)
	if err != nil {
		return nil, err
	}

	return c.SendMessage(SendMessageOptions{
		ChatID:      chatID,
		Text:        text,
		Notify:      notify,
		Attachments: []Attachment{*attachment},
	})
}

// SendMessageWithVideo sends a message with a video attachment
func (c *Client) SendMessageWithVideo(chatID int64, text string, videoData []byte, filename string, notify bool) (*Message, error) {
	attachment, err := c.UploadVideo(videoData, filename)
	if err != nil {
		return nil, err
	}

	return c.SendMessage(SendMessageOptions{
		ChatID:      chatID,
		Text:        text,
		Notify:      notify,
		Attachments: []Attachment{*attachment},
	})
}
