package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tavric-labs/youtube-mcp/internal/media"
	"github.com/tavric-labs/youtube-mcp/internal/youtube"
)

const (
	maxTitleRunes       = 100
	maxDescriptionBytes = 5000
	maxTagChars         = 500
	defaultCategory     = "22" // People & Blogs
	defaultUploadBudget = 40 * time.Second
)

var privacyValues = map[string]bool{"private": true, "unlisted": true, "public": true}

// UploadArgs are the inputs of youtube_upload_video.
type UploadArgs struct {
	AssetID     string   `json:"asset_id" jsonschema:"asset id returned by POST /media"`
	Title       string   `json:"title" jsonschema:"video title, up to 100 characters, no < or >"`
	Description string   `json:"description,omitempty" jsonschema:"description, up to 5000 bytes, no < or >"`
	Tags        []string `json:"tags,omitempty" jsonschema:"keywords; all together up to 500 characters"`
	CategoryID  string   `json:"category_id,omitempty" jsonschema:"YouTube category id, default 22"`
	Privacy     string   `json:"privacy,omitempty" jsonschema:"private (default), unlisted or public"`
	MadeForKids *bool    `json:"made_for_kids" jsonschema:"required: whether the video is made for kids (a legal declaration, no default)"`
	Confirm     bool     `json:"confirm,omitempty" jsonschema:"set true to upload; without it the call is a dry run"`
}

// validateText applies the limits YouTube enforces on titles, descriptions and tags.
func validateText(title, description string, tags []string) error {
	switch {
	case strings.TrimSpace(title) == "":
		return errors.New("title is required.")
	case utf8.RuneCountInString(title) > maxTitleRunes:
		return fmt.Errorf("the title is longer than %d characters.", maxTitleRunes)
	case len(description) > maxDescriptionBytes:
		return fmt.Errorf("the description is longer than %d bytes.", maxDescriptionBytes)
	case strings.ContainsAny(title+description, "<>"):
		return errors.New("YouTube does not accept < or > in a title or description.")
	}
	total := 0
	for _, tag := range tags {
		total += utf8.RuneCountInString(tag) + 1
		if strings.ContainsAny(tag, "<>") || strings.TrimSpace(tag) == "" {
			return errors.New("tags must be non-empty and contain no < or >.")
		}
	}
	if total > maxTagChars {
		return fmt.Errorf("the tags are longer than %d characters together.", maxTagChars)
	}
	return nil
}

