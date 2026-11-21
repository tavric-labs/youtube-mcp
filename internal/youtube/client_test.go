package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fast(c *Client) *Client {
	c.Backoff = time.Millisecond
	return c
}

func TestErrorsKeepOnlyTheStatusAndReasonAndMapToSentinels(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"error":{"errors":[{"reason":"authError"}]}}`, ErrUnauthorized},
		{403, `{"error":{"errors":[{"reason":"quotaExceeded"}]}}`, ErrQuota},
		{403, `{"error":{"errors":[{"reason":"forbidden"}]}}`, ErrForbidden},
		{404, `{"error":{"errors":[{"reason":"videoNotFound"}]}}`, ErrNotFound},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(strings.Replace(tc.body, `}]}}`, `}],"message":"secret-user-text"}}`, 1)))
		}))
		_, err := fast(NewWithBases(server.URL, server.URL)).Get(context.Background(), "tok", "videos", nil)
		server.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret-user-text") {
			t.Errorf("the upstream message leaked: %v", err)
		}
	}
	if errors.Is(&APIError{Status: 403, Reason: "quotaExceeded"}, ErrForbidden) {
		t.Error("a quota failure was reported as forbidden")
	}
}

func TestAnUnsafeReasonIsDropped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"bad reason with spaces and secrets"}]}}`))
	}))
	defer server.Close()
	_, err := NewWithBases(server.URL, server.URL).Get(context.Background(), "tok", "videos", nil)
	if err == nil || strings.Contains(err.Error(), "secrets") {
		t.Fatalf("err = %v", err)
	}
}

func TestIdempotentCallsRetryTransientFailuresButPostsDoNot(t *testing.T) {
	var gets, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(503)
			return
		}
		if gets.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"v1"}]}`))
	}))
	defer server.Close()
	c := fast(NewWithBases(server.URL, server.URL))
	out, err := c.Get(context.Background(), "tok", "videos", nil)
	if err != nil || len(Items(out)) != 1 || gets.Load() != 3 {
		t.Fatalf("out=%v err=%v gets=%d", out, err, gets.Load())
	}
	if _, err := c.Call(context.Background(), "tok", http.MethodPost, "playlists", nil, map[string]any{"a": 1}); err == nil || posts.Load() != 1 {
		t.Fatalf("err=%v posts=%d, want one attempt and an error", err, posts.Load())
	}
}

func TestRetriesStopWhenTheContextEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := NewWithBases(server.URL, server.URL)
	c.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "tok", "videos", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// fakeUpload is a minimal resumable-upload endpoint.
type fakeUpload struct {
	t          *testing.T
	server     *httptest.Server
	received   int64 // bytes Google "has"
	ranges     []string
	failOnce   atomic.Bool
	gone       atomic.Bool
	foreignHit atomic.Int32
}

func newFakeUpload(t *testing.T) *fakeUpload {
	f := &fakeUpload{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpload) session() string { return f.server.URL + "/videos?upload_id=abc" }

func (f *fakeUpload) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		f.t.Errorf("upload auth = %q", r.Header.Get("Authorization"))
	}
	switch {
	case r.Method == http.MethodPost:
		if r.Header.Get("X-Upload-Content-Length") == "" || r.Header.Get("X-Upload-Content-Type") != "video/mp4" {
			f.t.Errorf("start headers = %v", r.Header)
		}
		w.Header().Set("Location", f.session())
	case r.Method == http.MethodPut:
		if f.gone.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		contentRange := r.Header.Get("Content-Range")
		f.ranges = append(f.ranges, contentRange)
		var total int64
		if strings.HasPrefix(contentRange, "bytes */") {
			fmt.Sscanf(contentRange, "bytes */%d", &total)
			f.respond(w, total)
			return
		}
		var first, last int64
		fmt.Sscanf(contentRange, "bytes %d-%d/%d", &first, &last, &total)
		if f.failOnce.CompareAndSwap(true, false) {
			f.received = first + (last-first+1)/2 // took half the chunk, then the connection broke
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if first != f.received {
			f.t.Errorf("chunk starts at %d but Google has %d", first, f.received)
		}
		body := make([]byte, last-first+1)
		n, _ := r.Body.Read(body)
		_ = n
		f.received = last + 1
		f.respond(w, total)
	}
}

func (f *fakeUpload) respond(w http.ResponseWriter, total int64) {
	if f.received >= total {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"vid123","status":{"privacyStatus":"private"}}`))
		return
	}
	if f.received > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", f.received-1))
	}
	w.WriteHeader(http.StatusPermanentRedirect)
}

func TestUploadSendsChunksInOrderAndReturnsTheVideo(t *testing.T) {
	f := newFakeUpload(t)
	size := ChunkSize + 1<<20
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	video, err := c.Upload(context.Background(), "tok", f.session(), bytes.NewReader(make([]byte, size)), size, "video/mp4")
	if err != nil || String(video, "id") != "vid123" {
		t.Fatalf("video=%v err=%v", video, err)
	}
	want := []string{
		fmt.Sprintf("bytes */%d", size),
		fmt.Sprintf("bytes 0-%d/%d", ChunkSize-1, size),
		fmt.Sprintf("bytes %d-%d/%d", ChunkSize, size-1, size),
	}
	if strings.Join(f.ranges, "|") != strings.Join(want, "|") {
		t.Fatalf("ranges = %v, want %v", f.ranges, want)
	}
}

