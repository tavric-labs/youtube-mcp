// Package youtube calls the YouTube Data API v3 with one person's OAuth token.
//
// The client never stores a token and never returns an upstream response body
// to its callers: failures surface as typed errors (see APIError) whose text is
// safe to log and to show a person.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Errors a caller can test with errors.Is.
var (
	// ErrUnauthorized means Google rejected the token: it expired or was revoked.
	ErrUnauthorized = errors.New("youtube authorization expired or revoked")
	// ErrQuota means the Google Cloud project's daily quota (or rate limit) is used up.
	ErrQuota = errors.New("youtube API quota exceeded")
	// ErrForbidden means the signed-in account may not do this (missing scope,
	// not the channel owner, comments disabled, upload limit, and so on).
	ErrForbidden = errors.New("youtube refused the request for this account")
	// ErrNotFound means the video, playlist or comment does not exist or is not visible.
	ErrNotFound = errors.New("youtube resource not found")
	// ErrSessionExpired means a resumable upload session is gone and a new one is needed.
	ErrSessionExpired = errors.New("youtube upload session expired")
)

// Object is a decoded JSON object from the API.
type Object = map[string]any

// APIError is a non-2xx answer from YouTube. Only the HTTP status and Google's
// short machine reason (for example quotaExceeded) are kept; the message text
// is dropped because it can echo request content.
type APIError struct {
	Status int
	Reason string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("youtube returned HTTP %d (%s)", e.Status, e.Reason)
	}
	return fmt.Sprintf("youtube returned HTTP %d", e.Status)
}

// Is maps an APIError to the package's sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrQuota:
		switch e.Reason {
		case "quotaExceeded", "dailyLimitExceeded", "rateLimitExceeded", "userRateLimitExceeded":
			return true
		}
		return e.Status == http.StatusTooManyRequests
	case ErrForbidden:
		return e.Status == http.StatusForbidden && !e.Is(ErrQuota)
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	}
	return false
}

// IncompleteError reports a resumable upload that has not finished. Calling
// Upload again with the same session continues from Offset.
type IncompleteError struct {
	Offset, Size int64
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("upload incomplete: %d of %d bytes sent", e.Offset, e.Size)
}

// Client talks to the YouTube Data API.
type Client struct {
	// Base is the Data API root, UploadBase the media upload root.
	Base, UploadBase string
	HTTP             *http.Client
	// Attempts is how many times an idempotent request is tried (default 3).
	Attempts int
	// Backoff is the pause before the first retry, doubled each time (default 500ms).
	Backoff time.Duration
}

// ChunkSize is how much of a video one upload request carries. Google needs a
// multiple of 256 KiB for every chunk except the last.
const ChunkSize int64 = 8 << 20

// New returns a client for the public YouTube endpoints.
func New() *Client {
	return NewWithBases("https://www.googleapis.com/youtube/v3", "https://www.googleapis.com/upload/youtube/v3")
}

// NewWithBases returns a client for the given API and upload roots.
func NewWithBases(base, uploadBase string) *Client {
	return &Client{
		Base:       strings.TrimRight(base, "/"),
		UploadBase: strings.TrimRight(uploadBase, "/"),
		HTTP: &http.Client{
			Timeout:       90 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("youtube api must not redirect") },
		},
		Attempts: 3,
		Backoff:  500 * time.Millisecond,
	}
}

// Call sends one Data API request. GET, PUT and DELETE are retried on network
// errors, 429 and 5xx; POST is sent once because repeating it could create a
// duplicate playlist, comment or reply.
func (c *Client) Call(ctx context.Context, token, method, path string, query url.Values, body any) (Object, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = encoded
	}
	endpoint := c.Base + "/" + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	response, err := c.send(ctx, method != http.MethodPost, func() (*http.Request, error) {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		return request, nil
	})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return decode(response)
}

// Get is Call with GET and no body.
func (c *Client) Get(ctx context.Context, token, path string, query url.Values) (Object, error) {
	return c.Call(ctx, token, http.MethodGet, path, query, nil)
}

// send runs build's request, retrying when allowed. The caller closes the
// response. A response with a retryable status after the last attempt is
// returned as is so the caller turns it into an APIError.
func (c *Client) send(ctx context.Context, retry bool, build func() (*http.Request, error)) (*http.Response, error) {
	attempts := 1
	if retry && c.Attempts > 1 {
		attempts = c.Attempts
	}
	pause := c.Backoff
	var lastErr error
	for attempt := 1; ; attempt++ {
		request, err := build()
		if err != nil {
			return nil, err
		}
		response, err := c.HTTP.Do(request)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = errors.New("could not reach YouTube")
		case retryableStatus(response.StatusCode) && attempt < attempts:
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
			response.Body.Close()
			lastErr = nil
		default:
			return response, nil
		}
		if attempt >= attempts {
			return nil, lastErr
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		pause *= 2
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

var reasonPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// apiError reads Google's error envelope and keeps only the status and reason.
func apiError(response *http.Response) *APIError {
	failure := &APIError{Status: response.StatusCode}
	var envelope struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&envelope) == nil && len(envelope.Error.Errors) > 0 {
		if reason := envelope.Error.Errors[0].Reason; reasonPattern.MatchString(reason) {
			failure.Reason = reason
		}
	}
	return failure
}

func decode(response *http.Response) (Object, error) {
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, apiError(response)
	}
	if response.StatusCode == http.StatusNoContent {
		return Object{}, nil
	}
	var out Object
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&out); err != nil {
		return nil, errors.New("youtube returned an unreadable response")
	}
	return out, nil
}

