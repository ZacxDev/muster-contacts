package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

// Internal is the internal listener's handler (CONTACTS_INTERNAL_LISTEN):
// GET /healthz, and the contacts API for the meetings plugin
// (contactsapi). It is never routed publicly; the NetworkPolicy admits only
// the meetings plugin's pods to it.
//
// 🔴 Its access log is "api <method> <route pattern> <status> <duration>":
// never a path (a subject), never a body.
func (s *Server) Internal() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil || !s.app.Live() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+contactsapi.RouteOwner+"{owner}", s.apiAuth(s.handleOwnerStanding))
	mux.HandleFunc("POST "+contactsapi.RouteSubmissions, s.apiAuth(s.handleSubmission))
	mux.HandleFunc("/v1/", s.apiAuth(func(w http.ResponseWriter, r *http.Request) {
		apiErr(w, http.StatusNotFound, contactsapi.ErrNotFoundRoute)
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiErr(w, http.StatusNotFound, contactsapi.ErrNotFoundRoute)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.cfg.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(sw, r)
		if r.Pattern != "GET /healthz" {
			s.cfg.Log.Printf("api %s %s %d %s", r.Method, routeOf(r), sw.status, s.cfg.Now().Sub(start).Round(time.Millisecond))
		}
	})
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, contactsapi.ErrorAnswer{Error: msg})
}

// apiAuth admits a request carrying `Authorization: Bearer
// <CONTACTS_API_TOKEN>`, compared in constant time. Anything else is the
// one 401.
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + s.cfg.APIToken)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.cfg.APIToken == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			apiErr(w, http.StatusUnauthorized, contactsapi.ErrUnauthorised)
			return
		}
		next(w, r)
	}
}

// readBody reads one JSON object of at most contactsapi.MaxBody into v,
// refusing an unknown field, and answers the body's hash (of its compact
// form, so a retry's whitespace does not change it).
func readBody(w http.ResponseWriter, r *http.Request, v any) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, contactsapi.MaxBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		apiErr(w, http.StatusRequestEntityTooLarge, contactsapi.ErrBodyTooLarge)
		return "", false
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(v); err == nil && dec.More() {
			err = errors.New("trailing data")
		}
	}
	var compact bytes.Buffer
	if err == nil {
		err = json.Compact(&compact, raw)
	}
	if err != nil {
		apiErr(w, http.StatusBadRequest, contactsapi.ErrBadBody)
		return "", false
	}
	sum := sha256.Sum256(compact.Bytes())
	return hex.EncodeToString(sum[:]), true
}

func fieldErr(w http.ResponseWriter, err error) bool {
	var fe contactsapi.FieldError
	if errors.As(err, &fe) {
		writeJSON(w, http.StatusBadRequest, contactsapi.ErrorAnswer{Error: contactsapi.ErrInvalidField, Field: fe.Field})
		return true
	}
	return false
}

func (s *Server) internalErr(w http.ResponseWriter, what string) {
	s.cfg.Log.Printf("api: %s failed", what)
	apiErr(w, http.StatusInternalServerError, contactsapi.ErrServerInternal)
}

// handleOwnerStanding is GET /v1/owners/{owner}: may meetings offer the
// card for this owner?
func (s *Server) handleOwnerStanding(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !contactsapi.ValidSubject(owner) {
		apiErr(w, http.StatusBadRequest, contactsapi.ErrBadOwner)
		return
	}
	st, err := s.store.StandingOf(r.Context(), owner)
	if err != nil {
		s.internalErr(w, "standing")
		return
	}
	if !st.Accepting || !s.app.Live() {
		apiErr(w, http.StatusNotFound, contactsapi.ErrNotAccepting)
		return
	}
	writeJSON(w, http.StatusOK, contactsapi.OwnerAnswer{Accepting: true})
}

// errTooMany is the card limit's refusal, with when to try again.
type errTooMany struct{ retry time.Duration }

func (e errTooMany) Error() string { return "too many cards" }

