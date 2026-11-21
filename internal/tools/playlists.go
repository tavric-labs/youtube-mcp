package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tavric-labs/youtube-mcp/internal/youtube"
)

// PlaylistsArgs are the inputs of youtube_list_playlists.
type PlaylistsArgs struct {
	MaxResults int    `json:"max_results,omitempty" jsonschema:"how many playlists, 1 to 50, default 25"`
	PageToken  string `json:"page_token,omitempty" jsonschema:"next_page_token from the previous call"`
}

// ListPlaylists lists the signed-in person's playlists.
func (s *Server) ListPlaylists(ctx context.Context, request *mcp.CallToolRequest, args PlaylistsArgs) (*mcp.CallToolResult, any, error) {
	if args.PageToken != "" && !validID(args.PageToken) {
		return result("page_token is not valid.", true), nil, nil
	}
	c, refusal := s.member(ctx, request)
	if refusal != nil {
		return refusal, nil, nil
	}
	query := url.Values{"part": {"snippet,status,contentDetails"}, "mine": {"true"}, "maxResults": {page(args.MaxResults, 25, 50)}}
	if args.PageToken != "" {
		query.Set("pageToken", args.PageToken)
	}
	response, err := s.YouTube.Get(ctx, c.token, "playlists", query)
	if err != nil {
		return failure("youtube_list_playlists", c.subject, err), nil, nil
	}
	lines := playlistLines(response)
	if len(lines) == 0 {
		return ok("youtube_list_playlists", c.subject, "No playlists."), nil, nil
	}
	text := "id | privacy | items | title\n" + strings.Join(lines, "\n")
	if next := youtube.String(response, "nextPageToken"); next != "" {
		text += "\nnext_page_token: " + next
	}
	return ok("youtube_list_playlists", c.subject, text, "count", len(lines)), nil, nil
}

func playlistLines(response youtube.Object) []string {
	var lines []string
	for _, item := range youtube.Items(response) {
		count, _ := youtube.Child(item, "contentDetails")["itemCount"].(float64)
		lines = append(lines, fmt.Sprintf("%s | %s | %d | %s", youtube.String(item, "id"),
			youtube.String(youtube.Child(item, "status"), "privacyStatus"), int(count), clip(youtube.String(youtube.Child(item, "snippet"), "title"), 80)))
	}
	return lines
}

// CreatePlaylistArgs are the inputs of youtube_create_playlist.
type CreatePlaylistArgs struct {
	Title       string `json:"title" jsonschema:"playlist title, up to 150 characters"`
	Description string `json:"description,omitempty" jsonschema:"optional description"`
	Privacy     string `json:"privacy,omitempty" jsonschema:"private (default), unlisted or public"`
	Confirm     bool   `json:"confirm,omitempty" jsonschema:"set true to create; without it the call is a dry run"`
}