// StartUpload opens a resumable upload session for a video of size bytes and
// returns its URI. The URI is a capability: anyone holding it together with the
// token can write to the session, so callers keep it private and never log or
// return it. Starting a session creates no video, so this is safe to retry.
func (c *Client) StartUpload(ctx context.Context, token string, metadata Object, size int64, mime string) (string, error) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	endpoint := c.UploadBase + "/videos?uploadType=resumable&part=snippet,status&notifySubscribers=false"
	response, err := c.send(ctx, true, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json; charset=UTF-8")
		request.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
		request.Header.Set("X-Upload-Content-Type", mime)
		return request, nil
	})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", apiError(response)
	}
	session := response.Header.Get("Location")
	if err := c.CheckSession(session); err != nil {
		return "", err
	}
	return session, nil
}

// CheckSession refuses a session URI that does not point at the upload host.
// The saved URI is read back from disk and the OAuth token is sent to it, so it
// is checked before every use.
func (c *Client) CheckSession(session string) error {
	parsed, err := url.Parse(session)
	base, baseErr := url.Parse(c.UploadBase)
	if err != nil || baseErr != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host ||
		!strings.HasPrefix(parsed.Path, base.Path+"/videos") || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("youtube returned an invalid upload location")
	}
	return nil
}

// Upload sends file to a resumable session and returns the created video. It
// first asks Google how much it already has, so it is safe to call again after
// any interruption. When ctx ends or a chunk keeps failing it returns an
// *IncompleteError (or the underlying error) and the session stays usable;
// ErrSessionExpired means start a new session.
func (c *Client) Upload(ctx context.Context, token, session string, file io.ReaderAt, size int64, mime string) (Object, error) {
	if err := c.CheckSession(session); err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, errors.New("nothing to upload")
	}
	offset, done, err := c.probe(ctx, token, session, size)
	if err != nil || done != nil {
		return done, err
	}
	failures := 0
	for offset < size {
		if ctx.Err() != nil {
			return nil, &IncompleteError{Offset: offset, Size: size}
		}
		length := min(ChunkSize, size-offset)
		done, next, err := c.putChunk(ctx, token, session, file, offset, length, size, mime)
		switch {
		case done != nil:
			return done, nil
		case err == nil:
			if next <= offset || next > size {
				return nil, errors.New("youtube acknowledged an unexpected upload range")
			}
			offset, failures = next, 0
		case ctx.Err() != nil:
			return nil, &IncompleteError{Offset: offset, Size: size}
		case errors.Is(err, ErrSessionExpired), errors.Is(err, ErrUnauthorized), errors.Is(err, ErrQuota), errors.Is(err, ErrForbidden):
			return nil, err
		default:
			// A transient failure: ask Google where it stands, then continue.
			if failures++; failures >= max(c.Attempts, 1) {
				return nil, &IncompleteError{Offset: offset, Size: size}
			}
			if offset, done, err = c.probe(ctx, token, session, size); err != nil || done != nil {
				return done, err
			}
		}
	}
	return nil, errors.New("youtube sent no final upload result; run the upload again to check")
}

// putChunk sends bytes [offset, offset+length). It returns the video when that
// finished the upload, or the new offset when Google asks for more.
func (c *Client) putChunk(ctx context.Context, token, session string, file io.ReaderAt, offset, length, size int64, mime string) (Object, int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, session, io.NewSectionReader(file, offset, length))
	if err != nil {
		return nil, 0, err
	}
	request.ContentLength = length
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", mime)
	request.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size))
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, 0, errors.New("upload interrupted")
	}
	defer response.Body.Close()
	return interpretUpload(response)
}

// probe asks Google how many bytes the session holds. A finished session
// answers with the video itself.
func (c *Client) probe(ctx context.Context, token, session string, size int64) (int64, Object, error) {
	response, err := c.send(ctx, true, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, session, nil)
		if err != nil {
			return nil, err
		}
		request.ContentLength = 0
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return request, nil
	})
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	done, offset, err := interpretUpload(response)
	if err != nil || done != nil {
		return 0, done, err
	}
	if offset > size {
		return 0, nil, errors.New("youtube reported more bytes than were uploaded")
	}
	return offset, nil, nil
}

// interpretUpload turns an upload response into (video, nextOffset, error).
func interpretUpload(response *http.Response) (Object, int64, error) {
	switch {
	case response.StatusCode == http.StatusPermanentRedirect:
		next, err := rangeEnd(response.Header.Get("Range"))
		return nil, next, err
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		return nil, 0, ErrSessionExpired
	}
	video, err := decode(response)
	if err != nil {
		return nil, 0, err
	}
	return video, 0, nil
}

// rangeEnd parses a "bytes=0-N" header into the next offset, N+1. No header
// means Google holds nothing yet.
func rangeEnd(header string) (int64, error) {
	if header == "" {
		return 0, nil
	}
	last, ok := strings.CutPrefix(header, "bytes=0-")
	if !ok {
		return 0, errors.New("youtube sent an invalid upload range")
	}
	n, err := strconv.ParseInt(last, 10, 64)
	if err != nil || n < 0 || n == 1<<63-1 {
		return 0, errors.New("youtube sent an invalid upload range")
	}
	return n + 1, nil
}

// Items returns the "items" array of a list response.
func Items(o Object) []Object {
	var out []Object
	values, _ := o["items"].([]any)
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// Child returns the nested object at key, or an empty one.
func Child(o Object, key string) Object {
	if child, ok := o[key].(map[string]any); ok {
		return child
	}
	return Object{}
}

// String returns the string at key, or "".
func String(o Object, key string) string {
	value, _ := o[key].(string)
	return value
}