func TestUploadResumesFromWhatGoogleAlreadyHas(t *testing.T) {
	f := newFakeUpload(t)
	size := ChunkSize + 1<<20
	f.received = ChunkSize // an earlier call delivered the first chunk
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	video, err := c.Upload(context.Background(), "tok", f.session(), bytes.NewReader(make([]byte, size)), size, "video/mp4")
	if err != nil || String(video, "id") != "vid123" {
		t.Fatalf("video=%v err=%v", video, err)
	}
	if len(f.ranges) != 2 || f.ranges[1] != fmt.Sprintf("bytes %d-%d/%d", ChunkSize, size-1, size) {
		t.Fatalf("a resumed upload re-sent data: %v", f.ranges)
	}
}

func TestAnAlreadyFinishedSessionReturnsTheVideoWithoutSendingBytes(t *testing.T) {
	f := newFakeUpload(t)
	f.received = 1024
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	video, err := c.Upload(context.Background(), "tok", f.session(), bytes.NewReader(make([]byte, 1024)), 1024, "video/mp4")
	if err != nil || String(video, "id") != "vid123" || len(f.ranges) != 1 {
		t.Fatalf("video=%v err=%v ranges=%v", video, err, f.ranges)
	}
}

func TestATransientChunkFailureIsResumedFromTheProbe(t *testing.T) {
	f := newFakeUpload(t)
	f.failOnce.Store(true)
	size := ChunkSize + 1<<20
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	video, err := c.Upload(context.Background(), "tok", f.session(), bytes.NewReader(make([]byte, size)), size, "video/mp4")
	if err != nil || String(video, "id") != "vid123" {
		t.Fatalf("video=%v err=%v", video, err)
	}
	// probe, failed chunk, probe again, then the rest from the offset Google reported.
	if len(f.ranges) != 4 || f.ranges[2] != fmt.Sprintf("bytes */%d", size) || f.ranges[3] != fmt.Sprintf("bytes %d-%d/%d", ChunkSize/2, size-1, size) {
		t.Fatalf("ranges = %v", f.ranges)
	}
}

func TestAnExpiredSessionIsReported(t *testing.T) {
	f := newFakeUpload(t)
	f.gone.Store(true)
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	if _, err := c.Upload(context.Background(), "tok", f.session(), bytes.NewReader([]byte("x")), 1, "video/mp4"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestACancelledUploadReportsProgressAndKeepsTheSession(t *testing.T) {
	f := newFakeUpload(t)
	size := 3 * ChunkSize
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handle(w, r)
		if !strings.HasPrefix(r.Header.Get("Content-Range"), "bytes */") {
			cancel() // the budget runs out after the first chunk
		}
	}))
	defer server.Close()
	c := fast(NewWithBases(server.URL, server.URL))
	_, err := c.Upload(ctx, "tok", server.URL+"/videos?upload_id=abc", bytes.NewReader(make([]byte, size)), size, "video/mp4")
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Size != size || incomplete.Offset > size {
		t.Fatalf("err = %v, want IncompleteError", err)
	}
}

func TestTheTokenIsNeverSentToAHostOtherThanTheUploadHost(t *testing.T) {
	var hits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer foreign.Close()
	f := newFakeUpload(t)
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	for _, session := range []string{
		foreign.URL + "/videos?upload_id=abc",
		strings.Replace(f.session(), "http://", "http://user:pw@", 1),
		f.server.URL + "/other?upload_id=abc",
		f.server.URL + "/videos#frag",
		"not a url",
		"",
	} {
		if _, err := c.Upload(context.Background(), "tok", session, bytes.NewReader([]byte("x")), 1, "video/mp4"); err == nil {
			t.Errorf("session %q was accepted", session)
		}
	}
	if hits.Load() != 0 || len(f.ranges) != 0 {
		t.Fatalf("a request was sent: foreign=%d upload=%d", hits.Load(), len(f.ranges))
	}
}

func TestStartUploadReturnsTheSessionAndRefusesAForeignLocation(t *testing.T) {
	f := newFakeUpload(t)
	c := fast(NewWithBases(f.server.URL, f.server.URL))
	session, err := c.StartUpload(context.Background(), "tok", Object{"snippet": Object{"title": "t"}}, 10, "video/mp4")
	if err != nil || session != f.session() {
		t.Fatalf("session=%q err=%v", session, err)
	}
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example/videos?x=1")
	}))
	defer evil.Close()
	if _, err := fast(NewWithBases(evil.URL, evil.URL)).StartUpload(context.Background(), "tok", Object{}, 10, "video/mp4"); err == nil {
		t.Fatal("a Location pointing at another host was accepted")
	}
}

func TestRangeParsing(t *testing.T) {
	for header, want := range map[string]int64{"": 0, "bytes=0-0": 1, "bytes=0-8388607": 8388608} {
		if got, err := rangeEnd(header); err != nil || got != want {
			t.Errorf("%q: got %d err %v, want %d", header, got, err, want)
		}
	}
	for _, bad := range []string{"bytes=5-9", "bytes=0-x", "bytes=0--1", "items=0-3", "bytes=0-9223372036854775807"} {
		if _, err := rangeEnd(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