// CreatePlaylist creates a playlist, or returns the person's existing playlist
// with the same title so a retried call does not make a second one.
func (s *Server) CreatePlaylist(ctx context.Context, request *mcp.CallToolRequest, args CreatePlaylistArgs) (*mcp.CallToolResult, any, error) {
	title := strings.TrimSpace(args.Title)
	if err := validateText(title, args.Description, nil); err != nil || len([]rune(title)) > 150 {
		return result("A playlist needs a title of up to 150 characters, and a description of up to 5000 bytes, with no < or >.", true), nil, nil
	}
	privacy := args.Privacy
	if privacy == "" {
		privacy = "private"
	}
	if !privacyValues[privacy] {
		return result("privacy must be private, unlisted or public.", true), nil, nil
	}
	c, refusal := s.write(ctx, request, args.Confirm, fmt.Sprintf("This would create the %s playlist %q.", privacy, title))
	if refusal != nil {
		return refusal, nil, nil
	}
	pageToken := ""
	for pages := 0; pages < 4; pages++ {
		query := url.Values{"part": {"snippet"}, "mine": {"true"}, "maxResults": {"50"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		existing, err := s.YouTube.Get(ctx, c.token, "playlists", query)
		if err != nil {
			return failure("youtube_create_playlist", c.subject, err), nil, nil
		}
		for _, item := range youtube.Items(existing) {
			if youtube.String(youtube.Child(item, "snippet"), "title") == title {
				return ok("youtube_create_playlist", c.subject, fmt.Sprintf("A playlist titled %q already exists: id %s. Nothing was created.", title, youtube.String(item, "id")), "duplicate", true), nil, nil
			}
		}
		if pageToken = youtube.String(existing, "nextPageToken"); pageToken == "" {
			break
		}
	}
	body := youtube.Object{
		"snippet": youtube.Object{"title": title, "description": args.Description},
		"status":  youtube.Object{"privacyStatus": privacy},
	}
	created, err := s.YouTube.Call(ctx, c.token, http.MethodPost, "playlists", url.Values{"part": {"snippet,status"}}, body)
	if err != nil {
		return failure("youtube_create_playlist", c.subject, err), nil, nil
	}
	return ok("youtube_create_playlist", c.subject, fmt.Sprintf("Created playlist %q: id %s.", title, youtube.String(created, "id")), "playlist", youtube.String(created, "id")), nil, nil
}

// PlaylistItemArgs are the inputs of youtube_add_playlist_item.
type PlaylistItemArgs struct {
	PlaylistID string `json:"playlist_id" jsonschema:"the playlist id"`
	VideoID    string `json:"video_id" jsonschema:"the video id to add"`
	Confirm    bool   `json:"confirm,omitempty" jsonschema:"set true to add; without it the call is a dry run"`
}

// AddPlaylistItem adds a video to a playlist unless it is already there.
func (s *Server) AddPlaylistItem(ctx context.Context, request *mcp.CallToolRequest, args PlaylistItemArgs) (*mcp.CallToolResult, any, error) {
	if !validID(args.PlaylistID) || !validID(args.VideoID) {
		return result("playlist_id and video_id must be valid YouTube ids.", true), nil, nil
	}
	c, refusal := s.write(ctx, request, args.Confirm, fmt.Sprintf("This would add video %s to playlist %s.", args.VideoID, args.PlaylistID))
	if refusal != nil {
		return refusal, nil, nil
	}
	existing, err := s.YouTube.Get(ctx, c.token, "playlistItems", url.Values{"part": {"id"}, "playlistId": {args.PlaylistID}, "videoId": {args.VideoID}, "maxResults": {"1"}})
	if err != nil {
		return failure("youtube_add_playlist_item", c.subject, err), nil, nil
	}
	if items := youtube.Items(existing); len(items) > 0 {
		return ok("youtube_add_playlist_item", c.subject, fmt.Sprintf("Video %s is already in that playlist (item %s). Nothing was added.", args.VideoID, youtube.String(items[0], "id")), "duplicate", true), nil, nil
	}
	body := youtube.Object{"snippet": youtube.Object{
		"playlistId": args.PlaylistID,
		"resourceId": youtube.Object{"kind": "youtube#video", "videoId": args.VideoID},
	}}
	created, err := s.YouTube.Call(ctx, c.token, http.MethodPost, "playlistItems", url.Values{"part": {"snippet"}}, body)
	if err != nil {
		return failure("youtube_add_playlist_item", c.subject, err), nil, nil
	}
	return ok("youtube_add_playlist_item", c.subject, fmt.Sprintf("Added video %s to playlist %s (item %s).", args.VideoID, args.PlaylistID, youtube.String(created, "id")), "item", youtube.String(created, "id")), nil, nil
}

// RemoveItemArgs are the inputs of youtube_remove_playlist_item.
type RemoveItemArgs struct {
	PlaylistItemID string `json:"playlist_item_id" jsonschema:"the playlist entry id (not the video id)"`
	Confirm        bool   `json:"confirm,omitempty" jsonschema:"set true to remove; without it the call is a dry run"`
}

// RemovePlaylistItem removes one playlist entry; the video stays on YouTube.
func (s *Server) RemovePlaylistItem(ctx context.Context, request *mcp.CallToolRequest, args RemoveItemArgs) (*mcp.CallToolResult, any, error) {
	if !validID(args.PlaylistItemID) {
		return result("playlist_item_id is not valid.", true), nil, nil
	}
	c, refusal := s.write(ctx, request, args.Confirm, "This would remove playlist entry "+args.PlaylistItemID+". The video itself is not deleted.")
	if refusal != nil {
		return refusal, nil, nil
	}
	if _, err := s.YouTube.Call(ctx, c.token, http.MethodDelete, "playlistItems", url.Values{"id": {args.PlaylistItemID}}, nil); err != nil {
		return failure("youtube_remove_playlist_item", c.subject, err), nil, nil
	}
	return ok("youtube_remove_playlist_item", c.subject, "Removed playlist entry "+args.PlaylistItemID+".", "item", args.PlaylistItemID), nil, nil
}
