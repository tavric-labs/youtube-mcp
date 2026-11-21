# youtube-mcp

An MCP server that lets a signed-in person manage **their own YouTube channel**:
read it, upload and update videos, organise playlists and moderate comments. It is
built to run on [MCP Runtime](https://github.com/tavric-labs/mcp-runtime) behind
[mcp-auth](https://github.com/tavric-labs/mcp-auth), the same way
[instagram-mcp](https://github.com/tavric-labs/instagram-mcp) does.

> **Status: deployed as `org/youtube:0.1.0` with one ready replica.** The current
> implementation has only been tested against fake Google and YouTube endpoints;
> a real sign-in, token exchange, and YouTube API call still need end-to-end
> verification. See [Known gaps](#known-gaps) before relying on it.

## How it works

1. The MCP client signs the person in through mcp-auth (Sign in with Google) and sends
   the resulting access token to this server.
2. The server verifies the token (`mcpauth.RequireToken`, audience = this server's URL).
3. For each tool call it exchanges that token for **that person's own Google token**
   (RFC 8693, authenticating as a resource client with `private_key_jwt`) and calls
   the YouTube Data API as them.

There is **no shared owner token and no service account**. Every call acts as the signed-in
Google account, so it can only touch what that account can touch. The server stores no
Google token and caches nothing: revoking a person's mcp-auth session stops their YouTube
calls at once. Audit comes from the MCP Runtime gateway; the server logs subject, tool and
outcome, never a token, session URI, comment or caption text.

## Tools

Every tool that changes YouTube is a **dry run unless `confirm: true`**, and needs the
`tools:write` scope when confirmed. A dry run touches nothing, not even the token exchange.

| Tool | Scope | What it does |
|---|---|---|
| `youtube_get_channel` | `tools:read` | The person's own channel: title, handle, counts. |
| `youtube_list_videos` | `tools:read` | Uploads, newest first, via the uploads playlist (1 quota unit, not search's 100). |
| `youtube_get_video` | `tools:read` | One video's details and processing state. |
| `youtube_upload_video` | `tools:write` | Uploads a staged file. Private by default. Resumable and idempotent. |
| `youtube_update_video` | `tools:write` | Title, description, tags, category, privacy. Reads the video, merges, writes back, so fields not named are kept. |
| `youtube_list_playlists` | `tools:read` | The person's playlists. |
| `youtube_create_playlist` | `tools:write` | Creates a playlist (private by default); returns the existing one if the title exists. |
| `youtube_add_playlist_item` | `tools:write` | Adds a video; a no-op if it is already there. |
| `youtube_remove_playlist_item` | `tools:write` | Removes one playlist entry (the video stays). |
| `youtube_list_comments` | `tools:read` | Top-level comment threads. Output is labelled as untrusted viewer text. |
| `youtube_reply_to_comment` | `tools:write` | Replies as the person. Sent once, never auto-retried. |
| `youtube_moderate_comment` | `tools:write` | Publish, hold for review or reject (optionally ban the author) on the person's own videos. |
| `youtube_delete_comment` | `tools:write` | Permanent. |

Comment text is written by strangers. The server marks it as data in the tool output and in
the tool description; a client should still never treat it as an instruction.

### Uploading a video

A video is too large for a tool argument, so it is staged over HTTP first, then uploaded by
asset id:

```bash
# 1. Stage the file for your own account (needs a token with tools:write).
curl -X POST "$SERVER/media" \
  -H "Authorization: Bearer $MCP_ACCESS_TOKEN" \
  -H "Content-Type: video/mp4" \
  --data-binary @video.mp4
# -> {"asset_id":"3f2a...","bytes":...,"sha256":"..."}

# 2. In your MCP client: youtube_upload_video with that asset_id, a title, privacy and
#    made_for_kids. Repeat the identical call until it reports the video id.
```

- `/media` is also served at `<MCP path>/media` (for example `/youtube/mcp/media`), because a
  gateway may route only the prefix it was given for the MCP endpoint. **Which of the two is
  reachable through your gateway has not been verified.**
- Accepted: `video/mp4`, `video/quicktime`, `video/webm`, checked against the file's first
  bytes. At most 2 GiB per file, 8 files and 4 GiB per person. Staged files are deleted after
  24 hours.
- `made_for_kids` has no default: it is a legal declaration, so the caller must say.
- One `youtube_upload_video` call sends for up to about 40 seconds. For a large file it
  returns "in progress: X of Y MiB"; **call it again with the same arguments** and it resumes
  where it stopped. A call that is repeated after a crash, a timeout or a success never
  uploads the file twice: progress is saved per person and asset, bound to a fingerprint of
  the file bytes and the requested details, and a finished result is remembered. Asking for
  *different* details after a finished upload is refused and points to `youtube_update_video`.

## Configuration

| Variable | Purpose |
|---|---|
| `YOUTUBE_MCP_RESOURCE` | This server's public MCP URL (the token audience). |
| `YOUTUBE_MCP_ISSUER` | The mcp-auth issuer URL. `YOUTUBE_JWKS_URL` overrides discovery. |
| `YOUTUBE_MCP_CLIENT_ID`, `YOUTUBE_MCP_CLIENT_KEY_ID` | The resource client registered in mcp-auth. |
| `YOUTUBE_MCP_CLIENT_KEY` or `YOUTUBE_MCP_CLIENT_KEY_FILE` | The client's RSA private key (PEM). |
| `YOUTUBE_MCP_MEDIA_DIR` | Staging directory, absolute. Default `/var/lib/youtube-mcp/media`. |
| `YOUTUBE_API_BASE`, `YOUTUBE_UPLOAD_BASE` | Default to the public YouTube Data API v3 endpoints; tests and proxies override them. |
| `MCP_PATH`, `PORT` | Default `/mcp` and `8080`. |
| `YOUTUBE_MCP_ALLOW_INSECURE=true` | Plain http issuer, local development only. |

## Operator setup

You are the operator of the Google OAuth app. Each end user signs in with their own Google
account and grants *your* app access to *their* channel.

1. **Google Cloud project.** Create one, and enable **YouTube Data API v3**.
2. **OAuth consent screen.** Set the app name, support email and authorized domains. Add the
   scope `https://www.googleapis.com/auth/youtube.force-ssl` (it covers everything this
   server does, including upload; if you only need read tools, `youtube.readonly` is
   narrower).
3. **OAuth client.** Create a client of type **Web application**. Add the authorized redirect
   URI `<MCP Auth issuer URL>/identity/callback`. Keep the client ID and secret.
4. **Secrets.** Generate an RSA key pair for this server's resource client. Put the private
   key in the `youtube-mcp-client-key` Secret (`YOUTUBE_MCP_CLIENT_KEY`) and the public key in
   `.mcp/auth-resource.yaml`. Put `YOUTUBE_CLIENT_SECRET` in `mcp-auth-connector-secrets`.
   Nothing secret goes in a file in this repo.
5. **Deploy and wire sign-in.** `.mcp/servers.yaml` declares outbound
   `www.googleapis.com:443` for the YouTube Data and upload APIs. The platform
   administrator must separately approve this egress after deploy. Runtime records
   the hostname but its Kubernetes policy can enforce only TCP/443 to public IPs;
   it cannot enforce the hostname. Run `server validate`, `server build image`,
   `server push`, and `server deploy` with `.mcp/servers.yaml`; then fill
   `.mcp/auth-resource.yaml` and apply it with
   `mcp-runtime cluster mcp-auth resource apply --file .mcp/auth-resource.yaml` (needs mcp-runtime v0.18.0 or newer; v0.5.2 does not have this command).
6. **Run one replica.** Staged files and saved upload sessions live on the pod's disk. The
   image creates `/var/lib/youtube-mcp/media` for the non-root user; it is **ephemeral** unless
   you mount a volume there (not verified that `servers.yaml` can declare one). After a
   restart staged files and saved sessions are gone and the person stages the file again.

### Consent screen and verification

`youtube.force-ssl` and `youtube.upload` are **sensitive** scopes. While the consent screen is
in **Testing**, only the test users you list (up to 100) can sign in, and Google issues their
refresh tokens for 7 days only. To let anyone sign in you must publish the app and pass
Google's **OAuth verification** (privacy policy, homepage, a demo video of the scope in use).
Until then users see an "unverified app" warning and are capped at 100.

### API quota

The YouTube Data API gives a project **10,000 units a day by default**, resetting at midnight
Pacific Time. Approximate costs (check Google's current quota calculator, they change):

| Call | Units |
|---|---|
| `videos.insert` (an upload) | 1600 |
| `videos.update`, `playlists.insert`, `playlistItems.insert`, `comments.insert`, `comments.setModerationStatus`, deletes | 50 |
| any list or read | 1 |
| `search.list` (not used) | 100 |

So the default quota is about **six uploads a day for the whole project**, shared by every
user. When it runs out tools answer that the quota is used up. More quota needs Google's
quota-extension audit.

### Unverified API projects upload private

Videos uploaded through the API by a project that has **not passed Google's YouTube API
compliance audit** are locked to **private**, whatever `privacy` you ask for. The upload tool
reports this when it happens ("YouTube kept it private"). Until the audit passes, uploads can
be reviewed and then published by hand in YouTube Studio. This is Google policy, not something
the server can work around.

## Google offline access (read this)

**Short answer: durable refresh does not work with mcp-auth as it stands, and this server does
not pretend otherwise.**

What the mcp-auth code shows (inspected under `mcp-auth/auth-server/server/`, at commit
`b0234fb`):

- The upstream authorization URL is built in `oidc.go` from a fixed set of parameters
  (`response_type`, `client_id`, `redirect_uri`, `scope`, `state`, PKCE, and `nonce` when
  needed). The connector configuration (`ConnectorConfig` in `connectors.go`) has **no field
  for extra authorization parameters**, and no non-test code anywhere in the server mentions
  `access_type` or `prompt`.
- The `upstream_session` strategy this server relies on does refresh an expired upstream token
  with `grant_type=refresh_token` (`upstream_session.go`), but only when the provider returned a
  refresh token at sign-in; otherwise it answers *"upstream session expired and has no refresh
  token"*.
- Google returns a refresh token to a web-server OAuth client **only when the authorization
  request carries `access_type=offline`** (and, to get a new one on a repeat consent,
  `prompt=consent`). That is Google's documented behaviour; I have not observed it live.

So: the Google access token mcp-auth holds lasts about **one hour** and cannot be renewed. After
that, tool calls fail with "Sign in again from your MCP client" until the person signs in
again. That is fine for short interactive sessions, **not** for unattended automation, and a
large upload that outlives the token will stop and need a fresh sign-in (the saved upload
session survives, so the retry resumes).

Workarounds I considered and did **not** rely on:

- Putting `access_type=offline&prompt=consent` in `authorization_endpoint`: `oidc.go` appends
  `"?" + params` to that string, so a URL that already has a query would come out malformed
  (by reading the code; not tried).
- A shared owner token or service account: rejected on purpose; it would break per-user
  isolation.

The real fix is in mcp-auth: an optional per-connector map of extra authorization parameters
(for Google, `access_type=offline` and `prompt=consent`), and then a live test that a refresh
token is stored and used. Until that lands and is verified, treat every Google session as
one hour long. Also note that while the Google consent screen is in *Testing*, refresh tokens
expire after 7 days even once this works.

## Security notes

- **Per-person isolation.** Staged files live in a directory named by a hash of the verified
  token subject, mode 0700; every operation takes the subject from the verified token and
  accepts no path or other person's id. Asset ids are validated as 32 hex characters and opened
  through `os.Root`. Tests cover reading, writing state, deleting, resuming and uploading
  across two subjects.
- **Resumable session URIs are secrets.** Together with the person's token a session URI can
  write to YouTube. It is stored only in a 0600 file, never logged, never returned to the
  caller, and is checked to point at the upload host before the token is ever sent to it.
- **No upstream text leaks.** Errors keep only the HTTP status and Google's short reason code.
- **Retries.** Reads, PUT and DELETE are retried with backoff on network errors, 429 and 5xx.
  POST (create playlist, reply, moderate) is sent once so it cannot duplicate; the playlist and
  playlist-item tools check for an existing match first.
- **Limits.** Size, type (by magic bytes), per-person count and bytes, and a 24-hour sweep.

## Known gaps

- The fake server follows Google's documented upload protocol but is not Google. A real
  sign-in, token exchange and API call have not been verified yet; expect surprises on the
  first live upload.
- Offline access (above). Sessions are one hour.
- The staging area is a single pod's disk: one replica, ephemeral without a volume, and a 2 GiB
  file needs that much free node storage.
- Whether the gateway routes `/media` (or `<MCP path>/media`) to the pod is unverified.
- Playlist/title duplicate checks look at the first 200 playlists only.
- `videos.update` is read-modify-write without an ETag precondition, so a concurrent edit in
  YouTube Studio between the read and the write can be overwritten.
- The Dockerfile and CI were written but the image build was **not run** here (no Docker daemon).

## Develop

```bash
gofmt -l . && go vet ./... && go test -race ./... && go build ./...
```