// UploadVideo uploads a staged video to the signed-in person's channel.
//
// It is built to be called again after any failure. Progress (the resumable
// session) is saved per person and asset, bound by a fingerprint of the file
// and the requested details; a repeat of the same request resumes or returns
// the finished video, and never uploads the file twice.
func (s *Server) UploadVideo(ctx context.Context, request *mcp.CallToolRequest, args UploadArgs) (*mcp.CallToolResult, any, error) {
	if !media.ValidID(args.AssetID) {
		return result("asset_id is not valid. Stage the file with POST /media first.", true), nil, nil
	}
	if err := validateText(args.Title, args.Description, args.Tags); err != nil {
		return result(err.Error(), true), nil, nil
	}
	privacy := args.Privacy
	if privacy == "" {
		privacy = "private"
	}
	if !privacyValues[privacy] {
		return result("privacy must be private, unlisted or public.", true), nil, nil
	}
	if args.MadeForKids == nil {
		return result("made_for_kids is required: say true or false. It is a declaration under child-protection law and is not guessed.", true), nil, nil
	}
	category := args.CategoryID
	if category == "" {
		category = defaultCategory
	}
	if !validID(category) {
		return result("category_id is not valid.", true), nil, nil
	}
	subject := subjectOf(request)
	if subject == "" {
		return result("This call is not authenticated.", true), nil, nil
	}
	file, assetMeta, err := s.Media.Open(subject, args.AssetID)
	if err != nil {
		return result(err.Error(), true), nil, nil
	}
	defer file.Close()

	metadata := youtube.Object{
		"snippet": youtube.Object{"title": args.Title, "description": args.Description, "tags": args.Tags, "categoryId": category},
		"status":  youtube.Object{"privacyStatus": privacy, "selfDeclaredMadeForKids": *args.MadeForKids, "embeddable": true},
	}
	fingerprint, err := media.Fingerprint(assetMeta, metadata)
	if err != nil {
		return result("Could not prepare the upload.", true), nil, nil
	}
	preview := fmt.Sprintf("This would upload %s (%d MiB) as %q, privacy %s, made for kids: %t.", assetMeta.MIME, assetMeta.Size>>20, args.Title, privacy, *args.MadeForKids)
	c, refusal := s.write(ctx, request, args.Confirm, preview)
	if refusal != nil {
		return refusal, nil, nil
	}

	release := s.Media.Lock(c.subject, args.AssetID)
	defer release()
	state, err := s.Media.State(c.subject, args.AssetID)
	if err != nil {
		return result("Could not read the saved upload state.", true), nil, nil
	}
	if state.Result != nil {
		if state.Fingerprint == fingerprint {
			return ok("youtube_upload_video", c.subject, finished(state.Result, privacy, "Already uploaded; nothing was sent again."), "video", youtube.String(state.Result, "id")), nil, nil
		}
		return result(fmt.Sprintf("This file was already uploaded as video %s with different details. Use youtube_update_video to change them, or stage the file again to upload a second copy.", youtube.String(state.Result, "id")), true), nil, nil
	}
	if state.Fingerprint != fingerprint {
		state = media.UploadState{Fingerprint: fingerprint}
	}

	budget := s.UploadBudget
	if budget <= 0 {
		budget = defaultUploadBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if state.Session == "" {
			session, err := s.YouTube.StartUpload(ctx, c.token, metadata, assetMeta.Size, assetMeta.MIME)
			if err != nil {
				return failure("youtube_upload_video", c.subject, err), nil, nil
			}
			state.Session = session
			if err := s.Media.Save(c.subject, args.AssetID, state); err != nil {
				return result("Could not save the upload state, so nothing was sent.", true), nil, nil
			}
		}
		video, err := s.YouTube.Upload(ctx, c.token, state.Session, file, assetMeta.Size, assetMeta.MIME)
		var incomplete *youtube.IncompleteError
		switch {
		case err == nil:
			state.Session, state.Result = "", video
			if err := s.Media.Save(c.subject, args.AssetID, state); err != nil {
				// The video exists. A retry finds it through the session probe.
				return result(fmt.Sprintf("Uploaded as video %s, but the local record could not be saved; do not upload again.", youtube.String(video, "id")), false), nil, nil
			}
			return ok("youtube_upload_video", c.subject, finished(video, privacy, ""), "video", youtube.String(video, "id"), "bytes", assetMeta.Size), nil, nil
		case errors.Is(err, youtube.ErrSessionExpired):
			// Google dropped the session before the video was created: start over once.
			state.Session = ""
			continue
		case errors.As(err, &incomplete):
			return ok("youtube_upload_video", c.subject, fmt.Sprintf("Upload in progress: %d of %d MiB sent. Call youtube_upload_video again with the same arguments to continue; it resumes where it stopped and never uploads twice.", incomplete.Offset>>20, incomplete.Size>>20), "progress", incomplete.Offset), nil, nil
		default:
			return failure("youtube_upload_video", c.subject, err), nil, nil
		}
	}
	return result("YouTube kept ending the upload session. Try again shortly.", true), nil, nil
}

func finished(video youtube.Object, requested, prefix string) string {
	id := youtube.String(video, "id")
	privacy := youtube.String(youtube.Child(video, "status"), "privacyStatus")
	text := fmt.Sprintf("Video uploaded: https://youtu.be/%s (id %s), privacy %s. YouTube may still be processing it.", id, id, privacy)
	if privacy == "private" && requested != "private" {
		text += " It was requested as " + requested + ", but YouTube kept it private: Google locks uploads from API projects that have not passed its audit. Change the privacy in YouTube Studio, or complete the API audit."
	}
	if prefix != "" {
		text = prefix + " " + text
	}
	return text
}

