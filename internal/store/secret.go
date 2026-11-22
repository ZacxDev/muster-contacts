package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
)

// The plugin's ids and tokens, all base64url without padding.
const (
	// SessionTokenBytes is an owner session's token: 32 bytes, 43
	// characters after SessionPrefix.
	SessionTokenBytes = 32
	// ContactIDBytes is a contact's id: 128 bits, 22 characters. It is the
	// contact_ref meetings holds; random, so ids reveal no count.
	ContactIDBytes = 16
)

// SessionPrefix starts every owner session token ("muster contacts owner").
const SessionPrefix = "mco_"

var (
	sessionTokenRE = regexp.MustCompile(`^mco_[A-Za-z0-9_-]{43}$`)
	idRE           = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform Go supports; a plugin
		// that cannot draw a secret must not make up a weak one.
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewSessionToken is a fresh owner session token, with its prefix.
func NewSessionToken() string { return SessionPrefix + random(SessionTokenBytes) }

// NewContactID is a fresh contact id.
func NewContactID() string { return random(ContactIDBytes) }

// ValidSessionToken checks a bearer's shape before it is hashed.
func ValidSessionToken(s string) bool { return sessionTokenRE.MatchString(s) }

// ValidContactID is a contact id's shape.
func ValidContactID(s string) bool { return idRE.MatchString(s) }

// ValidSubject is a muster subject's shape: 22 base64url characters.
func ValidSubject(s string) bool { return idRE.MatchString(s) }

// Hash is the stored form of a token: hex SHA-256.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
