package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpauth "github.com/Agent-Hellboy/mcp-auth/auth-client/go/mcpauth"
)

func mp4(n int) []byte {
	data := make([]byte, n)
	copy(data, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	return data
}

func newStore(t *testing.T) *Store { return &Store{Root: t.TempDir()} }

func stage(t *testing.T, s *Store, subject string) string {
	t.Helper()
	id, _, err := s.Stage(subject, "video/mp4", bytes.NewReader(mp4(2048)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStageThenOpenReturnsTheSameBytes(t *testing.T) {
	s := newStore(t)
	id, meta, err := s.Stage("alice", "video/mp4", bytes.NewReader(mp4(4096)))
	if err != nil || !ValidID(id) || meta.Size != 4096 || len(meta.SHA256) != 64 {
		t.Fatalf("id=%q meta=%+v err=%v", id, meta, err)
	}
	file, got, err := s.Open("alice", id)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got != meta {
		t.Fatalf("metadata changed: %+v vs %+v", got, meta)
	}
}

func TestOnePersonCannotReachAnothersAsset(t *testing.T) {
	s := newStore(t)
	id := stage(t, s, "alice")
	if err := s.Save("alice", id, UploadState{Fingerprint: "f", Session: "https://upload/session-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open("bob", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob opened alice's asset: %v", err)
	}
	if _, err := s.State("bob", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob read alice's upload state: %v", err)
	}
	if err := s.Save("bob", id, UploadState{Fingerprint: "evil"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob wrote state for alice's asset: %v", err)
	}
	if err := s.Delete("bob", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob deleted alice's asset: %v", err)
	}
	// alice is untouched
	state, err := s.State("alice", id)
	if err != nil || state.Fingerprint != "f" || state.Session == "" {
		t.Fatalf("alice's state = %+v err=%v", state, err)
	}
	if s.subjectDir("alice") == s.subjectDir("bob") {
		t.Fatal("two subjects share a directory")
	}
}

func TestSubjectsThatLookLikePathsCannotEscapeTheirDirectory(t *testing.T) {
	s := newStore(t)
	for _, subject := range []string{"../../etc", "a/b", "..", "alice/../bob"} {
		id := stage(t, s, subject)
		if file, _, err := s.Open(subject, id); err != nil {
			t.Errorf("%q could not reopen its own asset: %v", subject, err)
		} else {
			file.Close()
		}
	}
	entries, _ := os.ReadDir(s.Root)
	for _, entry := range entries {
		if len(entry.Name()) != 64 {
			t.Errorf("unexpected directory name %q", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root, "..", "etc")); err == nil {
		t.Error("a subject escaped the root")
	}
}

func TestBadAssetIDsAreRefusedBeforeTouchingTheDisk(t *testing.T) {
	s := newStore(t)
	stage(t, s, "alice")
	for _, bad := range []string{"", "../x", "..", "a/b", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("a", 32) + ".json", "x\x00y"} {
		if _, _, err := s.Open("alice", bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v", bad, err)
		}
		if _, err := s.State("alice", bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("State(%q) = %v", bad, err)
		}
		if err := s.Save("alice", bad, UploadState{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Save(%q) = %v", bad, err)
		}
		if err := s.Delete("alice", bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v", bad, err)
		}
	}
	if _, _, err := s.Open("", strings.Repeat("a", 32)); !errors.Is(err, ErrNotFound) {
		t.Error("an empty subject was accepted")
	}
}

func TestMediaMustMatchItsTypeAndBeNonEmptyAndWithinLimits(t *testing.T) {
	s := &Store{Root: t.TempDir(), Limit: 1000}
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 100)...)
	for name, tc := range map[string]struct {
		mime string
		data []byte
		want error
	}{
		"text as mp4":      {"video/mp4", []byte("<html>not a video at all</html>"), ErrBadMedia},
		"mp4 as webm":      {"video/webm", mp4(100), ErrBadMedia},
		"unsupported type": {"application/zip", mp4(100), ErrBadMedia},
		"empty type":       {"", mp4(100), ErrBadMedia},
		"too short":        {"video/mp4", []byte("ftyp"), ErrBadMedia},
		"too large":        {"video/mp4", mp4(1001), ErrTooLarge},
		"webm ok":          {"video/webm", webm, nil},
		"exact limit ok":   {"video/mp4", mp4(1000), nil},
	} {
		_, _, err := s.Stage("alice", tc.mime, bytes.NewReader(tc.data))
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if tc.want != nil {
			entries, _ := os.ReadDir(s.subjectDir("alice"))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".part-") {
					t.Errorf("%s: a partial file was left behind", name)
				}
			}
		}
	}
}

func TestOnePersonCannotFillTheDisk(t *testing.T) {
	s := newStore(t)
	for i := 0; i < MaxAssetsPerSubject; i++ {
		stage(t, s, "alice")
	}
	if _, _, err := s.Stage("alice", "video/mp4", bytes.NewReader(mp4(100))); !errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
	if _, _, err := s.Stage("bob", "video/mp4", bytes.NewReader(mp4(100))); err != nil {
		t.Fatalf("alice's quota blocked bob: %v", err)
	}
}

func TestStagedFilesAndSavedSessionsAreOwnerOnly(t *testing.T) {
	s := newStore(t)
	id := stage(t, s, "alice")
	if err := s.Save("alice", id, UploadState{Fingerprint: "f", Session: "https://upload.example/secret-session"}); err != nil {
		t.Fatal(err)
	}
	dir := s.subjectDir("alice")
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want asset, metadata and upload state only (no stray temp files)", len(entries))
	}
	for _, entry := range entries {
		if info, _ := entry.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", entry.Name(), info.Mode().Perm())
		}
	}
}

func TestSavedStateRoundTripsAndStartsEmpty(t *testing.T) {
	s := newStore(t)
	id := stage(t, s, "alice")
	if state, err := s.State("alice", id); err != nil || state.Session != "" || state.Result != nil {
		t.Fatalf("fresh state = %+v err=%v", state, err)
	}
	want := UploadState{Fingerprint: "f", Result: map[string]any{"id": "vid1"}}
	if err := s.Save("alice", id, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.State("alice", id)
	if err != nil || got.Fingerprint != "f" || got.Result["id"] != "vid1" {
		t.Fatalf("state = %+v err=%v", got, err)
	}
}

func TestFingerprintBindsTheFileAndTheRequest(t *testing.T) {
	a := Metadata{SHA256: "aaa"}
	base, _ := Fingerprint(a, map[string]string{"title": "x"})
	same, _ := Fingerprint(a, map[string]string{"title": "x"})
	otherRequest, _ := Fingerprint(a, map[string]string{"title": "y"})
	otherFile, _ := Fingerprint(Metadata{SHA256: "bbb"}, map[string]string{"title": "x"})
	if base != same || base == otherRequest || base == otherFile {
		t.Fatalf("fingerprints: %s %s %s %s", base, same, otherRequest, otherFile)
	}
}

func TestDamagedAssetsAreRefused(t *testing.T) {
	s := newStore(t)
	id := stage(t, s, "alice")
	if err := os.WriteFile(filepath.Join(s.subjectDir("alice"), id), []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open("alice", id); err == nil {
		t.Fatal("an asset whose size no longer matches was opened")
	}
}

func TestSweepRemovesOldAssetsWithTheirStateAndKeepsFreshOnes(t *testing.T) {
	s := newStore(t)
	old, fresh := stage(t, s, "alice"), stage(t, s, "alice")
	if err := s.Save("alice", old, UploadState{Fingerprint: "f"}); err != nil {
		t.Fatal(err)
	}
	dir := s.subjectDir("alice")
	past := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{old, old + ".json", old + ".upload.json"} {
		_ = os.Chtimes(filepath.Join(dir, name), past, past)
	}
	stray := filepath.Join(dir, ".part-123")
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	_ = os.Chtimes(stray, past, past)

	s.Sweep(24 * time.Hour)
	if _, _, err := s.Open("alice", old); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old asset survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, old+".upload.json")); err == nil {
		t.Error("the old upload state survived")
	}
	if _, err := os.Stat(stray); err == nil {
		t.Error("an abandoned partial upload survived")
	}
	if file, _, err := s.Open("alice", fresh); err != nil {
		t.Errorf("the fresh asset was swept: %v", err)
	} else {
		file.Close()
	}
}

func TestLockSerialisesWorkOnOneAssetOnly(t *testing.T) {
	s := newStore(t)
	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := s.Lock("alice", "asset")
			if inside.Add(1) > 1 {
				overlaps.Add(1)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d overlaps", overlaps.Load())
	}
	releaseA := s.Lock("alice", "a")
	done := make(chan struct{})
	go func() { s.Lock("bob", "a")(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("another person's lock waited on alice's")
	}
	releaseA()
	if len(s.locks) != 0 {
		t.Fatalf("%d lock entries leaked", len(s.locks))
	}
}

func post(t *testing.T, s *Store, claims *mcpauth.TokenClaims, method, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/media", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	if claims != nil {
		request = request.WithContext(mcpauth.WithTokenClaims(context.Background(), *claims))
	}
	recorder := httptest.NewRecorder()
	s.Handler(recorder, request)
	return recorder
}

func TestHandlerNeedsAVerifiedSubjectWithTheWriteScope(t *testing.T) {
	s := newStore(t)
	for name, claims := range map[string]*mcpauth.TokenClaims{
		"no claims":  nil,
		"read only":  {Subject: "alice", Scopes: map[string]bool{"tools:read": true}},
		"no subject": {Scopes: map[string]bool{"tools:write": true}},
	} {
		if rec := post(t, s, claims, http.MethodPost, "video/mp4", mp4(100)); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	if entries, _ := os.ReadDir(s.Root); len(entries) != 0 {
		t.Fatal("an unauthorised request staged data")
	}
}

func TestHandlerStagesForTheTokensOwnSubject(t *testing.T) {
	s := newStore(t)
	alice := &mcpauth.TokenClaims{Subject: "alice", Scopes: map[string]bool{"tools:write": true}}
	rec := post(t, s, alice, http.MethodPost, "video/mp4; charset=binary", mp4(512))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		AssetID string `json:"asset_id"`
		Bytes   int64  `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Bytes != 512 || !ValidID(body.AssetID) {
		t.Fatalf("body = %s err=%v", rec.Body, err)
	}
	if file, _, err := s.Open("alice", body.AssetID); err != nil {
		t.Fatalf("alice cannot open what she staged: %v", err)
	} else {
		file.Close()
	}
	if _, _, err := s.Open("bob", body.AssetID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob can open alice's upload: %v", err)
	}
	if strings.Contains(rec.Body.String(), s.Root) {
		t.Fatal("the response exposes the staging path")
	}
}

func TestHandlerRejectsWrongMethodTypeAndSize(t *testing.T) {
	s := &Store{Root: t.TempDir(), Limit: 100}
	claims := &mcpauth.TokenClaims{Subject: "alice", Scopes: map[string]bool{"tools:write": true}}
	for name, tc := range map[string]struct {
		method, mime string
		body         []byte
		want         int
	}{
		"get":       {http.MethodGet, "video/mp4", mp4(50), http.StatusMethodNotAllowed},
		"bad type":  {http.MethodPost, "text/html", mp4(50), http.StatusUnsupportedMediaType},
		"not video": {http.MethodPost, "video/mp4", []byte("<script>alert(1)</script>"), http.StatusUnsupportedMediaType},
		"too large": {http.MethodPost, "video/mp4", mp4(500), http.StatusRequestEntityTooLarge},
		"empty":     {http.MethodPost, "video/mp4", nil, http.StatusUnsupportedMediaType},
	} {
		if rec := post(t, s, claims, tc.method, tc.mime, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, tc.want, rec.Body)
		}
	}
}
