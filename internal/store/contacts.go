package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZacxDev/muster-contacts/contactsapi"
)

// Owner is one person's view of the database: every method reads and
// writes that person's rows only. It is built by ForOwner alone, from a
// subject the caller already authenticated (an owner session, or the API's
// token and a subject meetings holds).
type Owner struct {
	s       *Store
	subject string
}

// ForOwner is subject's view. A value that is not a subject is
// ErrBadSubject.
func (s *Store) ForOwner(subject string) (*Owner, error) {
	if !ValidSubject(subject) {
		return nil, ErrBadSubject
	}
	return &Owner{s: s, subject: subject}, nil
}

// Subject is the person this view is for.
func (o *Owner) Subject() string { return o.subject }

// Limits on what the owner types.
const (
	MaxNameRunes   = 80
	MaxNoteRunes   = 2000
	MaxEmails      = 10
	MaxSearchRunes = 100
	ListLimit      = 50
)

// Email is one address of a contact.
type Email struct {
	Address string `json:"address"`
	Source  string `json:"source"`    // owner, self, invite
	Confirm bool   `json:"confirmed"` // phase 2: a confirmation link was followed
}

// MeetingLine is one meeting a contact was in.
type MeetingLine struct {
	MeetingID string     `json:"meeting_id"`
	Title     string     `json:"title"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	Role      string     `json:"role"` // guest, invitee
	Via       string     `json:"via"`  // card, invite, receipt, owner
	TypedName string     `json:"typed_name"`
	At        time.Time  `json:"at"`
	// Unconfirmed: the line came from a card, whose email nobody has
	// confirmed (until phase 2's verification), so the owner never reads an
	// unverified claim as fact.
	Unconfirmed bool `json:"unconfirmed"`
}

// Origin is one provenance line: how the contact came to be, or a merge.
type Origin struct {
	Kind         string    `json:"kind"` // owner_entered, self_submitted, invited, merged
	MeetingID    string    `json:"meeting_id,omitempty"`
	MeetingTitle string    `json:"meeting_title,omitempty"`
	SaidName     string    `json:"said_name,omitempty"`
	At           time.Time `json:"at"`
}

// Contact is one contact, whole: what the detail sheet draws and what
// Download gives.
type Contact struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	NameSource string        `json:"name_source"`
	Note       string        `json:"note"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	ActiveAt   time.Time     `json:"active_at"`
	Emails     []Email       `json:"emails"`
	Meetings   []MeetingLine `json:"meetings"`
	Provenance []Origin      `json:"provenance"`
}

// Row is one line of the list.
type Row struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	Origin      string    `json:"origin"` // the first provenance's kind
	OriginTitle string    `json:"origin_title"`
	ActiveAt    time.Time `json:"active_at"`
}

// FieldError is an owner's input that is refused, naming the field (and
// for an email, which one).
type FieldError struct {
	Field string
	Index int
}

func (e FieldError) Error() string { return "invalid field " + e.Field }

// ErrEmailTaken is an email another contact of this owner holds: the owner
// is offered Merge.
type ErrEmailTaken struct {
	ContactID string
	Name      string
	Address   string
}

func (e ErrEmailTaken) Error() string { return "another contact holds this email" }

// ContactIn is what the owner typed for a new contact.
type ContactIn struct {
	Name   string   `json:"name"`
	Emails []string `json:"emails"`
	Note   string   `json:"note"`
}

// ContactPatch is an edit: a nil field is left as it is.
type ContactPatch struct {
	Name   *string   `json:"name"`
	Emails *[]string `json:"emails"`
	Note   *string   `json:"note"`
}

type email struct{ address, key string }

func cleanName(s string) (string, error) {
	n, err := contactsapi.CleanText(s, MaxNameRunes)
	if err != nil {
		return "", FieldError{Field: "name"}
	}
	return n, nil
}

// cleanNote is the owner's note: up to MaxNoteRunes, line breaks and tabs
// allowed, no other control or format character (no text-direction tricks).
func cleanNote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxNoteRunes {
		return "", FieldError{Field: "note"}
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.Is(unicode.Cc, r) || (unicode.Is(unicode.Cf, r) && r != '‍') {
			return "", FieldError{Field: "note"}
		}
	}
	return s, nil
}

