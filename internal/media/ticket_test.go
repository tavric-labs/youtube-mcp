package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTickets(now *time.Time) *Tickets {
	return &Tickets{Now: func() time.Time { return *now }}
}

func TestTicketIsSingleUseAndBoundToItsGrant(t *testing.T) {
	now := time.Now()
	tickets := newTickets(&now)
	secret, expires, err := tickets.Issue("user-1", "video/mp4", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) < 43 || strings.ContainsAny(secret, "/+= ") {
		t.Fatalf("secret %q is not a long URL-safe random value", secret)
	}
	if !expires.Equal(now.Add(DefaultTicketTTL)) {
		t.Fatalf("expires = %s, want now+%s", expires, DefaultTicketTTL)
	}
	ticket, err := tickets.Redeem(secret, nil)
	if err != nil || ticket.Subject != "user-1" || ticket.MIME != "video/mp4" || ticket.Size != 1234 {
		t.Fatalf("Redeem() = %+v, %v", ticket, err)
	}
	if _, err := tickets.Redeem(secret, nil); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("second Redeem() error = %v, want ErrTicketInvalid", err)
	}
}

func TestTicketExpiresAndUnknownSecretsAreIndistinguishable(t *testing.T) {
	now := time.Now()
	tickets := newTickets(&now)
	secret, _, _ := tickets.Issue("user-1", "video/mp4", 10)
	now = now.Add(DefaultTicketTTL + time.Second)
	_, expired := tickets.Redeem(secret, nil)
	_, unknown := tickets.Redeem("not-a-real-secret-not-a-real-secret-not-a-real", nil)
	if !errors.Is(expired, ErrTicketInvalid) || !errors.Is(unknown, ErrTicketInvalid) || expired.Error() != unknown.Error() {
		t.Fatalf("expired = %v, unknown = %v; both must be the same ErrTicketInvalid", expired, unknown)
	}
}

func TestTicketCheckFailureKeepsTheTicket(t *testing.T) {
	now := time.Now()
	tickets := newTickets(&now)
	secret, _, _ := tickets.Issue("user-1", "video/mp4", 10)
	boom := errors.New("wrong content type")
	if _, err := tickets.Redeem(secret, func(Ticket) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Redeem() error = %v, want the check's error", err)
	}
	if _, err := tickets.Redeem(secret, nil); err != nil {
		t.Fatalf("a failed precondition must not burn the ticket: %v", err)
	}
}

func TestTicketsCapPendingPerSubject(t *testing.T) {
	now := time.Now()
	tickets := newTickets(&now)
	for i := 0; i < MaxPendingPerSubject; i++ {
		if _, _, err := tickets.Issue("user-1", "video/mp4", 10); err != nil {
			t.Fatalf("Issue #%d: %v", i, err)
		}
	}
	if _, _, err := tickets.Issue("user-1", "video/mp4", 10); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("Issue() error = %v, want ErrTooManyPending", err)
	}
	if _, _, err := tickets.Issue("user-2", "video/mp4", 10); err != nil {
		t.Fatalf("another person must not be affected: %v", err)
	}
	now = now.Add(DefaultTicketTTL + time.Second)
	if _, _, err := tickets.Issue("user-1", "video/mp4", 10); err != nil {
		t.Fatalf("expired tickets must free the quota: %v", err)
	}
}

func uploadRequest(secret, mime string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp/media/u/"+secret, bytes.NewReader(body))
	r.Header.Set("Content-Type", mime)
	return r
}

func mp4Body(n int) []byte {
	data := make([]byte, n)
	copy(data, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	return data
}

func TestSignedUploadStagesForTheTicketOwnerWithoutABearer(t *testing.T) {
	now := time.Now()
	tickets, store := newTickets(&now), &Store{Root: t.TempDir()}
	secret, _, _ := tickets.Issue("user-1", "video/mp4", 2048)
	handler := store.SignedUploadHandler(tickets)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, uploadRequest(secret, "video/mp4", mp4Body(2048)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var reply struct {
		AssetID string `json:"asset_id"`
		Bytes   int64  `json:"bytes"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil || !ValidID(reply.AssetID) || reply.Bytes != 2048 {
		t.Fatalf("reply = %s (%v)", recorder.Body.String(), err)
	}
	file, _, err := store.Open("user-1", reply.AssetID)
	if err != nil {
		t.Fatalf("the asset must belong to the ticket's subject: %v", err)
	}
	file.Close()
	if _, _, err := store.Open("user-2", reply.AssetID); err == nil {
		t.Fatal("another subject can open the asset")
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, uploadRequest(secret, "video/mp4", mp4Body(2048)))
	if replay.Code != http.StatusNotFound {
		t.Fatalf("replay status = %d, want 404 (single use)", replay.Code)
	}
}

func TestSignedUploadRejectsMismatchesWithoutBurningTheTicket(t *testing.T) {
	now := time.Now()
	tickets, store := newTickets(&now), &Store{Root: t.TempDir()}
	secret, _, _ := tickets.Issue("user-1", "video/mp4", 2048)
	handler := store.SignedUploadHandler(tickets)

	for _, tc := range []struct {
		name   string
		build  func() *http.Request
		status int
	}{
		{"wrong content type", func() *http.Request { return uploadRequest(secret, "video/webm", mp4Body(2048)) }, http.StatusUnsupportedMediaType},
		{"wrong length", func() *http.Request { return uploadRequest(secret, "video/mp4", mp4Body(2049)) }, http.StatusBadRequest},
		{"wrong method", func() *http.Request {
			r := uploadRequest(secret, "video/mp4", mp4Body(2048))
			r.Method = http.MethodPut
			return r
		}, http.StatusMethodNotAllowed},
		{"unknown secret", func() *http.Request { return uploadRequest(strings.Repeat("a", 43), "video/mp4", mp4Body(2048)) }, http.StatusNotFound},
		{"malformed secret", func() *http.Request { return uploadRequest("../../etc/passwd", "video/mp4", mp4Body(2048)) }, http.StatusNotFound},
		{"no secret", func() *http.Request { return uploadRequest("", "video/mp4", mp4Body(2048)) }, http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, tc.build())
		if recorder.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, recorder.Code, tc.status)
		}
	}
	good := httptest.NewRecorder()
	handler.ServeHTTP(good, uploadRequest(secret, "video/mp4", mp4Body(2048)))
	if good.Code != http.StatusCreated {
		t.Fatalf("after the rejected attempts the ticket must still work, got %d", good.Code)
	}
}

func TestSignedUploadRejectsFileThatIsNotTheDeclaredType(t *testing.T) {
	now := time.Now()
	tickets, store := newTickets(&now), &Store{Root: t.TempDir()}
	secret, _, _ := tickets.Issue("user-1", "video/mp4", 64)
	recorder := httptest.NewRecorder()
	store.SignedUploadHandler(tickets).ServeHTTP(recorder, uploadRequest(secret, "video/mp4", bytes.Repeat([]byte("x"), 64)))
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 for bytes that are not an mp4", recorder.Code)
	}
}
