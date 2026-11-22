package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
)

// The API's writes (design: "The contacts API"): a guest's card, a known
// guest's attendance, a receipt's check. Each runs in one transaction with
// its idempotency key: the same key and body answer again and change
// nothing; the same key and another body is ErrKeyReused.

// ErrNotAccepting is an owner who refuses writes: muster answered them
// revoked, or they turned "Add people from Meetings" off.
var ErrNotAccepting = errors.New("not accepting")

// ErrKeyReused is an idempotency key sent again with a different body.
var ErrKeyReused = errors.New("idempotency key reused with a different body")

// Card is a checked submission (contactsapi.Submission.Check).
type Card struct {
	Key         string // contactsapi.KeyForCard
	RequestHash string // the body's hash, for the key
	Name        string
	Address     string
	AddressKey  string
	Title       string
	Meeting     contactsapi.Meeting
	Participant contactsapi.Participant
}

// The outcomes of a card. 🔴 Only tests read one: the API answers the same
// to all of them (merge rule 5).
const (
	CardCreated  = "created"
	CardMerged   = "merged"
	CardDropped  = "dropped"
	CardReplayed = "replayed"
)

// CardResult is what Submit did, and the receipt to answer. Stored says the
// receipt checks known from now on (it is not, for a dropped card).
type CardResult struct {
	Outcome   string
	Receipt   string
	Stored    bool
	ContactID string
}

// remembered reads a key: (true, status, contact) when it was used with
// this body, ErrKeyReused when with another.
func (o *Owner) remembered(ctx context.Context, tx *sql.Tx, key, hash string) (bool, int, sql.NullString, error) {
	var h string
	var status int
	var contact sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT request_sha256, status, contact_id FROM idempotency WHERE owner = ? AND key = ?`, o.subject, key).Scan(&h, &status, &contact)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, contact, nil
	}
	if err != nil {
		return false, 0, contact, err
	}
	if h != hash {
		return false, 0, contact, ErrKeyReused
	}
	return true, status, contact, nil
}

func (o *Owner) remember(ctx context.Context, tx *sql.Tx, key, hash string, status int, contact string, now time.Time) error {
	var c any
	if contact != "" {
		c = contact
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO idempotency (owner, key, request_sha256, status, contact_id, at) VALUES (?, ?, ?, ?, ?, ?)`,
		o.subject, key, hash, status, c, ms(now))
	return err
}

func (o *Owner) ensureOwner(ctx context.Context, tx *sql.Tx, now time.Time) error {
	// A person who never opened Contacts is accepted (decision 8): their
	// row is made here, redeemed = 0, and owners/check never asks of them.
	_, err := tx.ExecContext(ctx, `INSERT INTO owners (subject, redeemed, updated_at) VALUES (?, 0, ?) ON CONFLICT (subject) DO NOTHING`, o.subject, ms(now))
	return err
}

func (o *Owner) upsertMeeting(ctx context.Context, tx *sql.Tx, title string, m contactsapi.Meeting) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meetings (owner, meeting_id, title, started_at, ended_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (owner, meeting_id) DO UPDATE SET title = excluded.title,
			started_at = coalesce(excluded.started_at, meetings.started_at), ended_at = coalesce(excluded.ended_at, meetings.ended_at)`,
		o.subject, m.ID, title, nullMS(m.StartedAt), nullMS(m.EndedAt))
	return err
}

func (o *Owner) link(ctx context.Context, tx *sql.Tx, contact, role, via string, m contactsapi.Meeting, p contactsapi.Participant, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meeting_links (owner, contact_id, meeting_id, participant_id, identity, role, typed_name, via, joined_at, left_at, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (owner, meeting_id, participant_id) DO NOTHING`,
		o.subject, contact, m.ID, p.ID, p.Identity, role, p.TypedName, via, nullMS(p.JoinedAt), nullMS(p.LeftAt), ms(now))
	return err
}

