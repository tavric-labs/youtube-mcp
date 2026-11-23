package tools

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tavric-labs/youtube-mcp/internal/media"
)

func TestReadToolsCallYouTubeAsThePersonWhoSignedIn(t *testing.T) {
	h := newHarness(t)
	result, _, err := h.server.GetChannel(context.Background(), reader(), ChannelArgs{})
	if err != nil || result.IsError {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if h.tokens.seen() != "mcp-bearer" {
		t.Fatalf("the exchange saw %q", h.tokens.seen())
	}
	for _, auth := range h.api.auths {
		if auth != "Bearer member-token" {
			t.Fatalf("YouTube saw %q, want the exchanged personal token", auth)
		}
	}
	if !strings.Contains(text(t, result), "Tavric Labs") || !strings.Contains(text(t, result), "@tavric") {
		t.Fatalf("channel text = %q", text(t, result))
	}
}

func TestEveryToolRefusesAnUnauthenticatedRequestWithoutCallingYouTube(t *testing.T) {
	h := newHarness(t)
	id := h.stage(t, "user-1", 100)
	ctx := context.Background()
	for _, req := range []*mcp.CallToolRequest{{}, request("user-1", ""), request("", "mcp-bearer", WriteScope)} {
		results := []*mcp.CallToolResult{
			first(h.server.GetChannel(ctx, req, ChannelArgs{})),
			first(h.server.ListVideos(ctx, req, VideosArgs{})),
			first(h.server.GetVideo(ctx, req, VideoArgs{VideoID: "vidAAAAAAAA"})),
			first(h.server.UploadVideo(ctx, req, UploadArgs{AssetID: id, Title: "t", MadeForKids: yes(), Confirm: true})),
			first(h.server.UpdateVideo(ctx, req, UpdateArgs{VideoID: "vidAAAAAAAA", Title: "t", Confirm: true})),
			first(h.server.ListPlaylists(ctx, req, PlaylistsArgs{})),
			first(h.server.CreatePlaylist(ctx, req, CreatePlaylistArgs{Title: "t", Confirm: true})),
			first(h.server.AddPlaylistItem(ctx, req, PlaylistItemArgs{PlaylistID: "PLexisting0001", VideoID: "vidAAAAAAAA", Confirm: true})),
			first(h.server.RemovePlaylistItem(ctx, req, RemoveItemArgs{PlaylistItemID: "PLITEM0000001", Confirm: true})),
			first(h.server.ListComments(ctx, req, CommentsArgs{VideoID: "vidAAAAAAAA"})),
			first(h.server.ReplyToComment(ctx, req, ReplyArgs{CommentID: "Ugx111", Text: "hi", Confirm: true})),
			first(h.server.ModerateComment(ctx, req, ModerateArgs{CommentIDs: []string{"Ugx111"}, Status: "rejected", Confirm: true})),
			first(h.server.DeleteComment(ctx, req, DeleteArgs{CommentID: "Ugx111", Confirm: true})),
		}
		for i, r := range results {
			if !r.IsError {
				t.Errorf("tool %d accepted an unauthenticated request: %s", i, text(t, r))
			}
		}
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatalf("an unauthenticated call reached the exchange or YouTube: calls=%d", h.api.callCount())
	}
}

func first(r *mcp.CallToolResult, _ any, _ error) *mcp.CallToolResult { return r }

func TestStateChangingToolsAreDryRunsUnlessConfirmed(t *testing.T) {
	h := newHarness(t)
	id := h.stage(t, "user-1", 100)
	ctx := context.Background()
	for name, r := range map[string]*mcp.CallToolResult{
		"upload":   first(h.server.UploadVideo(ctx, writer(), UploadArgs{AssetID: id, Title: "t", MadeForKids: yes()})),
		"update":   first(h.server.UpdateVideo(ctx, writer(), UpdateArgs{VideoID: "vidAAAAAAAA", Title: "t"})),
		"create":   first(h.server.CreatePlaylist(ctx, writer(), CreatePlaylistArgs{Title: "t"})),
		"add":      first(h.server.AddPlaylistItem(ctx, writer(), PlaylistItemArgs{PlaylistID: "PLexisting0001", VideoID: "vidAAAAAAAA"})),
		"remove":   first(h.server.RemovePlaylistItem(ctx, writer(), RemoveItemArgs{PlaylistItemID: "PLITEM0000001"})),
		"reply":    first(h.server.ReplyToComment(ctx, writer(), ReplyArgs{CommentID: "Ugx111", Text: "hi"})),
		"moderate": first(h.server.ModerateComment(ctx, writer(), ModerateArgs{CommentIDs: []string{"Ugx111"}, Status: "rejected"})),
		"delete":   first(h.server.DeleteComment(ctx, writer(), DeleteArgs{CommentID: "Ugx111"})),
	} {
		if r.IsError || !strings.Contains(strings.ToLower(text(t, r)), "dry run") {
			t.Errorf("%s: result = %q", name, text(t, r))
		}
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatalf("a dry run reached the exchange or YouTube: calls=%v", h.api.calls)
	}
}

func TestStateChangingToolsNeedTheWriteScope(t *testing.T) {
	h := newHarness(t)
	id := h.stage(t, "user-1", 100)
	ctx := context.Background()
	for name, r := range map[string]*mcp.CallToolResult{
		"upload":   first(h.server.UploadVideo(ctx, reader(), UploadArgs{AssetID: id, Title: "t", MadeForKids: yes(), Confirm: true})),
		"update":   first(h.server.UpdateVideo(ctx, reader(), UpdateArgs{VideoID: "vidAAAAAAAA", Title: "t", Confirm: true})),
		"create":   first(h.server.CreatePlaylist(ctx, reader(), CreatePlaylistArgs{Title: "t", Confirm: true})),
		"add":      first(h.server.AddPlaylistItem(ctx, reader(), PlaylistItemArgs{PlaylistID: "PLexisting0001", VideoID: "vidAAAAAAAA", Confirm: true})),
		"remove":   first(h.server.RemovePlaylistItem(ctx, reader(), RemoveItemArgs{PlaylistItemID: "PLITEM0000001", Confirm: true})),
		"reply":    first(h.server.ReplyToComment(ctx, reader(), ReplyArgs{CommentID: "Ugx111", Text: "hi", Confirm: true})),
		"moderate": first(h.server.ModerateComment(ctx, reader(), ModerateArgs{CommentIDs: []string{"Ugx111"}, Status: "rejected", Confirm: true})),
		"delete":   first(h.server.DeleteComment(ctx, reader(), DeleteArgs{CommentID: "Ugx111", Confirm: true})),
	} {
		if !r.IsError || !strings.Contains(text(t, r), WriteScope) {
			t.Errorf("%s: result = %q", name, text(t, r))
		}
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatalf("a read-only token reached the exchange or YouTube: calls=%v", h.api.calls)
	}
}

func TestIdentifiersThatCouldSteerARequestAreRefusedBeforeAnyCall(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []string{"", "../channels", "a/b", "a b", "a&mine=true", "a,b", "x?y=1", "..", strings.Repeat("a", 201)} {
		results := []*mcp.CallToolResult{
			first(h.server.GetVideo(ctx, writer(), VideoArgs{VideoID: bad})),
			first(h.server.UpdateVideo(ctx, writer(), UpdateArgs{VideoID: bad, Title: "t", Confirm: true})),
			first(h.server.AddPlaylistItem(ctx, writer(), PlaylistItemArgs{PlaylistID: bad, VideoID: "vidAAAAAAAA", Confirm: true})),
			first(h.server.AddPlaylistItem(ctx, writer(), PlaylistItemArgs{PlaylistID: "PLexisting0001", VideoID: bad, Confirm: true})),
			first(h.server.RemovePlaylistItem(ctx, writer(), RemoveItemArgs{PlaylistItemID: bad, Confirm: true})),
			first(h.server.ListComments(ctx, writer(), CommentsArgs{VideoID: bad})),
			first(h.server.ReplyToComment(ctx, writer(), ReplyArgs{CommentID: bad, Text: "hi", Confirm: true})),
			first(h.server.ModerateComment(ctx, writer(), ModerateArgs{CommentIDs: []string{bad}, Status: "rejected", Confirm: true})),
			first(h.server.DeleteComment(ctx, writer(), DeleteArgs{CommentID: bad, Confirm: true})),
			first(h.server.UploadVideo(ctx, writer(), UploadArgs{AssetID: bad, Title: "t", MadeForKids: yes(), Confirm: true})),
		}
		for i, r := range results {
			if !r.IsError {
				t.Errorf("id %q was accepted by tool %d", bad, i)
			}
		}
	}
	if h.api.callCount() != 0 || h.tokens.seen() != "" {
		t.Fatalf("an invalid id reached the exchange or YouTube: calls=%v", h.api.calls)
	}
}

func TestListVideosReadsTheUploadsPlaylistNotSearch(t *testing.T) {
	h := newHarness(t)
	result, _, err := h.server.ListVideos(context.Background(), reader(), VideosArgs{MaxResults: 500})
	if err != nil || result.IsError {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	out := text(t, result)
	if !strings.Contains(out, "vidAAAAAAAA | unlisted | GMP scheduler") || !strings.Contains(out, "next_page_token: NEXT1") {
		t.Fatalf("out = %q", out)
	}
	if h.api.count("GET /search") != 0 {
		t.Fatal("list used search, which costs 100 quota units per call")
	}
	if !strings.Contains(h.api.query("GET /playlistItems"), "maxResults=50") || !strings.Contains(h.api.query("GET /playlistItems"), "playlistId=UU1") {
		t.Fatalf("query = %q", h.api.query("GET /playlistItems"))
	}
}

func TestGetVideoShowsProcessingState(t *testing.T) {
	h := newHarness(t)
	result, _, _ := h.server.GetVideo(context.Background(), reader(), VideoArgs{VideoID: "vidAAAAAAAA"})
	for _, want := range []string{"unlisted", "processing: succeeded", "PT48S", "go runtime"} {
		if !strings.Contains(text(t, result), want) && !strings.Contains(text(t, result), strings.Replace(want, " ", "", 1)) {
			t.Errorf("missing %q in %q", want, text(t, result))
		}
	}
	h.api.on("GET /videos", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"items":[]}`) })
	if r, _, _ := h.server.GetVideo(context.Background(), reader(), VideoArgs{VideoID: "vidMISSING01"}); !r.IsError {
		t.Fatal("a missing video was reported as found")
	}
}

func TestUpdateVideoKeepsEverythingNotNamed(t *testing.T) {
	h := newHarness(t)
	result, _, err := h.server.UpdateVideo(context.Background(), writer(), UpdateArgs{VideoID: "vidAAAAAAAA", Privacy: "public", Confirm: true})
	if err != nil || result.IsError {
		t.Fatalf("err=%v result=%q", err, text(t, result))
	}
	body := h.api.body("PUT /videos")
	snippet, status := body["snippet"].(map[string]any), body["status"].(map[string]any)
	if snippet["title"] != "GMP scheduler" || snippet["description"] != "Old description" || snippet["categoryId"] != "28" || snippet["defaultLanguage"] != "en" {
		t.Fatalf("snippet lost fields: %v", snippet)
	}
	if tags, _ := snippet["tags"].([]any); len(tags) != 2 {
		t.Fatalf("tags lost: %v", snippet["tags"])
	}
	if status["privacyStatus"] != "public" || status["license"] != "youtube" || status["selfDeclaredMadeForKids"] != false {
		t.Fatalf("status = %v", status)
	}
	if _, present := status["uploadStatus"]; present {
		t.Fatal("a read-only status field was written back")
	}
	if _, present := status["publishAt"]; present {
		t.Fatal("a schedule was kept on a video made public")
	}
	if h.api.query("PUT /videos") != "part=snippet%2Cstatus" {
		t.Fatalf("query = %q", h.api.query("PUT /videos"))
	}
}

func TestUpdateVideoValidatesInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	long := strings.Repeat("x", 101)
	for name, args := range map[string]UpdateArgs{
		"nothing":      {VideoID: "vidAAAAAAAA", Confirm: true},
		"bad privacy":  {VideoID: "vidAAAAAAAA", Privacy: "secret", Confirm: true},
		"long title":   {VideoID: "vidAAAAAAAA", Title: long, Confirm: true},
		"angle":        {VideoID: "vidAAAAAAAA", Description: "<script>", Confirm: true},
		"bad category": {VideoID: "vidAAAAAAAA", CategoryID: "1/2", Confirm: true},
	} {
		if r, _, _ := h.server.UpdateVideo(ctx, writer(), args); !r.IsError {
			t.Errorf("%s was accepted", name)
		}
	}
	if h.api.callCount() != 0 {
		t.Fatal("invalid input reached YouTube")
	}
}

func TestCreatePlaylistDoesNotMakeADuplicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r, _, _ := h.server.CreatePlaylist(ctx, writer(), CreatePlaylistArgs{Title: "Existing", Confirm: true})
	if r.IsError || !strings.Contains(text(t, r), "already exists") || h.api.count("POST /playlists") != 0 {
		t.Fatalf("duplicate create: %q posts=%d", text(t, r), h.api.count("POST /playlists"))
	}
	r, _, _ = h.server.CreatePlaylist(ctx, writer(), CreatePlaylistArgs{Title: "Go runtime", Description: "d", Confirm: true})
	if r.IsError || h.api.count("POST /playlists") != 1 || !strings.Contains(text(t, r), "PLnew00000001") {
		t.Fatalf("create: %q", text(t, r))
	}
	if status := h.api.body("POST /playlists")["status"].(map[string]any); status["privacyStatus"] != "private" {
		t.Fatalf("a playlist defaults to %v, want private", status["privacyStatus"])
	}
}

func TestPlaylistItemsAreAddedOnceAndRemoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	args := PlaylistItemArgs{PlaylistID: "PLexisting0001", VideoID: "vidAAAAAAAA", Confirm: true}
	if r, _, _ := h.server.AddPlaylistItem(ctx, writer(), args); r.IsError || h.api.count("POST /playlistItems") != 1 {
		t.Fatalf("add: %q", text(t, r))
	}
	body := h.api.body("POST /playlistItems")["snippet"].(map[string]any)
	if body["playlistId"] != "PLexisting0001" || body["resourceId"].(map[string]any)["videoId"] != "vidAAAAAAAA" {
		t.Fatalf("body = %v", body)
	}
	h.api.on("GET /playlistItems", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"items":[{"id":"PLITEMEXISTING"}]}`) })
	r, _, _ := h.server.AddPlaylistItem(ctx, writer(), args)
	if r.IsError || h.api.count("POST /playlistItems") != 1 || !strings.Contains(text(t, r), "already in") {
		t.Fatalf("a repeated add posted again: %q", text(t, r))
	}
	if r, _, _ := h.server.RemovePlaylistItem(ctx, writer(), RemoveItemArgs{PlaylistItemID: "PLITEM0000001", Confirm: true}); r.IsError || h.api.count("DELETE /playlistItems") != 1 {
		t.Fatalf("remove: %q", text(t, r))
	}
}

