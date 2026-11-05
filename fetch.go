package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // image.DecodeConfig: GIF
	_ "image/jpeg" // image.DecodeConfig: JPEG
	_ "image/png"  // image.DecodeConfig: PNG
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp" // image.DecodeConfig: WebP
)

// Media the caller names by URL is fetched by the gateway itself, from its
// own address. Every such fetch goes through fetchURL: http and https only,
// no private, loopback, link-local or otherwise internal address (the SSRF
// guard of safeDialContext), a time limit, a size limit and a 2xx status.
// ALLOW_PRIVATE_IPS=true turns the address guard off altogether (dev and
// test); ALLOW_PRIVATE_HOSTS opens it for named hosts only, such as the
// operator's own storage in the same network.

const (
	// defaultPhotoMaxBytes bounds a photo: its download, its decoded data and
	// the body of POST /media/photo.
	defaultPhotoMaxBytes = 10 << 20
	// defaultMediaMaxBytes bounds any other media: a document, audio or video
	// to send, and a file downloaded for the caller.
	defaultMediaMaxBytes = 50 << 20

	// fetchTimeout bounds one fetch, from the dial to the last byte.
	fetchTimeout = 8 * time.Second
	// fetchMaxRedirects is how many redirects a fetch follows; each one goes
	// through the same guard.
	fetchMaxRedirects = 3
)

// photoMaxBytes is MAXAPI_PHOTO_MAX_BYTES, defaultPhotoMaxBytes when unset or
// not a positive number.
func photoMaxBytes() int64 { return envBytes("MAXAPI_PHOTO_MAX_BYTES", defaultPhotoMaxBytes) }

// mediaMaxBytes is MAXAPI_MEDIA_MAX_BYTES, defaultMediaMaxBytes when unset or
// not a positive number.
func mediaMaxBytes() int64 { return envBytes("MAXAPI_MEDIA_MAX_BYTES", defaultMediaMaxBytes) }

func envBytes(name string, def int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// fetchAllowedHost reports whether host (a name or an address, without the
// port) is listed in ALLOW_PRIVATE_HOSTS, a comma-separated list compared
// without case. Such a host may resolve to an address the guard otherwise
// refuses.
func fetchAllowedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range strings.Split(os.Getenv("ALLOW_PRIVATE_HOSTS"), ",") {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if h != "" && h == host {
			return true
		}
	}
	return false
}

// blockedAddrError is a fetch the SSRF guard refused: the host resolved to
// an address it does not connect to.
type blockedAddrError struct{ ip net.IP }

func (e *blockedAddrError) Error() string {
	return fmt.Sprintf("address %s is not allowed (SSRF guard)", e.ip)
}

// fetchError is a failed fetch of a caller's URL: it answers 422
// fetch_failed. reason is a fixed text for the answer; err, the cause, goes
// to the log only.
type fetchError struct {
	reason string
	err    error
}

func (e *fetchError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.reason + ": " + e.err.Error()
}

func (e *fetchError) Unwrap() error { return e.err }

// mediaDataError is media data in the body that could not be read: broken
// base64, a malformed data URL, or more than the limit. It answers 400.
type mediaDataError struct{ reason string }

func (e *mediaDataError) Error() string { return e.reason }

// fetchClient makes every fetch. It never uses a proxy from the environment:
// a proxy would make the connection the guard checks one to the proxy.
var fetchClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           safeDialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: fetchTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > fetchMaxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirect to a scheme other than http(s)")
		}
		return nil
	},
}

// fetchURL downloads rawURL through the guarded client: http(s) only, within
// fetchTimeout, a 2xx status, at most limit bytes. It returns the body and
// its Content-Type. Every failure is a *fetchError.
func fetchURL(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	return fetchURLWithin(ctx, rawURL, limit, fetchTimeout)
}

