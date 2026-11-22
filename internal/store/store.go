// Package store is the plugin's SQLite database (design: "Data model (the
// plugin's SQLite)"): owners and their sessions, contacts, their emails,
// where each came from, the meetings they were in, the guest browsers'
// receipts, the API's idempotency keys, and an audit trail that never holds
// a name, an email, a title or a note.
//
// An owner is a person muster's server-app redeem named, known only by the
// subject it gave (for a linked pair, the subject meetings knows them by
// too) and a display name. 🔴 Nothing here holds a muster user id or an
// email of the owner; the plugin is never told either.
//
// 🔴 Owner scoping is structural. Every read and write of a person's data
// goes through Owner, which is built only by ForOwner and carries the subject
// into every statement, and every statement on a person table filters on
// `owner = ?` once for each table it names (TestEverySQLStatementIsOwnerScoped
// reads this package's source and fails on one that does not). A child row's
// owner is tied to its contact's by a composite foreign key, so a row cannot
// hang under another owner's contact.
//
// Times are stored as Unix milliseconds (UTC). Tokens and receipts are
// stored only as their SHA-256 (Hash).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, no cgo
)

// SessionLifetime is how long an owner session lives (as meetings': 8 h).
const SessionLifetime = 8 * time.Hour

// IdempotencyLifetime is how long an API write's key is kept (design: 7 days).
const IdempotencyLifetime = 7 * 24 * time.Hour

// ErrNotFound is a row that does not exist or is not this owner's: the two
// are one error, so no caller can tell them apart.
var ErrNotFound = errors.New("not found")

// PersonTables are the tables that hold a person's data: every statement on
// one is owner-scoped, and a contact's delete reaches every one of them
// (TestDeletingAContactLeavesNothingInAnyTable reads the schema, and fails on
// a table not listed here or in otherTables).
var PersonTables = []string{"contacts", "contact_emails", "contact_handles", "provenance", "meetings", "meeting_links", "receipts", "idempotency"}

// otherTables are the tables that hold no contact's data: the owner's own
// row and sessions, and the audit trail (which holds no PII).
var otherTables = []string{"owners", "owner_sessions", "audit"}