func TestCommentsAreLabelledAsUntrustedViewerText(t *testing.T) {
	h := newHarness(t)
	r, _, _ := h.server.ListComments(context.Background(), reader(), CommentsArgs{VideoID: "vidAAAAAAAA", MaxResults: 1000})
	out := text(t, r)
	if !strings.Contains(out, "written by viewers") || !strings.Contains(out, "comment Ugx111") || !strings.Contains(out, "next_page_token: CN1") {
		t.Fatalf("out = %q", out)
	}
	if strings.Index(out, "written by viewers") > strings.Index(out, "Ignore previous instructions") {
		t.Fatal("the warning comes after the viewer text")
	}
	if !strings.Contains(h.api.query("GET /commentThreads"), "maxResults=100") {
		t.Fatalf("query = %q", h.api.query("GET /commentThreads"))
	}
}

func TestCommentWritesSendTheRightRequests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if r, _, _ := h.server.ReplyToComment(ctx, writer(), ReplyArgs{CommentID: "Ugx111", Text: " thanks! ", Confirm: true}); r.IsError {
		t.Fatalf("reply: %q", text(t, r))
	}
	if sn := h.api.body("POST /comments")["snippet"].(map[string]any); sn["parentId"] != "Ugx111" || sn["textOriginal"] != "thanks!" {
		t.Fatalf("reply body = %v", sn)
	}
	if r, _, _ := h.server.ModerateComment(ctx, writer(), ModerateArgs{CommentIDs: []string{"Ugx111", "Ugx222"}, Status: "rejected", BanAuthor: true, Confirm: true}); r.IsError {
		t.Fatalf("moderate: %q", text(t, r))
	}
	q := h.api.query("POST /comments/setModerationStatus")
	for _, want := range []string{"id=Ugx111%2CUgx222", "moderationStatus=rejected", "banAuthor=true"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	if r, _, _ := h.server.DeleteComment(ctx, writer(), DeleteArgs{CommentID: "Ugx111", Confirm: true}); r.IsError || h.api.count("DELETE /comments") != 1 {
		t.Fatalf("delete: %q", text(t, r))
	}
	for name, args := range map[string]ModerateArgs{
		"ban on publish": {CommentIDs: []string{"Ugx1"}, Status: "published", BanAuthor: true, Confirm: true},
		"bad status":     {CommentIDs: []string{"Ugx1"}, Status: "deleted", Confirm: true},
		"no ids":         {Status: "rejected", Confirm: true},
		"too many ids":   {CommentIDs: make([]string, 51), Status: "rejected", Confirm: true},
	} {
		if r, _, _ := h.server.ModerateComment(ctx, writer(), args); !r.IsError {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAReplyIsNotRetriedAfterAServerError(t *testing.T) {
	h := newHarness(t)
	h.api.on("POST /comments", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	r, _, _ := h.server.ReplyToComment(context.Background(), writer(), ReplyArgs{CommentID: "Ugx111", Text: "hi", Confirm: true})
	if !r.IsError || h.api.count("POST /comments") != 1 {
		t.Fatalf("result=%q posts=%d, want one attempt", text(t, r), h.api.count("POST /comments"))
	}
}

func TestFailuresAreExplainedWithoutLeakingUpstreamDetail(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"expired":   {401, `{"error":{"message":"secret-token-detail"}}`, "sign in again"},
		"quota":     {403, `{"error":{"errors":[{"reason":"quotaExceeded"}],"message":"secret-quota-detail"}}`, "quota"},
		"forbidden": {403, `{"error":{"errors":[{"reason":"forbidden"}],"message":"secret-forbidden-detail"}}`, "refused"},
		"missing":   {404, `{"error":{"message":"secret-missing-detail"}}`, "could not find"},
		"server":    {500, `secret-server-detail`, "could not complete"},
	}
	for name, tc := range cases {
		h := newHarness(t)
		h.api.on("GET /channels", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		r, _, _ := h.server.GetChannel(context.Background(), reader(), ChannelArgs{})
		got := strings.ToLower(text(t, r))
		if !r.IsError || !strings.Contains(got, tc.want) || strings.Contains(got, "secret") {
			t.Errorf("%s: %q", name, got)
		}
	}
	h := newHarness(t)
	h.tokens.err = errors.New("token exchange returned 500: secret-upstream-detail")
	r, _, _ := h.server.GetChannel(context.Background(), reader(), ChannelArgs{})
	if !r.IsError || strings.Contains(text(t, r), "secret-upstream-detail") || h.api.callCount() != 0 {
		t.Fatalf("exchange failure: %q", text(t, r))
	}
}

func TestAccountWithoutAChannelIsExplained(t *testing.T) {
	h := newHarness(t)
	h.api.on("GET /channels", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{}`) })
	r, _, _ := h.server.GetChannel(context.Background(), reader(), ChannelArgs{})
	if !r.IsError || !strings.Contains(text(t, r), "no YouTube channel") {
		t.Fatalf("result = %q", text(t, r))
	}
}

func TestEveryToolIsRegisteredWithAValidSchema(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	(&Server{}).Register(server) // AddTool panics on a schema it cannot build
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	session, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"youtube_get_channel", "youtube_list_videos", "youtube_get_video", "youtube_create_upload_url", "youtube_upload_video", "youtube_update_video", "youtube_list_playlists", "youtube_create_playlist", "youtube_add_playlist_item", "youtube_remove_playlist_item", "youtube_list_comments", "youtube_reply_to_comment", "youtube_moderate_comment", "youtube_delete_comment"} {
		if !names[want] {
			t.Errorf("tool %s is not registered", want)
		}
	}
	if len(names) != 14 {
		t.Errorf("%d tools registered, want 14", len(names))
	}
}

func newUploadURLHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.server.Tickets = &media.Tickets{}
	h.server.UploadBase = "https://mcp.example.com/youtube/mcp"
	return h
}

func TestCreateUploadURLIssuesAOneTimeURLForTheCaller(t *testing.T) {
	h := newUploadURLHarness(t)
	r, _, err := h.server.CreateUploadURL(context.Background(), writer(), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: 5 << 20})
	if err != nil || r.IsError {
		t.Fatalf("CreateUploadURL() = %q, %v", text(t, r), err)
	}
	body := text(t, r)
	if !strings.Contains(body, "https://mcp.example.com/youtube/mcp/media/u/") || !strings.Contains(body, "--data-binary") || !strings.Contains(body, "video/mp4") {
		t.Fatalf("reply does not carry a usable upload command: %s", body)
	}
	if strings.Contains(body, "-H \"Authorization") || strings.Contains(body, "Bearer") {
		t.Fatalf("the curl command must not carry a bearer token: %s", body)
	}
	if h.tokens.seen() != "" {
		t.Fatal("issuing a URL must not exchange the caller's token with Google")
	}
}

func TestCreateUploadURLChecksScopeAndInput(t *testing.T) {
	h := newUploadURLHarness(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		req  *mcp.CallToolRequest
		args CreateUploadURLArgs
		want string
	}{
		{"read-only token", reader(), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: 10}, WriteScope},
		{"unauthenticated", request("", ""), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: 10}, "authenticated"},
		{"unsupported type", writer(), CreateUploadURLArgs{ContentType: "image/png", SizeBytes: 10}, "video/mp4"},
		{"zero size", writer(), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: 0}, "size_bytes"},
		{"over the limit", writer(), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: media.MaxBytes + 1}, "2 GiB"},
	} {
		r, _, err := h.server.CreateUploadURL(ctx, tc.req, tc.args)
		if err != nil || !r.IsError || !strings.Contains(text(t, r), tc.want) {
			t.Errorf("%s: got %q (error=%v), want an error mentioning %q", tc.name, text(t, r), r.IsError, tc.want)
		}
	}
	if _, _, err := (&Server{}).CreateUploadURL(ctx, writer(), CreateUploadURLArgs{ContentType: "video/mp4", SizeBytes: 10}); err != nil {
		t.Fatalf("an unconfigured server must answer with a tool error, not fail: %v", err)
	}
}