// Submit is a guest's card (decision 5): a known email key merges into its
// contact, an unknown one makes a contact marked self-submitted, and the
// meeting is linked either way. Nothing is kept when the owner is not
// accepting (the card is dropped). admit is the rate limit, asked only for
// a card that is not a replay; its error is returned as it is.
//
// Merge rules (design): the email key is the only match; a name the owner
// set is never overwritten by a card (the card's name goes in said_name),
// and a name that came from a card takes a newer card's.
func (o *Owner) Submit(ctx context.Context, c Card, admit func() error, now time.Time) (CardResult, error) {
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return CardResult{}, err
	}
	defer tx.Rollback()
	receipt := NewReceipt()
	seen, _, contact, err := o.remembered(ctx, tx, c.Key, c.RequestHash)
	if err != nil {
		return CardResult{}, err
	}
	if seen {
		// A retry after a lost answer: the first answer's receipt was never
		// read, so a fresh one is minted for the same contact.
		res := CardResult{Outcome: CardReplayed, Receipt: receipt}
		if contact.Valid {
			if err := o.storeReceipt(ctx, tx, receipt, contact.String, now); err != nil {
				return CardResult{}, err
			}
			res.Stored, res.ContactID = true, contact.String
		}
		return res, tx.Commit()
	}
	if err := admit(); err != nil {
		return CardResult{}, err
	}
	st, err := standingOf(ctx, tx, o.subject)
	if err != nil {
		return CardResult{}, err
	}
	if !st.Accepting {
		if err := o.remember(ctx, tx, c.Key, c.RequestHash, 202, "", now); err != nil {
			return CardResult{}, err
		}
		if err := audit(ctx, tx, now, o.subject, "card.dropped", "not accepting"); err != nil {
			return CardResult{}, err
		}
		return CardResult{Outcome: CardDropped, Receipt: receipt}, tx.Commit()
	}
	if err := o.ensureOwner(ctx, tx, now); err != nil {
		return CardResult{}, err
	}
	if err := o.upsertMeeting(ctx, tx, c.Title, c.Meeting); err != nil {
		return CardResult{}, err
	}
	res := CardResult{Receipt: receipt, Stored: true}
	var id, name, source string
	err = tx.QueryRowContext(ctx, `SELECT x.contact_id, c.name, c.name_source FROM contact_emails x JOIN contacts c ON c.owner = ? AND c.id = x.contact_id
		WHERE x.owner = ? AND x.address_key = ?`, o.subject, o.subject, c.AddressKey).Scan(&id, &name, &source)
	switch {
	case err == nil:
		res.Outcome = CardMerged
		said := ""
		if c.Name != name {
			said = c.Name
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provenance (contact_id, owner, kind, meeting_id, said_name, at) VALUES (?, ?, 'merged', ?, ?, ?)`,
			id, o.subject, c.Meeting.ID, said, ms(now)); err != nil {
			return CardResult{}, err
		}
		newName := name
		if source != "owner" { // 🔴 the owner's name is never overwritten
			newName = c.Name
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET name = ?, active_at = ?, updated_at = ? WHERE id = ? AND owner = ?`,
			newName, ms(now), ms(now), id, o.subject); err != nil {
			return CardResult{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		res.Outcome = CardCreated
		id = NewContactID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO contacts (id, owner, name, name_source, created_at, updated_at, active_at) VALUES (?, ?, ?, 'self', ?, ?, ?)`,
			id, o.subject, c.Name, ms(now), ms(now), ms(now)); err != nil {
			return CardResult{}, err
		}
		if err := o.addEmail(ctx, tx, id, email{c.Address, c.AddressKey}, "self", now); err != nil {
			return CardResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provenance (contact_id, owner, kind, meeting_id, at) VALUES (?, ?, 'self_submitted', ?, ?)`,
			id, o.subject, c.Meeting.ID, ms(now)); err != nil {
			return CardResult{}, err
		}
	default:
		return CardResult{}, err
	}
	res.ContactID = id
	if err := o.link(ctx, tx, id, "guest", "card", c.Meeting, c.Participant, now); err != nil {
		return CardResult{}, err
	}
	if err := o.storeReceipt(ctx, tx, receipt, id, now); err != nil {
		return CardResult{}, err
	}
	if err := o.remember(ctx, tx, c.Key, c.RequestHash, 202, id, now); err != nil {
		return CardResult{}, err
	}
	if err := audit(ctx, tx, now, o.subject, "card."+res.Outcome, ""); err != nil {
		return CardResult{}, err
	}
	return res, tx.Commit()
}

func (o *Owner) storeReceipt(ctx context.Context, tx *sql.Tx, receipt, contact string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO receipts (receipt_sha256, owner, contact_id, created_at) VALUES (?, ?, ?, ?)`, Hash(receipt), o.subject, contact, ms(now))
	return err
}

// CheckReceipt says whether a guest browser's receipt is still a contact of
// this owner, and which. Another owner's receipt is not known, as an unknown
// one is not.
func (o *Owner) CheckReceipt(ctx context.Context, receipt string) (string, bool, error) {
	st, err := o.s.StandingOf(ctx, o.subject)
	if err != nil {
		return "", false, err
	}
	if !st.Accepting {
		return "", false, ErrNotAccepting
	}
	var id string
	err = o.s.db.QueryRowContext(ctx, `SELECT contact_id FROM receipts WHERE receipt_sha256 = ? AND owner = ?`, Hash(receipt), o.subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Visit is a checked attendance (contactsapi.Attendance.Check).
type Visit struct {
	Key         string // contactsapi.KeyForAttendance
	RequestHash string
	ContactRef  string
	Via         string // receipt, invite
	Title       string
	Meeting     contactsapi.Meeting
	Participant contactsapi.Participant
}

// Attend records that a contact the owner has was in a meeting ("met at
// meeting X"). An unknown contact, or another owner's, is ErrNotFound.
func (o *Owner) Attend(ctx context.Context, v Visit, now time.Time) error {
	tx, err := o.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen, _, _, err := o.remembered(ctx, tx, v.Key, v.RequestHash)
	if err != nil || seen {
		return err
	}
	st, err := standingOf(ctx, tx, o.subject)
	if err != nil {
		return err
	}
	if !st.Accepting {
		return ErrNotAccepting
	}
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM contacts WHERE id = ? AND owner = ?`, v.ContactRef, o.subject).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := o.upsertMeeting(ctx, tx, v.Title, v.Meeting); err != nil {
		return err
	}
	role := "guest"
	if v.Via == "invite" {
		role = "invitee"
	}
	if err := o.link(ctx, tx, v.ContactRef, role, v.Via, v.Meeting, v.Participant, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contacts SET active_at = ? WHERE id = ? AND owner = ?`, ms(now), v.ContactRef, o.subject); err != nil {
		return err
	}
	if err := o.remember(ctx, tx, v.Key, v.RequestHash, 204, v.ContactRef, now); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, o.subject, "link.made", v.Via); err != nil {
		return err
	}
	return tx.Commit()
}
