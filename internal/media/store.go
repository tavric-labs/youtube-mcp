// Package media stages video files before they are uploaded to YouTube.
//
// Each signed-in person (the mcp-auth subject) gets a private directory whose
// name is a hash of the subject, so one person can neither name nor guess
// another's files. Every operation takes the subject from the verified token;
// there is no API that accepts a path or another person's id.
package media

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	mcpauth "github.com/Agent-Hellboy/mcp-auth/auth-client/go/mcpauth"
)

// Limits. MaxBytes is YouTube's practical ceiling for this server; the
// per-person caps keep one account from filling the disk.
const (
	MaxBytes            int64 = 2 << 30
	MaxBytesPerSubject  int64 = 4 << 30
	MaxAssetsPerSubject       = 8
	// DefaultTTL is how long a staged file is kept before Sweep removes it.
	DefaultTTL = 24 * time.Hour
)

// Errors a caller can test with errors.Is.
var (
	ErrNotFound  = errors.New("asset not found for this account")
	ErrTooLarge  = errors.New("media is larger than the 2 GiB limit")
	ErrQuota     = errors.New("too much media is already staged for this account; wait for it to expire or upload it first")
	ErrBadMedia  = errors.New("supported media types are video/mp4, video/quicktime and video/webm, and the file must match its Content-Type")
	ErrEmptyFile = errors.New("the media upload was empty")
)

var assetPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// ValidID reports whether id has the shape of an asset id.
func ValidID(id string) bool { return assetPattern.MatchString(id) }