// fetchURLWithin is fetchURL with its own time limit, for large files MAX
// serves.
func fetchURLWithin(ctx context.Context, rawURL string, limit int64, timeout time.Duration) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", &fetchError{reason: "the url is not an http(s) address"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", &fetchError{reason: "the url is not an http(s) address", err: err}
	}
	req.Header.Set("User-Agent", "maxapi/"+version)

	resp, err := fetchClient.Do(req)
	if err != nil {
		var blocked *blockedAddrError
		if errors.As(err, &blocked) {
			return nil, "", &fetchError{reason: "the url's host resolves to an address that is not allowed", err: err}
		}
		return nil, "", &fetchError{reason: "the url could not be fetched", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", &fetchError{reason: fmt.Sprintf("the url answered HTTP %d", resp.StatusCode)}
	}
	if resp.ContentLength > limit {
		return nil, "", &fetchError{reason: fmt.Sprintf("the url's content is over %d bytes", limit)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", &fetchError{reason: "the url could not be fetched", err: err}
	}
	if int64(len(data)) > limit {
		return nil, "", &fetchError{reason: fmt.Sprintf("the url's content is over %d bytes", limit)}
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// decodeMedia reads media the caller gave as a string: an http(s) URL, which
// is fetched (a *fetchError on failure), a data URL, or base64 (a
// *mediaDataError on failure). The data is at most limit bytes.
func decodeMedia(ctx context.Context, value string, limit int64) ([]byte, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return nil, &mediaDataError{reason: "no media given"}
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		data, _, err := fetchURL(ctx, value, limit)
		return data, err
	case strings.HasPrefix(value, "data:"):
		return decodeDataURL(value, limit)
	default:
		return decodeBase64(value, limit)
	}
}

// decodeDataURL decodes data:[<type>][;base64],<data>.
func decodeDataURL(value string, limit int64) ([]byte, error) {
	header, payload, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !ok {
		return nil, &mediaDataError{reason: "the data URL has no comma"}
	}
	if strings.HasSuffix(strings.ToLower(header), ";base64") {
		return decodeBase64(payload, limit)
	}
	if int64(len(payload)) > 3*limit {
		return nil, &mediaDataError{reason: fmt.Sprintf("the media is over %d bytes", limit)}
	}
	data, err := url.PathUnescape(payload)
	if err != nil {
		return nil, &mediaDataError{reason: "the data URL is malformed"}
	}
	if int64(len(data)) > limit {
		return nil, &mediaDataError{reason: fmt.Sprintf("the media is over %d bytes", limit)}
	}
	return []byte(data), nil
}

// decodeBase64 decodes standard base64, padded or not. The length is checked
// before decoding, so an oversized value is never held decoded.
func decodeBase64(value string, limit int64) ([]byte, error) {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, value)
	if int64(base64.RawStdEncoding.DecodedLen(len(strings.TrimRight(value, "=")))) > limit {
		return nil, &mediaDataError{reason: fmt.Sprintf("the media is over %d bytes", limit)}
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(value, "="))
	}
	if err != nil {
		return nil, &mediaDataError{reason: "the media is neither an http(s) URL, a data URL nor base64"}
	}
	return data, nil
}

// imageInfo says what a photo is, read from its first bytes.
type imageInfo struct {
	format        string // jpeg, png, gif or webp
	width, height int
}

// errNotImage is data that is no JPEG, PNG, GIF or WebP image.
var errNotImage = errors.New("not a JPEG, PNG, GIF or WebP image")

// detectImage reads the format and size of a JPEG, PNG, GIF or WebP image
// without decoding its pixels.
func detectImage(data []byte) (imageInfo, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return imageInfo{}, errNotImage
	}
	return imageInfo{format: format, width: cfg.Width, height: cfg.Height}, nil
}

// downloadMedia downloads the media of an incoming message from MAX's
// storage, through the same guarded fetch as a caller's URL.
func downloadMedia(rawURL string) ([]byte, error) {
	data, _, err := fetchURL(context.Background(), rawURL, mediaMaxBytes())
	return data, err
}