func cleanEmails(in []string) ([]email, error) {
	if len(in) > MaxEmails {
		return nil, FieldError{Field: "emails"}
	}
	var out []email
	seen := map[string]bool{}
	for i, raw := range in {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		a, k, err := contactsapi.CheckEmail(raw)
		if err != nil {
			return nil, FieldError{Field: "emails", Index: i}
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, email{a, k})
	}
	return out, nil
}

// isUnique says err is a UNIQUE or PRIMARY KEY constraint's refusal.
func isUnique(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// takenBy is the contact holding an email key, for ErrEmailTaken.
func (o *Owner) takenBy(ctx context.Context, tx *sql.Tx, e email) error {
	t := ErrEmailTaken{Address: e.address}
	if err := tx.QueryRowContext(ctx, `SELECT c.id, c.name FROM contact_emails x JOIN contacts c ON c.owner = ? AND c.id = x.contact_id WHERE x.owner = ? AND x.address_key = ?`,
		o.subject, o.subject, e.key).Scan(&t.ContactID, &t.Name); err != nil {
		return err
	}
	return t
}

// addEmail stores an address for a contact. 🔴 The one-contact-per-email
// rule is the table's primary key (owner, address_key), not a lookup first:
// another contact's key is the constraint's refusal, answered as
// ErrEmailTaken.
func (o *Owner) addEmail(ctx context.Context, tx *sql.Tx, id string, e email, source string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO contact_emails (contact_id, owner, address, address_key, source, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, o.subject, e.address, e.key, source, ms(now))
	if isUnique(err) {
		return o.takenBy(ctx, tx, e)
	}
	return err
}

// Create is Add: a contact the owner typed in.
func (o *Owner) Create(ctx context.Context, in ContactIn, now time.Time) (Contact, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return Contact{}, err
	}
	note, err := cleanNote(in.Note)
	if err != nil {
		return Contact{}, err
	}
	emails, err := cleanEmails(in.Emails)
	if err != nil {
		return Contact{}, err
	}
	if name == "" && len(emails) == 0 {
		return Contact{}, FieldError{Field: "name"}
	}
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	defer tx.Rollback()
	if err := o.ensureOwner(ctx, tx, now); err != nil {
		return Contact{}, err
	}
	id := NewContactID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO contacts (id, owner, name, name_source, note, created_at, updated_at, active_at) VALUES (?, ?, ?, 'owner', ?, ?, ?, ?)`,
		id, o.subject, name, note, ms(now), ms(now), ms(now)); err != nil {
		return Contact{}, err
	}
	for _, e := range emails {
		if err := o.addEmail(ctx, tx, id, e, "owner", now); err != nil {
			return Contact{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provenance (contact_id, owner, kind, at) VALUES (?, ?, 'owner_entered', ?)`, id, o.subject, ms(now)); err != nil {
		return Contact{}, err
	}
	if err := audit(ctx, tx, now, o.subject, "contact.created", "by the owner"); err != nil {
		return Contact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	return o.Get(ctx, id)
}

