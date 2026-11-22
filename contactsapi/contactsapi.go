// Package contactsapi is the contract between muster-contacts and the
// meetings plugin: the internal API's routes, its bodies and answers, the
// field rules both sides check a card with, and (contract.go) the table of
// cases both repositories run against the real contacts server.
//
// It is the one package of muster-contacts another module may import: the
// meetings plugin's tests import it at a pinned version, so a change on
// either side that breaks the other is red in both.
//
// The API (README, "The contacts API"):
//
//	GET  /v1/owners/{owner}   200 {"accepting": true} or 404 {"error": "not accepting"}
//	POST /v1/submissions      a guest's card: 202 {"receipt": "<43 chars>"}, whatever happened
//	POST /v1/receipts/check   200 {"known": true, "contact_ref": "…"} or 200 {"known": false}
//	POST /v1/attendance       "met at meeting X" for a guest already known: 204
//
// Every route takes `Authorization: Bearer <CONTACTS_API_TOKEN>`; every
// write takes an Idempotency-Key (KeyFor*). Bodies are JSON, at most
// MaxBody bytes, and an unknown field is refused.
package contactsapi

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
)

// The routes.
const (
	RouteOwner         = "/v1/owners/" // + the owner's subject
	RouteSubmissions   = "/v1/submissions"
	RouteReceiptsCheck = "/v1/receipts/check"
	RouteAttendance    = "/v1/attendance"
)

// IdempotencyHeader carries a write's key.
const IdempotencyHeader = "Idempotency-Key"

// MaxBody is the largest body a route reads; a longer one is 413.
const MaxBody = 4 << 10

// The error texts the API answers with, each in {"error": …}.
const (
	ErrUnauthorised   = "unauthorised"
	ErrNotAccepting   = "not accepting"
	ErrNoSuchContact  = "no such contact"
	ErrInvalidField   = "invalid field" // with "field": the field's JSON path
	ErrBadBody        = "the body is not this route's JSON object"
	ErrBodyTooLarge   = "the body is too large"
	ErrKey            = "Idempotency-Key is missing or is not this request's key"
	ErrKeyReused      = "this Idempotency-Key was used with a different body"
	ErrTooManyCards   = "too many cards: try again later"
	ErrBadOwner       = "owner is not a subject"
	ErrNotFoundRoute  = "not found"
	ErrServerInternal = "the request could not be completed"
)

// Meeting is the meeting a card or an attendance names. ID is meetings' own
// id (1 to 64 base64url characters, so it can ride in muster's ?ref=);
// Title is what the owner named it (or meetings' default date label), at
// most MaxTitleRunes, and the only meeting field that is free text.
type Meeting struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

// Participant is the guest's row in meetings: its id, its LiveKit identity
// (for the transcript's lines), what they typed at the knock, and when they
// were in the call.
type Participant struct {
	ID        string     `json:"id"`
	Identity  string     `json:"identity"`
	TypedName string     `json:"typed_name"`
	JoinedAt  *time.Time `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at"`
}

// Submission is POST /v1/submissions: a guest's card. Its key is
// KeyForCard(CardID).
type Submission struct {
	Owner       string      `json:"owner"`
	CardID      string      `json:"card_id"`
	Meeting     Meeting     `json:"meeting"`
	Participant Participant `json:"participant"`
	Name        string      `json:"name"`
	Email       string      `json:"email"`
}

// SubmissionAnswer is its 202 answer, the same whether the card made a
// contact, merged into one, or was dropped (the owner is not accepting).
type SubmissionAnswer struct {
	Receipt string `json:"receipt"`
}

// ReceiptCheck is POST /v1/receipts/check.
type ReceiptCheck struct {
	Owner   string `json:"owner"`
	Receipt string `json:"receipt"`
}

// ReceiptAnswer is its 200 answer. ContactRef is set only when Known.
type ReceiptAnswer struct {
	Known      bool   `json:"known"`
	ContactRef string `json:"contact_ref,omitempty"`
}

// Attendance is POST /v1/attendance: a contact the owner already has was in
// a meeting. Via is how meetings knows it is them: "receipt" (their browser's
// receipt checked known) or "invite" (phase 2: their own invite link). Its
// key is KeyForAttendance(Meeting.ID, Participant.ID).
type Attendance struct {
	Owner       string      `json:"owner"`
	ContactRef  string      `json:"contact_ref"`
	Via         string      `json:"via"`
	Meeting     Meeting     `json:"meeting"`
	Participant Participant `json:"participant"`
}