// Metadata describes one staged file.
type Metadata struct {
	MIME    string    `json:"mime"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
}

// UploadState is the saved progress of uploading one asset to YouTube. Session
// is a resumable-upload URI: with the person's token it can write to YouTube,
// so it lives only in this 0600 file, is never logged, and is never returned.
type UploadState struct {
	// Fingerprint binds the state to the asset bytes and the exact metadata
	// that were requested, so a changed request never reuses another's session.
	Fingerprint string `json:"fingerprint"`
	Session     string `json:"session,omitempty"`
	// Result is the video YouTube created, kept so a retry returns it instead
	// of uploading the file a second time.
	Result map[string]any `json:"result,omitempty"`
}

// Store is a directory of per-subject staging areas.
type Store struct {
	Root string
	// Limit caps one file in bytes; zero means MaxBytes. Tests lower it.
	Limit int64

	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	users int
}

func (s *Store) limit() int64 {
	if s.Limit > 0 {
		return s.Limit
	}
	return MaxBytes
}

func (s *Store) subjectDir(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return filepath.Join(s.Root, hex.EncodeToString(sum[:]))
}

// Lock serialises work on one asset of one person, such as an upload that
// could otherwise run twice at once. Call the returned function to release.
func (s *Store) Lock(subject, id string) func() {
	key := s.subjectDir(subject) + "/" + id
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*keyLock{}
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &keyLock{}
		s.locks[key] = lock
	}
	lock.users++
	s.mu.Unlock()
	lock.Lock()
	return func() {
		lock.Unlock()
		s.mu.Lock()
		if lock.users--; lock.users == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
}

// Stage copies r into the person's private area and returns the new asset id.
// It checks the claimed type against the file's first bytes, enforces the size
// and per-person limits, and publishes the file only once it is complete.
func (s *Store) Stage(subject, mime string, r io.Reader) (string, Metadata, error) {
	if subject == "" {
		return "", Metadata{}, errors.New("a subject is required")
	}
	reader := bufio.NewReader(r)
	head, _ := reader.Peek(12)
	if !matchesMIME(mime, head) {
		return "", Metadata{}, ErrBadMedia
	}
	dir := s.subjectDir(subject)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Metadata{}, errors.New("cannot stage media")
	}
	if err := checkQuota(dir); err != nil {
		return "", Metadata{}, err
	}
	temp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return "", Metadata{}, errors.New("cannot stage media")
	}
	defer os.Remove(temp.Name()) // a no-op once the file has been renamed
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(reader, s.limit()+1))
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return "", Metadata{}, errors.New("the media upload was interrupted")
	case n == 0:
		return "", Metadata{}, ErrEmptyFile
	case n > s.limit():
		return "", Metadata{}, ErrTooLarge
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", Metadata{}, errors.New("cannot stage media")
	}
	name := hex.EncodeToString(id)
	meta := Metadata{MIME: mime, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil)), Created: time.Now().UTC()}
	if err := os.Rename(temp.Name(), filepath.Join(dir, name)); err != nil {
		return "", Metadata{}, errors.New("cannot stage media")
	}
	if err := s.write(subject, name+".json", meta); err != nil {
		_ = os.Remove(filepath.Join(dir, name))
		return "", Metadata{}, errors.New("cannot stage media")
	}
	return name, meta, nil
}

// matchesMIME accepts only the three container types YouTube takes and
// requires the first bytes to look like that container.
func matchesMIME(mime string, head []byte) bool {
	switch mime {
	case "video/mp4":
		return len(head) >= 8 && string(head[4:8]) == "ftyp"
	case "video/quicktime":
		// A QuickTime file starts with an ftyp box or, in older files, a moov, mdat or wide atom.
		if len(head) < 8 {
			return false
		}
		switch string(head[4:8]) {
		case "ftyp", "moov", "mdat", "wide":
			return true
		}
		return false
	case "video/webm":
		return bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3})
	}
	return false
}

// checkQuota counts the person's staged assets and bytes.
func checkQuota(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var count int
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".upload.json") || strings.HasPrefix(name, ".") {
			continue
		}
		var meta Metadata
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil && json.Unmarshal(data, &meta) == nil {
			count++
			total += meta.Size
		}
	}
	if count >= MaxAssetsPerSubject || total >= MaxBytesPerSubject {
		return ErrQuota
	}
	return nil
}

// Open returns the staged file and its metadata. The caller closes the file.
func (s *Store) Open(subject, id string) (*os.File, Metadata, error) {
	if subject == "" || !ValidID(id) {
		return nil, Metadata{}, ErrNotFound
	}
	root, err := os.OpenRoot(s.subjectDir(subject))
	if err != nil {
		return nil, Metadata{}, ErrNotFound
	}
	defer root.Close()
	data, err := root.ReadFile(id + ".json")
	if err != nil {
		return nil, Metadata{}, ErrNotFound
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, Metadata{}, errors.New("the staged asset is damaged")
	}
	file, err := root.Open(id)
	if err != nil {
		return nil, Metadata{}, ErrNotFound
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Size {
		file.Close()
		return nil, Metadata{}, errors.New("the staged asset is damaged")
	}
	return file, meta, nil
}

// State returns the saved upload progress of an asset; a zero value when none.
func (s *Store) State(subject, id string) (UploadState, error) {
	var state UploadState
	if subject == "" || !ValidID(id) {
		return state, ErrNotFound
	}
	root, err := os.OpenRoot(s.subjectDir(subject))
	if err != nil {
		return state, ErrNotFound
	}
	defer root.Close()
	data, err := root.ReadFile(id + ".upload.json")
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

// Save records upload progress for an asset the person owns.
func (s *Store) Save(subject, id string, state UploadState) error {
	if subject == "" || !ValidID(id) {
		return ErrNotFound
	}
	if _, err := os.Stat(filepath.Join(s.subjectDir(subject), id+".json")); err != nil {
		return ErrNotFound
	}
	return s.write(subject, id+".upload.json", state)
}

// Delete removes an asset and its saved state.
func (s *Store) Delete(subject, id string) error {
	if subject == "" || !ValidID(id) {
		return ErrNotFound
	}
	dir := s.subjectDir(subject)
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		return ErrNotFound
	}
	for _, name := range []string{id, id + ".upload.json", id + ".json"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Sweep deletes assets (with their state) older than maxAge and abandoned
// partial uploads older than an hour, across all people. Run it periodically.
func (s *Store) Sweep(maxAge time.Duration) {
	now := time.Now()
	subjects, err := os.ReadDir(s.Root)
	if err != nil {
		return
	}
	for _, subject := range subjects {
		dir := filepath.Join(s.Root, subject.Name())
		entries, err := os.ReadDir(dir)
		if err != nil || !subject.IsDir() {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			info, err := entry.Info()
			if err != nil {
				continue
			}
			switch {
			case strings.HasPrefix(name, ".") && now.Sub(info.ModTime()) > time.Hour:
				_ = os.Remove(filepath.Join(dir, name))
			case ValidID(strings.TrimSuffix(name, ".json")) && strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".upload.json"):
				if now.Sub(info.ModTime()) > maxAge {
					id := strings.TrimSuffix(name, ".json")
					for _, remove := range []string{id, id + ".upload.json", name} {
						_ = os.Remove(filepath.Join(dir, remove))
					}
				}
			}
		}
	}
}

// Fingerprint identifies "this asset uploaded with this request" so saved
// progress is reused only for an identical retry.
func Fingerprint(meta Metadata, request any) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(meta.SHA256 + "\x00" + string(encoded)))
	return hex.EncodeToString(sum[:]), nil
}

// write saves value atomically with mode 0600.
func (s *Store) write(subject, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := s.subjectDir(subject)
	temp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, name))
}

// Handler accepts POST /media: the raw video as the body with its Content-Type.
// Mount it behind the same token verification as the MCP endpoint. It needs the
// tools:write scope, stages the file for the token's own subject, and answers
// with the asset id to pass to youtube_upload_video.
func (s *Store) Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	claims, ok := mcpauth.TokenClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" || !claims.Scopes["tools:write"] {
		fail(w, http.StatusForbidden, "write authentication required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if r.ContentLength > s.limit() {
		fail(w, http.StatusRequestEntityTooLarge, ErrTooLarge.Error())
		return
	}
	// The server's default timeouts suit small requests; a video takes longer.
	control := http.NewResponseController(w)
	deadline := time.Now().Add(30 * time.Minute)
	_ = control.SetReadDeadline(deadline)
	_ = control.SetWriteDeadline(deadline)
	mime := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	id, meta, err := s.Stage(claims.Subject, mime, http.MaxBytesReader(w, r.Body, s.limit()+1))
	switch {
	case errors.Is(err, ErrBadMedia):
		fail(w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, ErrTooLarge):
		fail(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, ErrQuota):
		fail(w, http.StatusInsufficientStorage, err.Error())
	case errors.Is(err, ErrEmptyFile):
		fail(w, http.StatusBadRequest, err.Error())
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"asset_id": id, "bytes": meta.Size, "sha256": meta.SHA256, "mime": meta.MIME, "expires_in_hours": int(DefaultTTL.Hours())})
	}
}

func fail(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
