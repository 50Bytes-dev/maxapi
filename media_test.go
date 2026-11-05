package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"maxapi/maxclient"
)

// testPNG is a 3x2 PNG image.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noPrivateEscapes clears the switches that open the SSRF guard.
func noPrivateEscapes(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_IPS", "")
	t.Setenv("ALLOW_PRIVATE_HOSTS", "")
}

// serving returns a local HTTP server answering every request with h, and
// counts the requests.
func serving(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// The guarded fetch refuses what a caller's URL must not reach: an internal
// address (also behind a redirect), another scheme, a non-2xx answer, more
// than the limit. ALLOW_PRIVATE_HOSTS opens the guard for a named host only.
func TestFetchURLGuard(t *testing.T) {
	noPrivateEscapes(t)
	body := strings.Repeat("x", 100)
	srv, hits := serving(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("y", 101))
		case "/big-chunked":
			// No Content-Length: the limit must hold while reading.
			for i := 0; i < 101; i++ {
				_, _ = io.WriteString(w, "z")
				w.(http.Flusher).Flush()
			}
		default:
			_, _ = io.WriteString(w, body)
		}
	})
	ctx := context.Background()

	_, _, err := fetchURL(ctx, srv.URL+"/ok", 100)
	var blocked *blockedAddrError
	if !errors.As(err, &blocked) {
		t.Fatalf("a loopback address was fetched: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the guard connected to the loopback server")
	}
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/x", "//no-scheme", "http://"} {
		var fe *fetchError
		if _, _, err := fetchURL(ctx, raw, 100); !errors.As(err, &fe) {
			t.Fatalf("%q: %v, want a fetchError", raw, err)
		}
	}

	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	t.Setenv("ALLOW_PRIVATE_HOSTS", "storage.internal, "+host)
	if data, _, err := fetchURL(ctx, srv.URL+"/ok", 100); err != nil || string(data) != body {
		t.Fatalf("an allowed host: %q, %v", data, err)
	}
	if _, _, err := fetchURL(ctx, srv.URL+"/missing", 100); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a 404 was taken: %v", err)
	}
	for _, path := range []string{"/big", "/big-chunked"} {
		if _, _, err := fetchURL(ctx, srv.URL+path, 100); err == nil || !strings.Contains(err.Error(), "over 100 bytes") {
			t.Fatalf("%s: 101 bytes passed a limit of 100: %v", path, err)
		}
	}

	// A public-looking redirect into the internal network is still refused.
	t.Setenv("ALLOW_PRIVATE_HOSTS", host)
	redirect, _ := serving(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+srv.URL[strings.LastIndex(srv.URL, ":")+1:]+"/ok", http.StatusFound)
	})
	if _, _, err := fetchURL(ctx, redirect.URL, 100); !errors.As(err, &blocked) {
		t.Fatalf("a redirect to localhost was followed: %v", err)
	}
}

// Base64 over the limit is refused before it is decoded; broken base64 and
// a malformed data URL are refused too.
func TestDecodeMediaLimits(t *testing.T) {
	ctx := context.Background()
	big := base64.StdEncoding.EncodeToString(make([]byte, 101))
	var de *mediaDataError
	if _, err := decodeMedia(ctx, big, 100); !errors.As(err, &de) {
		t.Fatalf("101 bytes passed a limit of 100: %v", err)
	}
	if data, err := decodeMedia(ctx, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("abc")), 100); err != nil || string(data) != "abc" {
		t.Fatalf("data URL: %q %v", data, err)
	}
	for _, bad := range []string{"not base64 !!!", "data:image/png;base64", ""} {
		if _, err := decodeMedia(ctx, bad, 100); !errors.As(err, &de) {
			t.Fatalf("%q: %v, want a mediaDataError", bad, err)
		}
	}
}

// photoRig is a session whose MAX hands out an upload address on opcode 80,
// and an upload server answering like MAX's.
type photoRig struct {
	f       *fakeMAX
	s       *server
	userID  string
	uploads *atomic.Int32
	status  *atomic.Int32 // HTTP status of the upload server
}

