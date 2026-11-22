package media

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Signed upload URLs let an agent stage a video with a plain HTTP POST and no
// bearer token. A signed-in person asks for one through an MCP tool call, which
// already carries their token; the server answers with a URL whose last path
// segment is a random secret. The secret is bound to that person, one content
// type and one size, works once, and expires in minutes. Only a hash of it is
// kept, in memory: a restart simply invalidates pending URLs.
const (
	// DefaultTicketTTL is how long a URL stays valid if it is never used.
	DefaultTicketTTL = 10 * time.Minute
	// MaxPendingPerSubject and MaxPending bound the unused tickets held.
	MaxPendingPerSubject = 4
	MaxPending           = 1024
	// UploadPathSegment is the path, under the MCP path, that the signed URL uses.
	UploadPathSegment = "/media/u/"
)

// Errors a caller can test with errors.Is.
var (
	// ErrTicketInvalid covers an unknown, expired or already used secret. They
	// are deliberately indistinguishable.
	ErrTicketInvalid  = errors.New("this upload URL is not valid")
	ErrTooManyPending = errors.New("too many upload URLs are waiting to be used; use one or wait a few minutes")
)

var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Ticket is what a signed URL authorises.
type Ticket struct {
	Subject string
	MIME    string
	Size    int64
	Expires time.Time
}

// Tickets holds the unused signed-URL grants.
type Tickets struct {
	// Now and TTL are for tests; zero values mean time.Now and DefaultTicketTTL.
	Now func() time.Time
	TTL time.Duration

	mu     sync.Mutex
	byHash map[[32]byte]Ticket
}

func (t *Tickets) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tickets) ttl() time.Duration {
	if t.TTL > 0 {
		return t.TTL
	}
	return DefaultTicketTTL
}

// sweep drops expired tickets. Callers hold t.mu.
func (t *Tickets) sweep(now time.Time) {
	for key, ticket := range t.byHash {
		if !now.Before(ticket.Expires) {
			delete(t.byHash, key)
		}
	}
}

// Issue creates a ticket for subject and returns its secret and expiry.
func (t *Tickets) Issue(subject, mime string, size int64) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, errors.New("cannot create an upload URL")
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byHash == nil {
		t.byHash = map[[32]byte]Ticket{}
	}
	t.sweep(now)
	pending := 0
	for _, ticket := range t.byHash {
		if ticket.Subject == subject {
			pending++
		}
	}
	if pending >= MaxPendingPerSubject || len(t.byHash) >= MaxPending {
		return "", time.Time{}, ErrTooManyPending
	}
	ticket := Ticket{Subject: subject, MIME: mime, Size: size, Expires: now.Add(t.ttl())}
	t.byHash[sha256.Sum256([]byte(secret))] = ticket
	return secret, ticket.Expires, nil
}

// Redeem consumes the ticket for secret. check, when given, runs first under the
// same lock: if it fails, its error is returned and the ticket stays usable, so
// a typo in a header does not burn the URL. Unknown, expired and used secrets all
// return ErrTicketInvalid.
func (t *Tickets) Redeem(secret string, check func(Ticket) error) (Ticket, error) {
	key := sha256.Sum256([]byte(secret))
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	ticket, ok := t.byHash[key]
	if !ok {
		return Ticket{}, ErrTicketInvalid
	}
	if check != nil {
		if err := check(ticket); err != nil {
			return Ticket{}, err
		}
	}
	delete(t.byHash, key)
	return ticket, nil
}

var (
	errTicketType   = errors.New("the Content-Type must match the type the upload URL was created for")
	errTicketLength = errors.New("the Content-Length must equal the size the upload URL was created for")
)

// SignedUploadHandler accepts POST <MCP path>/media/u/<secret>: the raw video as
// the body. The secret is the only credential. The same response as Handler
// follows a successful stage.
func (s *Store) SignedUploadHandler(tickets *Tickets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		at := strings.LastIndex(r.URL.Path, UploadPathSegment)
		secret := ""
		if at >= 0 {
			secret = r.URL.Path[at+len(UploadPathSegment):]
		}
		if !secretPattern.MatchString(secret) {
			fail(w, http.StatusNotFound, ErrTicketInvalid.Error())
			return
		}
		mime := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		ticket, err := tickets.Redeem(secret, func(ticket Ticket) error {
			switch {
			case mime != ticket.MIME:
				return errTicketType
			case r.ContentLength != ticket.Size:
				return errTicketLength
			}
			return nil
		})
		switch {
		case errors.Is(err, errTicketType):
			fail(w, http.StatusUnsupportedMediaType, err.Error())
			return
		case errors.Is(err, errTicketLength):
			fail(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			fail(w, http.StatusNotFound, ErrTicketInvalid.Error())
			return
		}
		control := http.NewResponseController(w)
		deadline := time.Now().Add(30 * time.Minute)
		_ = control.SetReadDeadline(deadline)
		_ = control.SetWriteDeadline(deadline)
		id, meta, err := s.Stage(ticket.Subject, ticket.MIME, http.MaxBytesReader(w, r.Body, ticket.Size))
		respondStaged(w, id, meta, err)
	}
}
