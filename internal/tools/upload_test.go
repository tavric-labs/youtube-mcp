package tools

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tavric-labs/youtube-mcp/internal/youtube"
)

func uploadArgs(id string) UploadArgs {
	return UploadArgs{AssetID: id, Title: "Go scheduler", Description: "How it works", Tags: []string{"go"}, MadeForKids: yes(), Confirm: true}
}

func TestUploadSendsTheStagedFileAsAPrivateVideoByDefault(t *testing.T) {
	h := newHarness(t)
	id := h.stage(t, "user-1", 4096)
	r, _, err := h.server.UploadVideo(context.Background(), writer(), uploadArgs(id))
	if err != nil || r.IsError {
		t.Fatalf("err=%v result=%q", err, text(t, r))
	}
	if !strings.Contains(text(t, r), "https://youtu.be/vid123") {
		t.Fatalf("result = %q", text(t, r))
	}
	if h.api.starts != 1 || h.api.uploadFinish != 1 {
		t.Fatalf("starts=%d finishes=%d", h.api.starts, h.api.uploadFinish)
	}
	if !strings.Contains(string(h.api.startBody), `"privacyStatus":"private"`) || !strings.Contains(string(h.api.startBody), `"selfDeclaredMadeForKids":true`) {
		t.Fatalf("start body = %s", h.api.startBody)
	}
	for _, auth := range h.api.auths {
		if auth != "Bearer member-token" {
			t.Fatalf("YouTube saw %q", auth)
		}
	}
}

func TestUploadNeedsExplicitMadeForKidsAndValidDetails(t *testing.T) {
	h := newHarness(t)
	id := h.stage(t, "user-1", 100)
	ctx := context.Background()
	base := uploadArgs(id)
	noKids := base
	noKids.MadeForKids = nil
	badPrivacy := base
	badPrivacy.Privacy = "friends"
	noTitle := base
	noTitle.Title = "  "
	longTitle := base
	longTitle.Title = strings.Repeat("t", 101)
	angle := base
	angle.Description = "<b>hi</b>"
	manyTags := base
	manyTags.Tags = []string{strings.Repeat("k", 300), strings.Repeat("k", 300)}
	badCategory := base
	badCategory.CategoryID = "../x"
	for name, args := range map[string]UploadArgs{"no kids": noKids, "privacy": badPrivacy, "no title": noTitle, "long title": longTitle, "angle": angle, "tags": manyTags, "category": badCategory} {
		if r, _, _ := h.server.UploadVideo(ctx, writer(), args); !r.IsError {
			t.Errorf("%s was accepted", name)
		}
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatal("invalid input reached the exchange or YouTube")
	}
}

func TestOnePersonCannotUploadAnotherPersonsStagedFile(t *testing.T) {
	h := newHarness(t)
	bobsAsset := h.stage(t, "user-2", 2048)
	r, _, _ := h.server.UploadVideo(context.Background(), writer(), uploadArgs(bobsAsset)) // writer() is user-1
	if !r.IsError || !strings.Contains(text(t, r), "not found for this account") {
		t.Fatalf("result = %q", text(t, r))
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatal("a foreign asset reached the exchange or YouTube")
	}
	// A dry run must not reveal that the asset exists either.
	dry := uploadArgs(bobsAsset)
	dry.Confirm = false
	if r, _, _ := h.server.UploadVideo(context.Background(), writer(), dry); !r.IsError || !strings.Contains(text(t, r), "not found for this account") {
		t.Fatalf("dry run result = %q", text(t, r))
	}
	// Bob's own upload state is untouched by the attempt.
	if state, err := h.media.State("user-2", bobsAsset); err != nil || state.Session != "" || state.Result != nil {
		t.Fatalf("bob's state = %+v err=%v", state, err)
	}
}

func TestOnePersonCannotResumeAnotherPersonsUploadSession(t *testing.T) {
	h := newHarness(t)
	size := int(youtube.ChunkSize) + 1<<20
	asset := h.stage(t, "user-1", size)
	delay := make(chan struct{})
	defer close(delay)
	h.api.chunkDelay = delay
	h.server.UploadBudget = 300 * time.Millisecond
	if r, _, _ := h.server.UploadVideo(context.Background(), writer(), uploadArgs(asset)); r.IsError || !strings.Contains(text(t, r), "in progress") {
		t.Fatalf("setup: %q", text(t, r))
	}
	if state, _ := h.media.State("user-1", asset); state.Session == "" {
		t.Fatal("no session was saved to resume")
	}
	// user-2 holds the same asset id string but no such asset exists in their namespace.
	other := request("user-2", "mcp-bearer-2", "tools:read", WriteScope)
	r, _, _ := h.server.UploadVideo(context.Background(), other, uploadArgs(asset))
	if !r.IsError || h.api.starts != 1 {
		t.Fatalf("result=%q starts=%d", text(t, r), h.api.starts)
	}
}