func newPhotoRig(t *testing.T, opcode80 func() map[string]interface{}) *photoRig {
	t.Helper()
	r := &photoRig{status: new(atomic.Int32)}
	r.status.Store(http.StatusOK)
	var upload *httptest.Server
	upload, r.uploads = serving(t, func(w http.ResponseWriter, req *http.Request) {
		file, header, err := req.FormFile("file")
		if err != nil || header.Header.Get("Content-Type") != "image/png" {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		file.Close()
		if code := int(r.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = io.WriteString(w, `{"photos":{"987654321":{"token":"PHOTO-TOKEN"}}}`)
	})
	r.f = newFakeMAX(t, func(fr fakeFrame) map[string]interface{} {
		if maxclient.Opcode(fr.Opcode) == maxclient.OpPhotoUpload {
			if opcode80 != nil {
				return opcode80()
			}
			return map[string]interface{}{"url": upload.URL + "/upload"}
		}
		return map[string]interface{}{}
	})
	r.s = newTestServer(t)
	r.userID = t.Name()
	sessionUser(t, r.s, r.f, r.userID)
	return r
}

// A photo given as data is checked, uploaded to MAX once and answered with
// its attachment and size.
func TestUploadPhotoFromData(t *testing.T) {
	r := newPhotoRig(t, nil)
	body := mustJSON(t, map[string]string{"data": base64.StdEncoding.EncodeToString(testPNG(t))})

	status, got := call(t, r.s.UploadPhoto(), r.userID, body)

	if status != http.StatusOK || got["attachment"] != "PHOTO-TOKEN" || got["width"] != float64(3) || got["height"] != float64(2) {
		t.Fatalf("upload: %d %v", status, got)
	}
	if r.f.count(maxclient.OpPhotoUpload) != 1 || r.uploads.Load() != 1 {
		t.Fatalf("opcode 80 x%d, uploads x%d; want one each", r.f.count(maxclient.OpPhotoUpload), r.uploads.Load())
	}
}

// A multipart file works the same way.
func TestUploadPhotoMultipart(t *testing.T) {
	r := newPhotoRig(t, nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.png")
	_, _ = fw.Write(testPNG(t))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/media/photo", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(context.WithValue(req.Context(), "userinfo", Values{map[string]string{"Id": r.userID}}))
	rec := httptest.NewRecorder()

	r.s.UploadPhoto()(rec, req)

	var got map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got["attachment"] != "PHOTO-TOKEN" {
		t.Fatalf("multipart upload: %d %v", rec.Code, got)
	}
}

// A photo by URL is fetched through the guard: an internal address answers
// 422 fetch_failed without a MAX call; an allowed host is fetched.
func TestUploadPhotoFromURL(t *testing.T) {
	noPrivateEscapes(t)
	r := newPhotoRig(t, nil)
	pic := testPNG(t)
	src, hits := serving(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pic) })
	body := mustJSON(t, map[string]string{"url": src.URL + "/p.png"})

	status, got := call(t, r.s.UploadPhoto(), r.userID, body)
	wantError(t, status, got, http.StatusUnprocessableEntity, CodeFetchFailed)
	if hits.Load() != 0 || r.f.count(maxclient.OpPhotoUpload) != 0 {
		t.Fatalf("a refused fetch reached the server (%d) or MAX (%d)", hits.Load(), r.f.count(maxclient.OpPhotoUpload))
	}

	u, _ := url.Parse(src.URL)
	t.Setenv("ALLOW_PRIVATE_HOSTS", u.Hostname())
	status, got = call(t, r.s.UploadPhoto(), r.userID, body)
	if status != http.StatusOK || got["attachment"] != "PHOTO-TOKEN" {
		t.Fatalf("allowed host: %d %v", status, got)
	}
}

// What is no image never reaches MAX; a body over the limit answers 413.
func TestUploadPhotoRefusesNonImagesAndLargeBodies(t *testing.T) {
	r := newPhotoRig(t, nil)
	status, got := call(t, r.s.UploadPhoto(), r.userID, mustJSON(t, map[string]string{"data": base64.StdEncoding.EncodeToString([]byte("<html>"))}))
	wantError(t, status, got, http.StatusBadRequest, CodeInvalidImage)

	t.Setenv("MAXAPI_PHOTO_MAX_BYTES", "64")
	status, got = call(t, r.s.UploadPhoto(), r.userID, mustJSON(t, map[string]string{"data": strings.Repeat("A", 100)}))
	wantError(t, status, got, http.StatusRequestEntityTooLarge, ErrInvalidInput)
	if r.f.count(maxclient.OpPhotoUpload) != 0 {
		t.Fatalf("MAX was asked for an upload address")
	}
}

// The upload server refusing is 502 upload_failed, which may be tried again;
// a stop signal on opcode 80 pauses the account (409 page_paused), and while
// paused no upload is tried.
func TestUploadPhotoFailures(t *testing.T) {
	r := newPhotoRig(t, nil)
	data := mustJSON(t, map[string]string{"data": base64.StdEncoding.EncodeToString(testPNG(t))})
	r.status.Store(http.StatusInternalServerError)
	status, got := call(t, r.s.UploadPhoto(), r.userID, data)
	wantError(t, status, got, http.StatusBadGateway, CodeUploadFailed)

	limited := newPhotoRig(t, func() map[string]interface{} {
		return map[string]interface{}{"error": "error.limit.violate", "message": "Too many"}
	})
	status, got = call(t, limited.s.UploadPhoto(), limited.userID, data)
	wantError(t, status, got, http.StatusConflict, CodePagePaused)
	assertPaused(t, got, "error.limit.violate")
	status, got = call(t, limited.s.UploadPhoto(), limited.userID, data)
	wantError(t, status, got, http.StatusConflict, CodePagePaused)
	if n := limited.f.count(maxclient.OpPhotoUpload); n != 1 {
		t.Fatalf("opcode 80 x%d, want 1: the paused account asked again", n)
	}

	sized := newPhotoRig(t, func() map[string]interface{} {
		return map[string]interface{}{"error": "file.size.limit", "message": "Too large"}
	})
	status, got = call(t, sized.s.UploadPhoto(), sized.userID, data)
	wantError(t, status, got, http.StatusBadGateway, CodeUploadFailed)
	if got["paused"] != nil || got["upstreamCode"] != "file.size.limit" {
		t.Fatalf("a size limit paused the account: %v", got)
	}
}
