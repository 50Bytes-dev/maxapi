package maxclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"
)

// UploadPhoto uploads a photo and returns the attachment for sending
func (c *Client) UploadPhoto(data []byte, filename string) (*Attachment, error) {
	// Request upload URL
	payload := map[string]interface{}{
		"count": 1,
	}
	
	c.Logger.Info().Str("filename", filename).Msg("Requesting photo upload URL")
	
	resp, err := c.sendAndWait(OpPhotoUpload, payload)
	if err != nil {
		return nil, err
	}
	
	url, ok := resp.Payload["url"].(string)
	if !ok || url == "" {
		return nil, NewError("no_upload_url", "No upload URL in response", "Upload Error")
	}
	
	// Upload photo via HTTP POST multipart
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	
	if err := writer.Close(); err != nil {
		return nil, err
	}
	
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	
	client := &http.Client{Timeout: DefaultTimeout}
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	
	if httpResp.StatusCode != http.StatusOK {
		return nil, NewError("upload_failed", fmt.Sprintf("Upload failed with status %d", httpResp.StatusCode), "Upload Error")
	}
	
	// Parse response
	var uploadResult PhotoUploadResult
	if err := json.NewDecoder(httpResp.Body).Decode(&uploadResult); err != nil {
		return nil, err
	}
	
	// Get token from first photo
	var photoToken string
	for _, photo := range uploadResult.Photos {
		photoToken = photo.Token
		break
	}
	
	if photoToken == "" {
		return nil, NewError("no_photo_token", "No photo token in response", "Upload Error")
	}
	
	c.Logger.Info().Msg("Photo uploaded successfully")
	
	return &Attachment{
		Type:       AttachTypePhoto,
		PhotoToken: photoToken,
	}, nil
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

// SendMessageWithPhoto sends a message with a photo attachment
func (c *Client) SendMessageWithPhoto(chatID int64, text string, photoData []byte, filename string, notify bool) (*Message, error) {
	attachment, err := c.UploadPhoto(photoData, filename)
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