// migrations are applied in order; PRAGMA user_version counts those done.
// Never edit one that has shipped: add the next.
var migrations = []string{
	// 1: phase 1 (design: "Data model"). contact_handles is created empty
	// for phase 3, so its rows need no migration that rewrites these.
	`CREATE TABLE owners (
		subject      TEXT    PRIMARY KEY CHECK (length(subject) = 22), -- the (pair's) subject; never a muster user id
		display_name TEXT    NOT NULL DEFAULT '',                       -- from the redeem; never an email
		redeemed     INTEGER NOT NULL CHECK (redeemed IN (0, 1)),       -- 1 once this person opened Contacts
		redeemed_at  INTEGER,                                           -- their last open
		revoked      INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)), -- muster answered them revoked since
		collect      INTEGER NOT NULL DEFAULT 1 CHECK (collect IN (0, 1)), -- Settings: "Add people from Meetings"
		updated_at   INTEGER NOT NULL
	) STRICT;
	CREATE TABLE owner_sessions (
		token_sha256 TEXT    PRIMARY KEY,
		subject      TEXT    NOT NULL REFERENCES owners (subject) ON DELETE CASCADE,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	) STRICT;
	CREATE INDEX owner_sessions_subject ON owner_sessions (subject);
	CREATE TABLE contacts (
		id          TEXT    PRIMARY KEY CHECK (length(id) = 22), -- random; the contact_ref meetings holds
		owner       TEXT    NOT NULL REFERENCES owners (subject) ON DELETE CASCADE,
		name        TEXT    NOT NULL DEFAULT '',
		name_source TEXT    NOT NULL CHECK (name_source IN ('owner', 'self', 'invite')),
		note        TEXT    NOT NULL DEFAULT '',                 -- the owner's own words, at most 2,000
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		active_at   INTEGER NOT NULL,                            -- the list's order: made, merged into, met
		UNIQUE (id, owner)
	) STRICT;
	CREATE INDEX contacts_owner_active ON contacts (owner, active_at DESC, id DESC);
	CREATE TABLE contact_emails (
		contact_id  TEXT    NOT NULL,
		owner       TEXT    NOT NULL,
		address     TEXT    NOT NULL,                            -- as given, for display and sending
		address_key TEXT    NOT NULL,                            -- the merge key (contactsapi.CheckEmail)
		verified_at INTEGER,                                     -- phase 2: a confirmation link was followed
		source      TEXT    NOT NULL CHECK (source IN ('owner', 'self', 'invite')),
		created_at  INTEGER NOT NULL,
		PRIMARY KEY (owner, address_key),                        -- 🔴 one contact per email per owner
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE
	) STRICT;
	CREATE INDEX contact_emails_contact ON contact_emails (contact_id);
	CREATE TABLE contact_handles (
		contact_id TEXT    NOT NULL,
		owner      TEXT    NOT NULL,
		kind       TEXT    NOT NULL CHECK (kind IN ('whatsapp', 'signal', 'discord', 'email')),
		value      TEXT    NOT NULL,
		value_key  TEXT    NOT NULL,
		source     TEXT    NOT NULL CHECK (source IN ('owner', 'self', 'inbound')),
		created_at INTEGER NOT NULL,
		PRIMARY KEY (owner, kind, value_key),
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE
	) STRICT;
	CREATE INDEX contact_handles_contact ON contact_handles (contact_id);
	CREATE TABLE provenance (
		id         INTEGER PRIMARY KEY,
		contact_id TEXT    NOT NULL,
		owner      TEXT    NOT NULL,
		kind       TEXT    NOT NULL CHECK (kind IN ('owner_entered', 'self_submitted', 'invited', 'merged')),
		meeting_id TEXT,                                         -- self_submitted, invited, a card's merge: which meeting
		said_name  TEXT    NOT NULL DEFAULT '',                  -- a card's name when it differs; the other contact's on a merge by hand
		at         INTEGER NOT NULL,
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE
	) STRICT;
	CREATE INDEX provenance_contact ON provenance (contact_id, id);
	CREATE TABLE meetings (                                      -- only meetings some contact is linked to
		owner      TEXT    NOT NULL REFERENCES owners (subject) ON DELETE CASCADE,
		meeting_id TEXT    NOT NULL,                             -- meetings' own id
		title      TEXT    NOT NULL DEFAULT '',
		started_at INTEGER,
		ended_at   INTEGER,
		PRIMARY KEY (owner, meeting_id)
	) STRICT;
	CREATE TABLE meeting_links (                                 -- "met at meeting X"
		owner          TEXT    NOT NULL,
		contact_id     TEXT    NOT NULL,
		meeting_id     TEXT    NOT NULL,
		participant_id TEXT    NOT NULL,                         -- meetings' participants.id
		identity       TEXT    NOT NULL,                         -- meetings' LiveKit identity
		role           TEXT    NOT NULL CHECK (role IN ('guest', 'invitee')),
		typed_name     TEXT    NOT NULL,                         -- what they typed at the knock
		via            TEXT    NOT NULL CHECK (via IN ('card', 'invite', 'receipt', 'owner')),
		joined_at      INTEGER,
		left_at        INTEGER,
		at             INTEGER NOT NULL,
		PRIMARY KEY (owner, meeting_id, participant_id),
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE,
		FOREIGN KEY (owner, meeting_id) REFERENCES meetings (owner, meeting_id)
	) STRICT;
	CREATE INDEX meeting_links_contact ON meeting_links (contact_id);
	CREATE TABLE receipts (                                      -- a guest browser's proof of an earlier card
		receipt_sha256 TEXT    PRIMARY KEY,
		owner          TEXT    NOT NULL,
		contact_id     TEXT    NOT NULL,
		created_at     INTEGER NOT NULL,
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE
	) STRICT;
	CREATE INDEX receipts_contact ON receipts (contact_id);
	CREATE TABLE idempotency (
		owner          TEXT    NOT NULL,
		key            TEXT    NOT NULL,                         -- 'card:<id>', 'attend:<meeting>:<participant>'
		request_sha256 TEXT    NOT NULL,
		status         INTEGER NOT NULL,                         -- the answer's status; never its body
		contact_id     TEXT,
		at             INTEGER NOT NULL,
		PRIMARY KEY (owner, key),
		FOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE
	) STRICT;
	CREATE INDEX idempotency_contact ON idempotency (contact_id);
	CREATE TABLE audit (
		at     INTEGER NOT NULL,
		owner  TEXT,
		event  TEXT    NOT NULL,                                 -- 'contact.created', 'contact.deleted', 'link.made', …
		detail TEXT    NOT NULL DEFAULT ''                       -- 🔴 never a name, email, handle, title or note
	) STRICT;`,
}