func TestARetriedUploadResumesTheSavedSessionAndNeverUploadsTwice(t *testing.T) {
	h := newHarness(t)
	size := int(youtube.ChunkSize) + 1<<20
	asset := h.stage(t, "user-1", size)
	delay := make(chan struct{})
	defer close(delay)
	h.api.chunkDelay = delay // the first chunk stalls until the budget ends
	h.server.UploadBudget = 300 * time.Millisecond
	ctx := context.Background()

	first, _, _ := h.server.UploadVideo(ctx, writer(), uploadArgs(asset))
	if first.IsError || !strings.Contains(text(t, first), "in progress") || strings.Contains(text(t, first), "http") {
		t.Fatalf("first call = %q", text(t, first))
	}
	state, _ := h.media.State("user-1", asset)
	if state.Session == "" || state.Fingerprint == "" {
		t.Fatalf("saved state = %+v", state)
	}

	h.server.UploadBudget = 30 * time.Second
	second, _, _ := h.server.UploadVideo(ctx, writer(), uploadArgs(asset))
	if second.IsError || !strings.Contains(text(t, second), "https://youtu.be/vid123") {
		t.Fatalf("second call = %q", text(t, second))
	}
	if h.api.starts != 1 {
		t.Fatalf("the retry opened %d sessions, want to reuse the first", h.api.starts)
	}

	third, _, _ := h.server.UploadVideo(ctx, writer(), uploadArgs(asset))
	if third.IsError || !strings.Contains(text(t, third), "Already uploaded") || h.api.starts != 1 || h.api.uploadFinish != 1 {
		t.Fatalf("third call = %q starts=%d finishes=%d", text(t, third), h.api.starts, h.api.uploadFinish)
	}
	if state, _ := h.media.State("user-1", asset); state.Session != "" || state.Result == nil {
		t.Fatalf("final state = %+v, want the session dropped and the result kept", state)
	}
}

func TestChangedDetailsAfterAnUploadPointToUpdateInsteadOfUploadingAgain(t *testing.T) {
	h := newHarness(t)
	asset := h.stage(t, "user-1", 4096)
	ctx := context.Background()
	if r, _, _ := h.server.UploadVideo(ctx, writer(), uploadArgs(asset)); r.IsError {
		t.Fatalf("setup: %q", text(t, r))
	}
	changed := uploadArgs(asset)
	changed.Title = "A different title"
	r, _, _ := h.server.UploadVideo(ctx, writer(), changed)
	if !r.IsError || !strings.Contains(text(t, r), "vid123") || !strings.Contains(text(t, r), "youtube_update_video") || h.api.starts != 1 {
		t.Fatalf("result=%q starts=%d", text(t, r), h.api.starts)
	}
}

func TestDifferentDetailsBeforeCompletionStartAFreshSession(t *testing.T) {
	h := newHarness(t)
	size := int(youtube.ChunkSize) + 1<<20
	asset := h.stage(t, "user-1", size)
	delay := make(chan struct{})
	defer close(delay)
	h.api.chunkDelay = delay
	h.server.UploadBudget = 300 * time.Millisecond
	ctx := context.Background()
	if r, _, _ := h.server.UploadVideo(ctx, writer(), uploadArgs(asset)); r.IsError {
		t.Fatal(text(t, r))
	}
	changed := uploadArgs(asset)
	changed.Title = "Better title"
	h.server.UploadBudget = 30 * time.Second
	if r, _, _ := h.server.UploadVideo(ctx, writer(), changed); r.IsError || h.api.starts != 2 {
		t.Fatalf("result=%q starts=%d, want a second session bound to the new details", text(t, r), h.api.starts)
	}
	if !strings.Contains(string(h.api.startBody), "Better title") {
		t.Fatalf("the new session did not carry the new title: %s", h.api.startBody)
	}
}

func TestAnExpiredSessionIsReplacedOnce(t *testing.T) {
	h := newHarness(t)
	asset := h.stage(t, "user-1", 4096)
	h.api.probeStatus = http.StatusNotFound // Google no longer knows the first session
	r, _, _ := h.server.UploadVideo(context.Background(), writer(), uploadArgs(asset))
	if r.IsError || !strings.Contains(text(t, r), "vid123") || h.api.starts != 2 {
		t.Fatalf("result=%q starts=%d", text(t, r), h.api.starts)
	}
}

func TestUploadFailuresAreExplainedAndKeepTheStagedFile(t *testing.T) {
	h := newHarness(t)
	asset := h.stage(t, "user-1", 4096)
	h.api.on("POST /upload/youtube/v3/videos", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"quotaExceeded"}],"message":"secret-quota-detail"}}`))
	})
	r, _, _ := h.server.UploadVideo(context.Background(), writer(), uploadArgs(asset))
	if !r.IsError || !strings.Contains(strings.ToLower(text(t, r)), "quota") || strings.Contains(text(t, r), "secret") {
		t.Fatalf("result = %q", text(t, r))
	}
	if file, _, err := h.media.Open("user-1", asset); err != nil {
		t.Fatalf("the staged file was lost: %v", err)
	} else {
		file.Close()
	}
}

func TestAnUnverifiedProjectKeepingAVideoPrivateIsExplained(t *testing.T) {
	h := newHarness(t)
	h.api.keepPrivate = true
	asset := h.stage(t, "user-1", 4096)
	args := uploadArgs(asset)
	args.Privacy = "public"
	r, _, _ := h.server.UploadVideo(context.Background(), writer(), args)
	if r.IsError || !strings.Contains(text(t, r), "kept it private") || !strings.Contains(text(t, r), "audit") {
		t.Fatalf("result = %q", text(t, r))
	}
}

func TestSavedSessionsAreNeverShownToTheCaller(t *testing.T) {
	h := newHarness(t)
	size := int(youtube.ChunkSize) + 1<<20
	asset := h.stage(t, "user-1", size)
	delay := make(chan struct{})
	defer close(delay)
	h.api.chunkDelay = delay
	h.server.UploadBudget = 300 * time.Millisecond
	r, _, _ := h.server.UploadVideo(context.Background(), writer(), uploadArgs(asset))
	if strings.Contains(text(t, r), "upload_id") || strings.Contains(text(t, r), h.api.server.URL) {
		t.Fatalf("the session URI reached the caller: %q", text(t, r))
	}
	state, _ := h.media.State("user-1", asset)
	var found bool
	_ = filepath.Walk(h.media.Root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".upload.json") {
			found = true
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s has mode %v", path, info.Mode().Perm())
			}
		}
		return nil
	})
	if !found || state.Session == "" {
		t.Fatal("the session was not persisted privately")
	}
}