// Update is Edit. A name the owner sets is the owner's from then on: no
// card renames the contact again (merge rule 2).
func (o *Owner) Update(ctx context.Context, id string, p ContactPatch, now time.Time) (Contact, error) {
	if !ValidContactID(id) {
		return Contact{}, ErrNotFound
	}
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM contacts WHERE id = ? AND owner = ?`, id, o.subject).Scan(&cur); errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	} else if err != nil {
		return Contact{}, err
	}
	if p.Name != nil {
		name, err := cleanName(*p.Name)
		if err != nil {
			return Contact{}, err
		}
		if name != cur {
			if _, err := tx.ExecContext(ctx, `UPDATE contacts SET name = ?, name_source = 'owner', updated_at = ? WHERE id = ? AND owner = ?`, name, ms(now), id, o.subject); err != nil {
				return Contact{}, err
			}
		}
	}
	if p.Note != nil {
		note, err := cleanNote(*p.Note)
		if err != nil {
			return Contact{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET note = ?, updated_at = ? WHERE id = ? AND owner = ?`, note, ms(now), id, o.subject); err != nil {
			return Contact{}, err
		}
	}
	if p.Emails != nil {
		want, err := cleanEmails(*p.Emails)
		if err != nil {
			return Contact{}, err
		}
		if err := o.setEmails(ctx, tx, id, want, now); err != nil {
			return Contact{}, err
		}
	}
	var name string
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT c.name, (SELECT count(*) FROM contact_emails e WHERE e.owner = ? AND e.contact_id = c.id) FROM contacts c WHERE c.id = ? AND c.owner = ?`,
		o.subject, id, o.subject).Scan(&name, &n); err != nil {
		return Contact{}, err
	}
	if name == "" && n == 0 {
		return Contact{}, FieldError{Field: "name"}
	}
	if err := audit(ctx, tx, now, o.subject, "contact.updated", ""); err != nil {
		return Contact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	return o.Get(ctx, id)
}

// setEmails makes a contact's addresses exactly want: removed keys go,
// new ones are added as the owner's, and a kept key keeps its source and
// confirmation (its display form follows what the owner typed).
func (o *Owner) setEmails(ctx context.Context, tx *sql.Tx, id string, want []email, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT address_key, address FROM contact_emails WHERE contact_id = ? AND owner = ?`, id, o.subject)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for rows.Next() {
		var k, a string
		if err := rows.Scan(&k, &a); err != nil {
			rows.Close()
			return err
		}
		have[k] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, e := range want {
		keep[e.key] = true
	}
	for k := range have {
		if !keep[k] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM contact_emails WHERE contact_id = ? AND owner = ? AND address_key = ?`, id, o.subject, k); err != nil {
				return err
			}
		}
	}
	for _, e := range want {
		a, ok := have[e.key]
		switch {
		case !ok:
			if err := o.addEmail(ctx, tx, id, e, "owner", now); err != nil {
				return err
			}
		case a != e.address:
			if _, err := tx.ExecContext(ctx, `UPDATE contact_emails SET address = ? WHERE contact_id = ? AND owner = ? AND address_key = ?`, e.address, id, o.subject, e.key); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete deletes a contact for real: its row and, by cascade, its emails,
// handles, provenance, meeting links, receipts and idempotency rows; then
// every meeting row no link is left on; then a checkpoint, so secure_delete's
// zeroed pages are what is on disk. The audit row says nothing about whom.
// meetings' own records of the calls are not touched (they are meetings').
func (o *Owner) Delete(ctx context.Context, id string, now time.Time) error {
	if !ValidContactID(id) {
		return ErrNotFound
	}
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ? AND owner = ?`, id, o.subject)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := o.dropOrphanMeetings(ctx, tx); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, o.subject, "contact.deleted", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return o.s.checkpoint(ctx)
}