// Store is the database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) the database at path and brings its schema up to
// date. The directory is created 0700 and the file is 0600. Every
// connection has foreign keys on and secure_delete on (a deleted row's
// bytes are zeroed, not left in a free page).
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "secure_delete(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One writer (one replica, one SQLite file): a single connection
	// serialises every statement, so no request ever sees SQLITE_BUSY and
	// a read-then-write in one transaction cannot race another.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path is the database file.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("the database is at schema %d, newer than this build's %d: run a newer muster-contacts", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// checkpoint writes the WAL back into the database file and truncates it,
// so the zeroed pages secure_delete left are what is on disk (and the
// deleted rows' old page images in the WAL are gone).
func (s *Store) checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func nullMS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ms(*t)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// audit records an event. detail is counts and ids only: 🔴 never a name,
// an email, a title or a note (every caller passes a constant or counts).
func audit(ctx context.Context, x execer, now time.Time, owner, event, detail string) error {
	var o any
	if owner != "" {
		o = owner
	}
	_, err := x.ExecContext(ctx, `INSERT INTO audit (at, owner, event, detail) VALUES (?, ?, ?, ?)`, ms(now), o, event, detail)
	return err
}

// ---- owners and their sessions ----

// Session is a live owner session.
type Session struct {
	Subject   string
	ExpiresAt time.Time
}

// ErrBadSubject is a subject that is not 22 base64url characters.
var ErrBadSubject = errors.New("not a subject")

