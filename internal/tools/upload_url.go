package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tavric-labs/youtube-mcp/internal/media"
)

// CreateUploadURLArgs are the inputs of youtube_create_upload_url.
type CreateUploadURLArgs struct {
	ContentType string `json:"content_type" jsonschema:"video/mp4, video/quicktime or video/webm"`
	SizeBytes   int64  `json:"size_bytes" jsonschema:"exact size of the file in bytes, up to 2 GiB"`
}

var uploadContentTypes = map[string]bool{"video/mp4": true, "video/quicktime": true, "video/webm": true}

// CreateUploadURL issues a single-use URL for staging one video file.
//
// The call carries the caller's mcp-auth token, so the URL needs no token of
// its own: the secret inside it is bound to this person, one content type and
// one size, works once and expires in minutes. It changes nothing on YouTube and
// does not touch the person's Google token.
func (s *Server) CreateUploadURL(_ context.Context, request *mcp.CallToolRequest, args CreateUploadURLArgs) (*mcp.CallToolResult, any, error) {
	subject := subjectOf(request)
	switch {
	case subject == "":
		return result("This call is not authenticated.", true), nil, nil
	case !hasScope(request, WriteScope):
		return result("This needs the "+WriteScope+" scope. Reconnect with write access.", true), nil, nil
	case s.Tickets == nil || s.UploadBase == "":
		return result("Signed upload URLs are not enabled on this server.", true), nil, nil
	}
	contentType := strings.ToLower(strings.TrimSpace(args.ContentType))
	switch {
	case !uploadContentTypes[contentType]:
		return result("content_type must be video/mp4, video/quicktime or video/webm.", true), nil, nil
	case args.SizeBytes <= 0:
		return result("size_bytes must be the file's exact size in bytes, greater than zero.", true), nil, nil
	case args.SizeBytes > media.MaxBytes:
		return result("The file is larger than the 2 GiB limit.", true), nil, nil
	}
	secret, expires, err := s.Tickets.Issue(subject, contentType, args.SizeBytes)
	if err != nil {
		return result(err.Error()+".", true), nil, nil
	}
	uploadURL := strings.TrimRight(s.UploadBase, "/") + media.UploadPathSegment + secret
	// The URL is a credential: it is returned to the caller and never logged.
	slogTool("youtube_create_upload_url", subject, "bytes", args.SizeBytes)
	return result(fmt.Sprintf("Upload the file with one plain HTTP POST; no Authorization header is needed. The URL works once and expires in %d minutes (%s).\n\n"+
		"curl -X POST -H \"Content-Type: %s\" --data-binary @video \"%s\"\n\n"+
		"The Content-Type and the file size (%d bytes) must match what you gave here. The reply carries an asset_id: pass it to youtube_upload_video.",
		int(time.Until(expires).Round(time.Minute).Minutes()), expires.UTC().Format(time.RFC3339), contentType, uploadURL, args.SizeBytes), false), nil, nil
}