// handleSubmission is POST /v1/submissions: a guest's card.
//
// 🔴 It answers 202 with no body the same whether the card made a contact,
// merged into one, or was dropped because the owner is not accepting (merge
// rule 5): what meetings is told never says whether an email was already a
// contact.
func (s *Server) handleSubmission(w http.ResponseWriter, r *http.Request) {
	var in contactsapi.Submission
	hash, ok := readBody(w, r, &in)
	if !ok {
		return
	}
	name, addr, key, title, err := in.Check()
	if fieldErr(w, err) {
		return
	}
	if r.Header.Get(contactsapi.IdempotencyHeader) != contactsapi.KeyForCard(in.CardID) {
		apiErr(w, http.StatusBadRequest, contactsapi.ErrKey)
		return
	}
	o, err := s.store.ForOwner(in.Owner)
	if err != nil {
		apiErr(w, http.StatusBadRequest, contactsapi.ErrBadOwner)
		return
	}
	if !s.app.Live() {
		// The app is uninstalled: nothing is kept, and the answer is a
		// dropped card's.
		accepted(w)
		return
	}
	admit := func() error {
		if retry, ok := s.limits.admit(in.Owner); !ok {
			return errTooMany{retry}
		}
		return nil
	}
	_, err = o.Submit(r.Context(), store.Card{Key: contactsapi.KeyForCard(in.CardID), RequestHash: hash, Name: name, Address: addr, AddressKey: key,
		Title: title, Meeting: in.Meeting, Participant: in.Participant}, admit, s.cfg.Now())
	var tm errTooMany
	switch {
	case errors.As(err, &tm):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(tm.retry.Seconds()))))
		apiErr(w, http.StatusTooManyRequests, contactsapi.ErrTooManyCards)
		return
	case errors.Is(err, store.ErrKeyReused):
		apiErr(w, http.StatusConflict, contactsapi.ErrKeyReused)
		return
	case err != nil:
		s.internalErr(w, "submission")
		return
	}
	accepted(w)
}

// accepted is a card's one answer: 202, no body.
func accepted(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
}

// cardLimiter caps card submissions in a sliding window: perOwner for one
// owner, perInstall for all of them (design: 200 and 2,000 a day). It is in
// memory (design: no guest address or user agent is ever stored), so a
// restart forgets it.
type cardLimiter struct {
	mu         sync.Mutex
	perOwner   int
	perInstall int
	window     time.Duration
	now        func() time.Time
	owners     map[string][]time.Time
	all        []time.Time
}

func newCardLimiter(perOwner, perInstall int, window time.Duration, now func() time.Time) *cardLimiter {
	return &cardLimiter{perOwner: perOwner, perInstall: perInstall, window: window, now: now, owners: map[string][]time.Time{}}
}

func trimWindow(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	return ts[i:]
}

// admit counts one card for owner, or answers how long until one is
// allowed.
func (l *cardLimiter) admit(owner string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.all = trimWindow(l.all, now, l.window)
	mine := trimWindow(l.owners[owner], now, l.window)
	if len(mine) == 0 {
		delete(l.owners, owner)
	}
	var retry time.Duration
	if len(mine) >= l.perOwner {
		retry = l.window - now.Sub(mine[len(mine)-l.perOwner])
	}
	if len(l.all) >= l.perInstall {
		retry = max(retry, l.window-now.Sub(l.all[len(l.all)-l.perInstall]))
	}
	if retry > 0 {
		return max(retry, time.Second), false
	}
	l.owners[owner] = append(mine, now)
	l.all = append(l.all, now)
	return 0, true
}

// fill spends n cards for owner (tests: the contract's card_limit_reached).
func (l *cardLimiter) fill(owner string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for range n {
		l.owners[owner] = append(l.owners[owner], now)
	}
}

// SpendCards spends n of owner's cards now (tests only: the contract's
// card_limit_reached precondition).
func (s *Server) SpendCards(owner string, n int) { s.limits.fill(owner, n) }