// dropOrphanMeetings deletes this owner's meeting rows that no contact is
// linked to any more (the design keeps no meeting without a contact).
func (o *Owner) dropOrphanMeetings(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meetings WHERE owner = ? AND NOT EXISTS (SELECT 1 FROM meeting_links l WHERE l.owner = ? AND l.meeting_id = meetings.meeting_id)`,
		o.subject, o.subject)
	return err
}

// DeleteAll is Delete everything: every contact, and so every email,
// handle, provenance line, meeting link, receipt and key; every meeting
// row; and the owner's audit trail. The owner's own row (their settings)
// and session stay: they are how the page keeps working.
func (o *Owner) DeleteAll(ctx context.Context, now time.Time) error {
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM contacts WHERE owner = ?`,
		`DELETE FROM meetings WHERE owner = ?`,
		`DELETE FROM idempotency WHERE owner = ?`,
		`DELETE FROM audit WHERE owner = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, o.subject); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, now, o.subject, "owner.erased", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return o.s.checkpoint(ctx)
}

// Merge moves every email, handle, meeting link, receipt, key and
// provenance line of from onto keep, adds a merged line, and deletes from:
// one transaction (merge rule 4). keep keeps its name (or takes from's when
// it has none).
func (o *Owner) Merge(ctx context.Context, keep, from string, now time.Time) (Contact, error) {
	if !ValidContactID(keep) || !ValidContactID(from) {
		return Contact{}, ErrNotFound
	}
	if keep == from {
		return Contact{}, FieldError{Field: "from"}
	}
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	defer tx.Rollback()
	var keepName, fromName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM contacts WHERE id = ? AND owner = ?`, keep, o.subject).Scan(&keepName); errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	} else if err != nil {
		return Contact{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT name FROM contacts WHERE id = ? AND owner = ?`, from, o.subject).Scan(&fromName); errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	} else if err != nil {
		return Contact{}, err
	}
	for _, q := range []string{
		`UPDATE contact_emails SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
		`UPDATE contact_handles SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
		`UPDATE provenance SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
		`UPDATE meeting_links SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
		`UPDATE receipts SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
		`UPDATE idempotency SET contact_id = ? WHERE contact_id = ? AND owner = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, keep, from, o.subject); err != nil {
			return Contact{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provenance (contact_id, owner, kind, said_name, at) VALUES (?, ?, 'merged', ?, ?)`, keep, o.subject, fromName, ms(now)); err != nil {
		return Contact{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contacts SET name = CASE WHEN name = '' THEN ? ELSE name END, active_at = ?, updated_at = ? WHERE id = ? AND owner = ?`,
		fromName, ms(now), ms(now), keep, o.subject); err != nil {
		return Contact{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ? AND owner = ?`, from, o.subject); err != nil {
		return Contact{}, err
	}
	if err := audit(ctx, tx, now, o.subject, "contact.merged", "by the owner"); err != nil {
		return Contact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	return o.Get(ctx, keep)
}

// Get is one contact, whole. Another owner's contact is ErrNotFound, as an
// unknown one is.
func (o *Owner) Get(ctx context.Context, id string) (Contact, error) {
	if !ValidContactID(id) {
		return Contact{}, ErrNotFound
	}
	var c Contact
	var created, updated, active int64
	err := o.s.db.QueryRowContext(ctx, `SELECT id, name, name_source, note, created_at, updated_at, active_at FROM contacts WHERE id = ? AND owner = ?`, id, o.subject).
		Scan(&c.ID, &c.Name, &c.NameSource, &c.Note, &created, &updated, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, err
	}
	c.CreatedAt, c.UpdatedAt, c.ActiveAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC(), time.UnixMilli(active).UTC()
	c.Emails, c.Meetings, c.Provenance = []Email{}, []MeetingLine{}, []Origin{}

	rows, err := o.s.db.QueryContext(ctx, `SELECT address, source, verified_at FROM contact_emails WHERE contact_id = ? AND owner = ? ORDER BY created_at, address_key`, id, o.subject)
	if err != nil {
		return Contact{}, err
	}
	for rows.Next() {
		var e Email
		var v sql.NullInt64
		if err := rows.Scan(&e.Address, &e.Source, &v); err != nil {
			rows.Close()
			return Contact{}, err
		}
		e.Confirm = v.Valid
		c.Emails = append(c.Emails, e)
	}
	rows.Close()

	rows, err = o.s.db.QueryContext(ctx, `SELECT l.meeting_id, m.title, m.started_at, m.ended_at, l.role, l.via, l.typed_name, l.at
		FROM meeting_links l JOIN meetings m ON m.owner = ? AND m.meeting_id = l.meeting_id
		WHERE l.contact_id = ? AND l.owner = ? ORDER BY coalesce(m.started_at, l.at) DESC, l.meeting_id`, o.subject, id, o.subject)
	if err != nil {
		return Contact{}, err
	}
	for rows.Next() {
		var m MeetingLine
		var st, en sql.NullInt64
		var at int64
		if err := rows.Scan(&m.MeetingID, &m.Title, &st, &en, &m.Role, &m.Via, &m.TypedName, &at); err != nil {
			rows.Close()
			return Contact{}, err
		}
		m.StartedAt, m.EndedAt, m.At = fromMS(st), fromMS(en), time.UnixMilli(at).UTC()
		m.Unconfirmed = m.Via == "card"
		c.Meetings = append(c.Meetings, m)
	}
	rows.Close()

	rows, err = o.s.db.QueryContext(ctx, `SELECT p.kind, coalesce(p.meeting_id, ''), coalesce(m.title, ''), p.said_name, p.at
		FROM provenance p LEFT JOIN meetings m ON m.owner = ? AND m.meeting_id = p.meeting_id
		WHERE p.contact_id = ? AND p.owner = ? ORDER BY p.id`, o.subject, id, o.subject)
	if err != nil {
		return Contact{}, err
	}
	for rows.Next() {
		var p Origin
		var at int64
		if err := rows.Scan(&p.Kind, &p.MeetingID, &p.MeetingTitle, &p.SaidName, &at); err != nil {
			rows.Close()
			return Contact{}, err
		}
		p.At = time.UnixMilli(at).UTC()
		c.Provenance = append(c.Provenance, p)
	}
	rows.Close()
	return c, rows.Err()
}

// Cursor is where a list page ended.
type Cursor struct {
	ActiveAt int64
	ID       string
}

// String is the cursor as the API carries it.
func (c Cursor) String() string { return strconv.FormatInt(c.ActiveAt, 10) + "." + c.ID }

// ParseCursor reads a cursor an earlier page answered.
func ParseCursor(s string) (Cursor, bool) {
	a, id, ok := strings.Cut(s, ".")
	n, err := strconv.ParseInt(a, 10, 64)
	if !ok || err != nil || !ValidContactID(id) {
		return Cursor{}, false
	}
	return Cursor{n, id}, true
}

// likeEscape makes s a LIKE pattern matching s anywhere, literally.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// List is the list: newest activity first, filtered by q (a name or an
// email, anywhere in it, case aside for ASCII), limit at a time.
func (o *Owner) List(ctx context.Context, q string, after *Cursor, limit int) ([]Row, *Cursor, error) {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > MaxSearchRunes {
		return nil, nil, FieldError{Field: "q"}
	}
	if limit <= 0 {
		limit = ListLimit
	}
	pat := likeEscape(q)
	var aAt int64
	aID := ""
	if after != nil {
		aAt, aID = after.ActiveAt, after.ID
	}
	rows, err := o.s.db.QueryContext(ctx, `SELECT c.id, c.name, c.active_at,
			coalesce((SELECT e.address FROM contact_emails e WHERE e.owner = ? AND e.contact_id = c.id ORDER BY e.created_at, e.address_key LIMIT 1), ''),
			coalesce((SELECT p.kind FROM provenance p WHERE p.owner = ? AND p.contact_id = c.id ORDER BY p.id LIMIT 1), ''),
			coalesce((SELECT m.title FROM provenance p2 LEFT JOIN meetings m ON m.owner = ? AND m.meeting_id = p2.meeting_id WHERE p2.owner = ? AND p2.contact_id = c.id ORDER BY p2.id LIMIT 1), '')
		FROM contacts c
		WHERE c.owner = ?
		  AND (? = '' OR c.name LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM contact_emails e2 WHERE e2.owner = ? AND e2.contact_id = c.id AND e2.address LIKE ? ESCAPE '\'))
		  AND (? = '' OR c.active_at < ? OR (c.active_at = ? AND c.id < ?))
		ORDER BY c.active_at DESC, c.id DESC LIMIT ?`,
		o.subject, o.subject, o.subject, o.subject, o.subject,
		q, pat, o.subject, pat,
		aID, aAt, aAt, aID,
		limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var at int64
		if err := rows.Scan(&r.ID, &r.Name, &at, &r.Email, &r.Origin, &r.OriginTitle); err != nil {
			return nil, nil, err
		}
		r.ActiveAt = time.UnixMilli(at).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &Cursor{ms(last.ActiveAt), last.ID}
	}
	return out, next, nil
}

// Export is every contact, whole (Download all).
func (o *Owner) Export(ctx context.Context) ([]Contact, error) {
	rows, err := o.s.db.QueryContext(ctx, `SELECT id FROM contacts WHERE owner = ? ORDER BY active_at DESC, id DESC`, o.subject)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Contact{}
	for _, id := range ids {
		c, err := o.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Settings is the owner's settings.
type Settings struct {
	Collect bool `json:"collect"` // "Add people from Meetings"
}

// Settings reads them; a person with no row has the defaults.
func (o *Owner) Settings(ctx context.Context) (Settings, error) {
	st, err := o.s.StandingOf(ctx, o.subject)
	return Settings{Collect: st.Collect}, err
}

// SetCollect turns "Add people from Meetings" on or off.
func (o *Owner) SetCollect(ctx context.Context, on bool, now time.Time) error {
	v := 0
	if on {
		v = 1
	}
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owners (subject, redeemed, collect, updated_at) VALUES (?, 0, ?, ?)
		ON CONFLICT (subject) DO UPDATE SET collect = excluded.collect, updated_at = excluded.updated_at`, o.subject, v, ms(now)); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, o.subject, "settings.collect", fmt.Sprintf("collect=%d", v)); err != nil {
		return err
	}
	return tx.Commit()
}