// UpdateArgs are the inputs of youtube_update_video. Anything left out is kept.
type UpdateArgs struct {
	VideoID     string   `json:"video_id" jsonschema:"the YouTube video id"`
	Title       string   `json:"title,omitempty" jsonschema:"new title"`
	Description string   `json:"description,omitempty" jsonschema:"new description"`
	Tags        []string `json:"tags,omitempty" jsonschema:"replacement tags; an empty list clears them"`
	CategoryID  string   `json:"category_id,omitempty" jsonschema:"new category id"`
	Privacy     string   `json:"privacy,omitempty" jsonschema:"private, unlisted or public"`
	Confirm     bool     `json:"confirm,omitempty" jsonschema:"set true to apply; without it the call is a dry run"`
}

// writableStatus lists the video status fields videos.update accepts.
var writableStatus = []string{"privacyStatus", "embeddable", "license", "publicStatsViewable", "selfDeclaredMadeForKids", "publishAt"}

// UpdateVideo changes a video's details. videos.update replaces whole parts,
// so it reads the current video, merges the requested fields and writes the
// result back; fields the caller did not name keep their values.
func (s *Server) UpdateVideo(ctx context.Context, request *mcp.CallToolRequest, args UpdateArgs) (*mcp.CallToolResult, any, error) {
	if !validID(args.VideoID) {
		return result("video_id is not a valid YouTube video id.", true), nil, nil
	}
	if args.Title == "" && args.Description == "" && args.Tags == nil && args.CategoryID == "" && args.Privacy == "" {
		return result("Nothing to change: give at least one of title, description, tags, category_id or privacy.", true), nil, nil
	}
	if args.Title != "" || args.Description != "" || args.Tags != nil {
		title := args.Title
		if title == "" {
			title = "kept" // checked against the real title after loading
		}
		if err := validateText(title, args.Description, args.Tags); err != nil {
			return result(err.Error(), true), nil, nil
		}
	}
	if args.Privacy != "" && !privacyValues[args.Privacy] {
		return result("privacy must be private, unlisted or public.", true), nil, nil
	}
	if args.CategoryID != "" && !validID(args.CategoryID) {
		return result("category_id is not valid.", true), nil, nil
	}
	c, refusal := s.write(ctx, request, args.Confirm, "This would update video "+args.VideoID+": "+changes(args)+".")
	if refusal != nil {
		return refusal, nil, nil
	}
	video, refusal := s.loadVideo(ctx, c, "youtube_update_video", args.VideoID, "snippet,status")
	if refusal != nil {
		return refusal, nil, nil
	}
	snippet, status := youtube.Child(video, "snippet"), youtube.Child(video, "status")
	newSnippet := youtube.Object{}
	for _, key := range []string{"title", "description", "tags", "categoryId", "defaultLanguage"} {
		if value, present := snippet[key]; present {
			newSnippet[key] = value
		}
	}
	if args.Title != "" {
		newSnippet["title"] = args.Title
	}
	if args.Description != "" {
		newSnippet["description"] = args.Description
	}
	if args.Tags != nil {
		newSnippet["tags"] = args.Tags
	}
	if args.CategoryID != "" {
		newSnippet["categoryId"] = args.CategoryID
	}
	newStatus := youtube.Object{}
	for _, key := range writableStatus {
		if value, present := status[key]; present {
			newStatus[key] = value
		}
	}
	if args.Privacy != "" {
		newStatus["privacyStatus"] = args.Privacy
		delete(newStatus, "publishAt") // a schedule only applies to a private video
	}
	body := youtube.Object{"id": args.VideoID, "snippet": newSnippet, "status": newStatus}
	if _, err := s.YouTube.Call(ctx, c.token, "PUT", "videos", url.Values{"part": {"snippet,status"}}, body); err != nil {
		return failure("youtube_update_video", c.subject, err), nil, nil
	}
	return ok("youtube_update_video", c.subject, "Updated video "+args.VideoID+": "+changes(args)+".", "video", args.VideoID), nil, nil
}

func changes(args UpdateArgs) string {
	var parts []string
	if args.Title != "" {
		parts = append(parts, fmt.Sprintf("title → %q", clip(args.Title, 60)))
	}
	if args.Description != "" {
		parts = append(parts, "description")
	}
	if args.Tags != nil {
		parts = append(parts, fmt.Sprintf("%d tags", len(args.Tags)))
	}
	if args.CategoryID != "" {
		parts = append(parts, "category "+args.CategoryID)
	}
	if args.Privacy != "" {
		parts = append(parts, "privacy → "+args.Privacy)
	}
	return strings.Join(parts, ", ")
}