// CreateSession records the person a redeem named (subject and display
// name: they opened Contacts, so they are redeemed and not revoked) and
// mints their owner session, valid SessionLifetime from now. It returns the
// token, which is stored only as its hash.
func (s *Store) CreateSession(ctx context.Context, subject, displayName string, now time.Time) (string, error) {
	if !ValidSubject(subject) {
		return "", ErrBadSubject
	}
	tok := NewSessionToken()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owners (subject, display_name, redeemed, redeemed_at, revoked, updated_at) VALUES (?, ?, 1, ?, 0, ?)
		ON CONFLICT (subject) DO UPDATE SET display_name = excluded.display_name, redeemed = 1, redeemed_at = excluded.redeemed_at, revoked = 0, updated_at = excluded.updated_at`,
		subject, displayName, ms(now), ms(now)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_sessions (token_sha256, subject, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		Hash(tok), subject, ms(now), ms(now.Add(SessionLifetime))); err != nil {
		return "", err
	}
	if err := audit(ctx, tx, now, subject, "session.created", ""); err != nil {
		return "", err
	}
	return tok, tx.Commit()
}

// LookupSession returns the live session token opens at now. A malformed,
// unknown or expired token is ErrNotFound.
func (s *Store) LookupSession(ctx context.Context, token string, now time.Time) (Session, error) {
	if !ValidSessionToken(token) {
		return Session{}, ErrNotFound
	}
	var out Session
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT subject, expires_at FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,
		Hash(token), ms(now)).Scan(&out.Subject, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	out.ExpiresAt = time.UnixMilli(exp).UTC()
	return out, nil
}

// RedeemedSubjects is every person who has opened Contacts: the subjects
// the owners/check poll asks muster about (a person who never opened it
// has no grant under this app, and muster would answer "revoked" for them,
// the same as for one who hid it; design: open question 1, decision 8).
func (s *Store) RedeemedSubjects(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject FROM owners WHERE redeemed = 1 ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Judged is what Judge did.
type Judged struct {
	Sessions int64 // owner sessions deleted
	Revoked  int64 // people newly refused
}

// Judge acts on one owners/check answer, sent at asOf: 🔴 revoked people's
// sessions are deleted and their writes refused from now on; expired
// people's sessions are deleted and they stay accepting (expired only means
// they have not opened the tab in 8 hours); live and expired people are
// accepting again (they showed the app again). A session or an open made
// after asOf is newer than the answer and is kept: a person who reopened
// the tab while the answer was on its way is not refused by it.
func (s *Store) Judge(ctx context.Context, live, expired, revoked []string, asOf, now time.Time) (Judged, error) {
	var out Judged
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	ended := jsonStrings(append(append([]string{}, expired...), revoked...))
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE subject IN (SELECT value FROM json_each(?)) AND created_at <= ?`, ended, ms(asOf))
	if err != nil {
		return out, err
	}
	out.Sessions, _ = r.RowsAffected()
	r, err = tx.ExecContext(ctx, `UPDATE owners SET revoked = 1, updated_at = ? WHERE subject IN (SELECT value FROM json_each(?)) AND revoked = 0 AND redeemed_at <= ?`,
		ms(now), jsonStrings(revoked), ms(asOf))
	if err != nil {
		return out, err
	}
	out.Revoked, _ = r.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE owners SET revoked = 0, updated_at = ? WHERE subject IN (SELECT value FROM json_each(?)) AND revoked = 1`,
		ms(now), jsonStrings(append(append([]string{}, live...), expired...))); err != nil {
		return out, err
	}
	if out.Sessions > 0 || out.Revoked > 0 {
		if err := audit(ctx, tx, now, "", "owners.judged", fmt.Sprintf("%d sessions deleted, %d people refused", out.Sessions, out.Revoked)); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// EndAll acts on muster's word that the server token is dead (a 401: the
// app was uninstalled, or its token rotated away): every owner session is
// deleted. The data stays on the volume (README, "Rollback").
func (s *Store) EndAll(ctx context.Context, why string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE 1`)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if err := audit(ctx, tx, now, "", "app.revoked", fmt.Sprintf("%s: %d sessions deleted", why, n)); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Standing is what the API needs to know of an owner before a write.
type Standing struct {
	Known     bool // an owners row exists
	Redeemed  bool
	Revoked   bool
	Collect   bool
	Accepting bool // known or not, not revoked and collecting
}

// StandingOf reads an owner's standing. A person never seen is accepting
// (decision 8: cards are kept from their very first meeting).
func (s *Store) StandingOf(ctx context.Context, subject string) (Standing, error) {
	return standingOf(ctx, s.db, subject)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func standingOf(ctx context.Context, q querier, subject string) (Standing, error) {
	var st Standing
	var redeemed, revoked, collect int
	err := q.QueryRowContext(ctx, `SELECT redeemed, revoked, collect FROM owners WHERE subject = ?`, subject).Scan(&redeemed, &revoked, &collect)
	if errors.Is(err, sql.ErrNoRows) {
		return Standing{Collect: true, Accepting: true}, nil
	}
	if err != nil {
		return st, err
	}
	st = Standing{Known: true, Redeemed: redeemed == 1, Revoked: revoked == 1, Collect: collect == 1}
	st.Accepting = !st.Revoked && st.Collect
	return st, nil
}

// Sweep deletes expired sessions and idempotency keys past their lifetime.
func (s *Store) Sweep(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE expires_at <= ?`, ms(now)); err != nil {
		return err
	}
	// Keys of every owner past their lifetime: the one person-table
	// statement with no owner (the ledger names it).
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE at <= ?`, ms(now.Add(-IdempotencyLifetime))); err != nil {
		return err
	}
	return tx.Commit()
}

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func jsonStrings(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
