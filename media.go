package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"maxapi/maxclient"

	"github.com/rs/zerolog/log"
)

// Uploading a photo once (C3). POST /media/photo uploads a photo to MAX for
// the account and answers with its attachment, MAX's photo token, which
// POST /chat/send/text takes in attachments. One photo serves every chat of
// the account; how long MAX keeps a token is to be confirmed live
// (docs/research/max-live-checks-2026-11.md). MAX does not attach a photo by
// an outside address, so the gateway fetches it itself.

const (
	// photoUploadRequestTimeout bounds MAX's answer to PHOTO_UPLOAD (80).
	photoUploadRequestTimeout = 4 * time.Second
	// photoUploadTimeout bounds the HTTP upload of the photo to MAX.
	photoUploadTimeout = 15 * time.Second
)

// PhotoBody is the JSON body of POST /media/photo: url or data.
type PhotoBody struct {
	URL  string `json:"url"`
	Data string `json:"data"`
}

// UploadPhoto is POST /media/photo.
func (s *server) UploadPhoto() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userinfo").(Values).Get("Id")
		limit := photoMaxBytes()

		client, ok := s.liveClient(w, r, userID)
		if !ok {
			return
		}
		if r.ContentLength > limit {
			s.respondError(w, r, http.StatusRequestEntityTooLarge, ErrInvalidInput,
				fmt.Sprintf("the body is over %d bytes", limit))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)

		var (
			data   []byte
			rawURL string
		)
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType == "multipart/form-data" {
			file, _, err := r.FormFile("file")
			if err != nil {
				s.respondBodyError(w, r, err, "the form has no file")
				return
			}
			data, err = io.ReadAll(file)
			file.Close()
			if err != nil {
				s.respondBodyError(w, r, err, "the file could not be read")
				return
			}
		} else {
			var body PhotoBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				s.respondBodyError(w, r, err, "could not decode payload")
				return
			}
			switch {
			case body.URL != "" && body.Data != "":
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "give url or data, not both")
				return
			case body.URL != "":
				rawURL = body.URL
			case body.Data != "":
				d, err := decodeMedia(r.Context(), body.Data, limit)
				if err != nil {
					s.respondError(w, r, http.StatusBadRequest, CodeInvalidImage, err.Error())
					return
				}
				data = d
			default:
				s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "url, data or a multipart file is required")
				return
			}
		}

		maxUserID, paused := s.refuseWhilePaused(w, r, userID)
		if paused {
			return
		}
		if rawURL != "" {
			d, _, err := fetchURL(r.Context(), rawURL, limit)
			if err != nil {
				var fetchErr *fetchError
				reason := "the url could not be fetched"
				if errors.As(err, &fetchErr) {
					reason = fetchErr.reason
				}
				log.Info().Err(err).Str("userID", userID).Msg("Photo fetch failed")
				s.respondError(w, r, http.StatusUnprocessableEntity, CodeFetchFailed, reason)
				return
			}
			data = d
		}
		info, err := detectImage(data)
		if err != nil {
			s.respondError(w, r, http.StatusBadRequest, CodeInvalidImage, "the data is not a JPEG, PNG, GIF or WebP image")
			return
		}

		if userSemaphoreManager != nil {
			userSemaphoreManager.Acquire(userID)
			defer userSemaphoreManager.Release(userID)
		}
		photo, err := client.UploadPhotoWithin(data, "image."+info.format, "image/"+info.format,
			photoUploadRequestTimeout, photoUploadTimeout)
		if err != nil {
			s.respondUploadFailure(w, r, userID, maxUserID, err)
			return
		}
		s.Respond(w, r, http.StatusOK, map[string]interface{}{
			"success":    true,
			"attachment": photo.Token,
			"width":      info.width,
			"height":     info.height,
		})
	}
}

// respondBodyError answers a body that could not be read: 413 when it is
// over the limit, 400 otherwise.
func (s *server) respondBodyError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		s.respondError(w, r, http.StatusRequestEntityTooLarge, ErrInvalidInput,
			fmt.Sprintf("the body is over %d bytes", tooLarge.Limit))
		return
	}
	s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, msg)
}

// respondUploadFailure answers a failed upload: a stop signal pauses the
// account (409 page_paused), the session lost is 503 not_connected, anything
// else 502 upload_failed, which may be tried again.
func (s *server) respondUploadFailure(w http.ResponseWriter, r *http.Request, userID string, maxUserID int64, err error) {
	if pause := s.pauseOnLimit(userID, maxUserID, err); pause != nil {
		s.respondDelivery(w, r, http.StatusConflict, CodePagePaused, "MAX refused the upload; the account is paused", err, pause)
		return
	}
	if errors.Is(err, maxclient.ErrNotConnected) {
		s.respondDelivery(w, r, http.StatusServiceUnavailable, CodeNotConnected, "the session was lost during the upload", err, nil)
		return
	}
	var uploadErr *maxclient.UploadError
	msg := "MAX did not take the photo"
	if errors.As(err, &uploadErr) {
		msg = strings.TrimPrefix(uploadErr.Error(), "upload failed: ")
		if uploadErr.Err != nil {
			msg = strings.TrimSuffix(msg, ": "+uploadErr.Err.Error())
		}
	}
	log.Warn().Err(err).Str("userID", userID).Msg("Photo upload failed")
	s.respondDelivery(w, r, http.StatusBadGateway, CodeUploadFailed, msg, err, nil)
}