// OwnerAnswer is GET /v1/owners/{owner}'s 200 answer.
type OwnerAnswer struct {
	Accepting bool `json:"accepting"`
}

// ErrorAnswer is every refusal's body.
type ErrorAnswer struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

// KeyForCard is a submission's Idempotency-Key.
func KeyForCard(cardID string) string { return "card:" + cardID }

// KeyForAttendance is an attendance's Idempotency-Key.
func KeyForAttendance(meetingID, participantID string) string {
	return "attend:" + meetingID + ":" + participantID
}

var (
	subjectRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	tokenRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	refRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	labelRE   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	digitsRE  = regexp.MustCompile(`^[0-9]+$`)
)

// ValidSubject is the shape of a muster subject: 22 base64url characters.
func ValidSubject(s string) bool { return subjectRE.MatchString(s) }

// ValidContactRef is a contact id's shape: 22 base64url characters.
func ValidContactRef(s string) bool { return subjectRE.MatchString(s) }

// ValidCardID and ValidReceipt are 32 random bytes, 43 base64url characters.
func ValidCardID(s string) bool  { return tokenRE.MatchString(s) }
func ValidReceipt(s string) bool { return tokenRE.MatchString(s) }

// ValidRef is meetings' meeting, participant and identity shape: 1 to 64
// base64url characters (muster's ?ref= pattern).
func ValidRef(s string) bool { return refRE.MatchString(s) }

// FieldError names the field a body got wrong. Its text names the field
// only, never what was in it.
type FieldError struct{ Field string }

func (e FieldError) Error() string { return "invalid field " + e.Field }

// MaxNameRunes is the longest card name (meetings' knock rule).
const MaxNameRunes = 40

// MaxTitleRunes is the longest meeting title.
const MaxTitleRunes = 80

// MaxEmailBytes is the longest address; MaxLocalBytes its local part's.
const (
	MaxEmailBytes = 254
	MaxLocalBytes = 64
)

// ErrName and ErrEmail are the two card fields' refusals.
var (
	ErrName  = errors.New("type a name of 1 to 40 characters that shows something, without control or text-direction characters")
	ErrEmail = errors.New("type one email address, such as name@example.com")
)

// CleanName is the card's name rule, the same as meetings' knock
// (lobby.CleanName): valid UTF-8, trimmed, 1 to MaxNameRunes characters, no
// control characters, no format characters but the zero-width joiner emoji
// use (so no text-direction controls), no line or paragraph separators, and
// at least one character that draws something. It returns the name trimmed.
func CleanName(s string) (string, error) { return cleanText(s, 1, MaxNameRunes, ErrName) }

// CleanTitle is a meeting title's rule: CleanText's, up to MaxTitleRunes.
func CleanTitle(s string) (string, error) { return CleanText(s, MaxTitleRunes) }

// CleanText is CleanName's rule for a text that may be empty: trimmed, at
// most maxRunes, no control, format (but the zero-width joiner), line or
// paragraph separator characters, and, when not empty, something visible.
func CleanText(s string, maxRunes int) (string, error) {
	return cleanText(s, 0, maxRunes, errors.New("text"))
}

func cleanText(s string, minRunes, maxRunes int, bad error) (string, error) {
	if !utf8.ValidString(s) {
		return "", bad
	}
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < minRunes || n > maxRunes {
		return "", bad
	}
	seen := false
	for _, r := range s {
		seen = seen || visible(r)
		switch {
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return "", bad
		case unicode.Is(unicode.Cf, r) && r != '‍':
			return "", bad
		}
	}
	if !seen && n > 0 {
		return "", bad
	}
	return s, nil
}

// visible says r draws something by itself (meetings' lobby.visible).
func visible(r rune) bool {
	switch {
	case !unicode.IsGraphic(r), unicode.IsSpace(r), r == '⠀':
		return false
	case unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me, unicode.Cf, unicode.Zs):
		return false
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r), unicode.Is(unicode.Variation_Selector, r):
		return false
	}
	return true
}

// localForbidden are characters an address's local part may not hold: the
// display-name form ("Ada <a@x>"), a second address (",", ";"), quoting and
// comments, none of which a card needs.
const localForbidden = `<>()[]\,;:"@`

// CheckEmail is the email rule: required; trimmed; NFC; at most
// MaxEmailBytes; exactly one "@"; a local part of 1 to MaxLocalBytes with no
// space, control or format character and none of localForbidden; a domain
// of DNS labels with at least one dot, converted by IDNA (the lookup
// profile), whose last label is not all digits. It returns the address as
// given (trimmed, NFC: what is shown and sent to) and its key: the local
// part lower-cased and the domain in lower-case ASCII. The key is the only
// thing two contacts are matched on; plus-addressing and dots are not
// folded ("ada+work@" and "ada@" are two addresses).
func CheckEmail(raw string) (address, key string, err error) {
	if !utf8.ValidString(raw) {
		return "", "", ErrEmail
	}
	s := norm.NFC.String(strings.TrimSpace(raw))
	if s == "" || len(s) > MaxEmailBytes || strings.Count(s, "@") != 1 {
		return "", "", ErrEmail
	}
	local, domain, _ := strings.Cut(s, "@")
	if len(local) < 1 || len(local) > MaxLocalBytes {
		return "", "", ErrEmail
	}
	for _, r := range local {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(localForbidden, r) {
			return "", "", ErrEmail
		}
	}
	for _, r := range domain {
		if unicode.IsSpace(r) || strings.ContainsRune(localForbidden, r) {
			return "", "", ErrEmail
		}
	}
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", "", ErrEmail
	}
	ascii = strings.ToLower(ascii)
	labels := strings.Split(ascii, ".")
	if len(ascii) > 253 || len(labels) < 2 || digitsRE.MatchString(labels[len(labels)-1]) {
		return "", "", ErrEmail
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return "", "", ErrEmail
		}
	}
	return s, strings.ToLower(local) + "@" + ascii, nil
}

// EmailKey is CheckEmail's key alone.
func EmailKey(raw string) (string, error) {
	_, k, err := CheckEmail(raw)
	return k, err
}

// Check validates a submission's fields, returning the cleaned name,
// address, key and title. A wrong field is a FieldError naming it.
func (s Submission) Check() (name, address, key, title string, err error) {
	if !ValidSubject(s.Owner) {
		return "", "", "", "", FieldError{"owner"}
	}
	if !ValidCardID(s.CardID) {
		return "", "", "", "", FieldError{"card_id"}
	}
	if title, err = checkMeeting(s.Meeting); err != nil {
		return "", "", "", "", err
	}
	if err = checkParticipant(s.Participant); err != nil {
		return "", "", "", "", err
	}
	if name, err = CleanName(s.Name); err != nil {
		return "", "", "", "", FieldError{"name"}
	}
	if address, key, err = CheckEmail(s.Email); err != nil {
		return "", "", "", "", FieldError{"email"}
	}
	return name, address, key, title, nil
}

// Check validates an attendance's fields, returning the meeting's title.
func (a Attendance) Check() (title string, err error) {
	if !ValidSubject(a.Owner) {
		return "", FieldError{"owner"}
	}
	if !ValidContactRef(a.ContactRef) {
		return "", FieldError{"contact_ref"}
	}
	if a.Via != "receipt" && a.Via != "invite" {
		return "", FieldError{"via"}
	}
	if title, err = checkMeeting(a.Meeting); err != nil {
		return "", err
	}
	return title, checkParticipant(a.Participant)
}

func checkMeeting(m Meeting) (string, error) {
	if !ValidRef(m.ID) {
		return "", FieldError{"meeting.id"}
	}
	t, err := CleanTitle(m.Title)
	if err != nil {
		return "", FieldError{"meeting.title"}
	}
	return t, nil
}

func checkParticipant(p Participant) error {
	if !ValidRef(p.ID) {
		return FieldError{"participant.id"}
	}
	if !ValidRef(p.Identity) {
		return FieldError{"participant.identity"}
	}
	if _, err := CleanName(p.TypedName); err != nil {
		return FieldError{"participant.typed_name"}
	}
	return nil
}

// String keeps a Submission out of a log line by accident: it names the
// fields' presence only.
func (s Submission) String() string {
	return fmt.Sprintf("submission{card for meeting %q}", s.Meeting.ID)
}
