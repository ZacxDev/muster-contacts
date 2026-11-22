package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
)

var ctx = context.Background()

// t0 is the tests' clock: a fixed instant, never a written date.
var t0 = time.Unix(1_900_000_000, 0).UTC()

// The two owners' fixtures are pairwise distinct, and none equals a
// constant an assertion names.
const (
	ownerA = "AAAAownerAAAAAAAAAAAAA"
	ownerB = "BBBBownerBBBBBBBBBBBBB"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func view(t *testing.T, s *Store, subject string) *Owner {
	t.Helper()
	o, err := s.ForOwner(subject)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func tp(t time.Time) *time.Time { return &t }

// card builds a checked card for meeting m, participant p.
func card(t *testing.T, key, name, addr, meeting, part string) Card {
	t.Helper()
	a, k, err := contactsapi.CheckEmail(addr)
	if err != nil {
		t.Fatalf("fixture email %q: %v", addr, err)
	}
	return Card{Key: contactsapi.KeyForCard(key), RequestHash: "h-" + key, Name: name, Address: a, AddressKey: k, Title: "Title of " + meeting,
		Meeting:     contactsapi.Meeting{ID: meeting, Title: "Title of " + meeting, StartedAt: tp(t0), EndedAt: tp(t0.Add(time.Hour))},
		Participant: contactsapi.Participant{ID: part, Identity: "guest-" + part, TypedName: name, JoinedAt: tp(t0), LeftAt: tp(t0.Add(time.Hour))}}
}

func admitAll() error { return nil }

func submit(t *testing.T, o *Owner, c Card) CardResult {
	t.Helper()
	res, err := o.Submit(ctx, c, admitAll, t0)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return res
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// ---- merge rules (design: "Merge rules"), one row per rule ----

func TestMergeRules(t *testing.T) {
	type step struct {
		key, name, addr, meeting, part string
		want                           string // the outcome
	}
	cases := []struct {
		name       string
		ownerNamed string // an owner-entered contact holding the first card's email, before the cards
		steps      []step
		contacts   int
		finalName  string
		saidName   string // the last merged provenance line's said_name
		links      int
	}{
		{name: "an unknown email makes a contact marked self-submitted",
			steps:    []step{{"k1", "Grace", "grace@example.test", "m1", "p1", CardCreated}},
			contacts: 1, finalName: "Grace", links: 1},
		{name: "second card, same email: one contact, two meetings",
			steps: []step{{"k1", "Grace", "grace@example.test", "m1", "p1", CardCreated},
				{"k2", "Grace", "grace@example.test", "m2", "p2", CardMerged}},
			contacts: 1, finalName: "Grace", links: 2},
		{name: "case differs: the key folds it",
			steps: []step{{"k1", "Grace", "grace@example.test", "m1", "p1", CardCreated},
				{"k2", "Grace", "GRACE@Example.TEST", "m2", "p2", CardMerged}},
			contacts: 1, finalName: "Grace", links: 2},
		{name: "IDNA: a Unicode domain and its ASCII form are one key",
			steps: []step{{"k1", "Ada", "ada@bücher.example", "m1", "p1", CardCreated},
				{"k2", "Ada", "ada@xn--bcher-kva.example", "m2", "p2", CardMerged}},
			contacts: 1, finalName: "Ada", links: 2},
		{name: "plus-addressing is not folded: two contacts",
			steps: []step{{"k1", "Ada", "ada@example.test", "m1", "p1", CardCreated},
				{"k2", "Ada", "ada+work@example.test", "m2", "p2", CardCreated}},
			contacts: 2, links: 2},
		{name: "the owner's name is kept; the card's goes in said_name",
			ownerNamed: "Grace Brewster Hopper",
			steps:      []step{{"k1", "Gracie", "grace@example.test", "m1", "p1", CardMerged}},
			contacts:   1, finalName: "Grace Brewster Hopper", saidName: "Gracie", links: 1},
		{name: "a name from a card takes a newer card's",
			steps: []step{{"k1", "Grace", "grace@example.test", "m1", "p1", CardCreated},
				{"k2", "Grace H.", "grace@example.test", "m2", "p2", CardMerged}},
			contacts: 1, finalName: "Grace H.", saidName: "Grace H.", links: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)
			o := view(t, s, ownerA)
			if c.ownerNamed != "" {
				if _, err := o.Create(ctx, ContactIn{Name: c.ownerNamed, Emails: []string{c.steps[0].addr}}, t0); err != nil {
					t.Fatal(err)
				}
			}
			for i, st := range c.steps {
				res := submit(t, o, card(t, st.key, st.name, st.addr, st.meeting, st.part))
				if res.Outcome != st.want {
					t.Fatalf("card %d: outcome %q, want %q", i+1, res.Outcome, st.want)
				}
			}
			if n := count(t, s, `SELECT count(*) FROM contacts WHERE owner = ?`, ownerA); n != c.contacts {
				t.Fatalf("%d contacts, want %d", n, c.contacts)
			}
			if n := count(t, s, `SELECT count(*) FROM meeting_links WHERE owner = ?`, ownerA); n != c.links {
				t.Fatalf("%d meeting links, want %d", n, c.links)
			}
			if c.finalName != "" {
				rows, _, err := o.List(ctx, "", nil, 10)
				if err != nil || len(rows) != 1 || rows[0].Name != c.finalName {
					t.Fatalf("the contact is %+v (%v), want one named %q", rows, err, c.finalName)
				}
				got, _ := o.Get(ctx, rows[0].ID)
				var said string
				for _, p := range got.Provenance {
					if p.Kind == "merged" {
						said = p.SaidName
					}
				}
				if said != c.saidName {
					t.Fatalf("said_name %q, want %q", said, c.saidName)
				}
				if c.ownerNamed == "" && got.NameSource != "self" {
					t.Fatalf("name_source %q, want self (a card's)", got.NameSource)
				}
				if c.ownerNamed == "" && (got.Provenance[0].Kind != "self_submitted" || got.Provenance[0].MeetingTitle != "Title of m1") {
					t.Fatalf("origin %+v, want self_submitted after Title of m1", got.Provenance[0])
				}
				for _, m := range got.Meetings {
					if !m.Unconfirmed || m.Via != "card" {
						t.Fatalf("a card's meeting line %+v, want via card, unconfirmed", m)
					}
				}
			}
		})
	}
}

// 🔴 One contact per email per owner is the table's constraint, not only a
// lookup: a second row for the same key is refused by SQLite itself.
func TestOneContactPerEmailPerOwnerIsAConstraint(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	a, _ := o.Create(ctx, ContactIn{Name: "One", Emails: []string{"one@example.test"}}, t0)
	b, _ := o.Create(ctx, ContactIn{Name: "Two"}, t0)
	_, err := s.db.Exec(`INSERT INTO contact_emails (contact_id, owner, address, address_key, source, created_at) VALUES (?, ?, 'one@example.test', 'one@example.test', 'owner', 0)`, b.ID, ownerA)
	if !isUnique(err) {
		t.Fatalf("a second row for %s's key: %v, want a UNIQUE/PRIMARY KEY refusal", a.ID, err)
	}
}

// The owner adding an email another contact holds is refused, naming that
// contact (and Merge is offered); nothing is half-written.
func TestOwnerAddingATakenEmailIsRefused(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	grace, err := o.Create(ctx, ContactIn{Name: "Grace", Emails: []string{"grace@example.test"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Create(ctx, ContactIn{Name: "Impostor", Emails: []string{"Grace@Example.test"}}, t0)
	var taken ErrEmailTaken
	if !errors.As(err, &taken) || taken.ContactID != grace.ID || taken.Name != "Grace" {
		t.Fatalf("Create with a held email = %v, want ErrEmailTaken naming %s", err, grace.ID)
	}
	if n := count(t, s, `SELECT count(*) FROM contacts WHERE owner = ?`, ownerA); n != 1 {
		t.Fatalf("%d contacts after the refusal, want 1 (the refused one rolled back)", n)
	}
	ada, _ := o.Create(ctx, ContactIn{Name: "Ada"}, t0)
	_, err = o.Update(ctx, ada.ID, ContactPatch{Emails: &[]string{"grace@example.test"}}, t0)
	if !errors.As(err, &taken) || taken.ContactID != grace.ID {
		t.Fatalf("Update adding a held email = %v, want ErrEmailTaken", err)
	}
}

// A manual merge moves every child table's rows to the contact kept, adds a
// merged line, and deletes the other: asserted by counting each table.
func TestAManualMergeMovesEveryChildTable(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	keep, _ := o.Create(ctx, ContactIn{Name: "Grace", Emails: []string{"grace@example.test"}}, t0)
	res := submit(t, o, card(t, "k1", "G. Hopper", "hopper@example.test", "m1", "p1"))
	from := res.ContactID
	tables := []string{"contact_emails", "provenance", "meeting_links", "idempotency"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = count(t, s, `SELECT count(*) FROM `+tb+` WHERE contact_id = ?`, from)
		if before[tb] == 0 {
			t.Fatalf("the fixture has no %s row for the contact merged away: the test could not see it move", tb)
		}
	}
	keptBefore := map[string]int{}
	for _, tb := range tables {
		keptBefore[tb] = count(t, s, `SELECT count(*) FROM `+tb+` WHERE contact_id = ?`, keep.ID)
	}
	got, err := o.Merge(ctx, keep.ID, from, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		want := keptBefore[tb] + before[tb]
		if tb == "provenance" {
			want++ // the merged line
		}
		if n := count(t, s, `SELECT count(*) FROM `+tb+` WHERE contact_id = ?`, keep.ID); n != want {
			t.Errorf("%s: %d rows on the kept contact, want %d", tb, n, want)
		}
		if n := count(t, s, `SELECT count(*) FROM `+tb+` WHERE contact_id = ?`, from); n != 0 {
			t.Errorf("%s: %d rows left on the merged contact", tb, n)
		}
	}
	if _, err := o.Get(ctx, from); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the merged contact still answers: %v", err)
	}
	if got.Name != "Grace" || len(got.Emails) != 2 {
		t.Fatalf("kept %+v, want Grace with both emails", got)
	}
}

// ---- deletion ----

// tableNames is every table in the schema, from sqlite_master.
func tableNames(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

// holds says some text column of some row of table holds needle.
func holds(t *testing.T, s *Store, table, needle string) bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		for _, v := range vals {
			if sv, ok := v.(string); ok && strings.Contains(sv, needle) {
				return true
			}
		}
	}
	return false
}

// 🔴 After a delete, no table holds the contact's id or its email; the meeting only it was linked to is gone, and a meeting
// another contact shares is kept. The table list is read from the schema:
// a new table must be classed in PersonTables or otherTables, or this fails.
func TestDeletingAContactLeavesNothingInAnyTable(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	gone := submit(t, o, card(t, "k1", "Grace", "grace.deleted@example.test", "mOnly", "p1"))
	submit(t, o, card(t, "k2", "Grace", "grace.deleted@example.test", "mShared", "p2"))
	other := submit(t, o, card(t, "k3", "Ada", "ada.kept@example.test", "mShared", "p3"))
	needles := []string{gone.ContactID, "grace.deleted@example.test", "mOnly"}
	known := append(slices.Clone(PersonTables), otherTables...)
	for _, tb := range tableNames(t, s) {
		if !slices.Contains(known, tb) {
			t.Fatalf("table %s is in the schema but in neither PersonTables nor otherTables: class it, so a delete is checked against it", tb)
		}
	}
	// The positive control: before the delete, every needle is found.
	for _, n := range needles {
		found := false
		for _, tb := range PersonTables {
			found = found || holds(t, s, tb, n)
		}
		if !found {
			t.Fatalf("before the delete, %q is in no table: the search cannot see it", n)
		}
	}
	if err := o.Delete(ctx, gone.ContactID, t0); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tableNames(t, s) {
		for _, n := range needles {
			if holds(t, s, tb, n) {
				t.Errorf("after the delete, table %s still holds %q", tb, n)
			}
		}
	}
	if n := count(t, s, `SELECT count(*) FROM meetings WHERE owner = ? AND meeting_id = 'mOnly'`, ownerA); n != 0 {
		t.Errorf("deletion: orphaned meeting: the meeting only the deleted contact was in is still there")
	}
	if n := count(t, s, `SELECT count(*) FROM meetings WHERE owner = ? AND meeting_id = 'mShared'`, ownerA); n != 1 {
		t.Errorf("the meeting another contact shares was deleted")
	}
	if _, err := o.Get(ctx, other.ContactID); err != nil {
		t.Errorf("the other contact: %v", err)
	}
}

// 🔴 secure_delete and the checkpoint: a raw read of the database file (and
// its WAL) holds the fixture email before the delete (the control) and not
// after.
func TestADeleteLeavesNoTraceInTheFile(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	const fixture = "zebulon.securedelete@example.test"
	res := submit(t, o, card(t, "k1", "Zebulon Quux", fixture, "m1", "p1"))
	if err := s.checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	raw := func() []byte {
		var all []byte
		for _, p := range []string{s.Path(), s.Path() + "-wal"} {
			b, _ := os.ReadFile(p)
			all = append(all, b...)
		}
		return all
	}
	if !bytes.Contains(raw(), []byte(fixture)) {
		t.Fatalf("before the delete the file does not hold %q: the read cannot see it", fixture)
	}
	if err := o.Delete(ctx, res.ContactID, t0); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw(), []byte(fixture)) {
		t.Fatalf("after the delete the database file still holds %q", fixture)
	}
}

// Delete everything removes every row of the owner's in every person table,
// and no other owner's.
func TestDeleteEverything(t *testing.T) {
	s := open(t)
	a, b := view(t, s, ownerA), view(t, s, ownerB)
	submit(t, a, card(t, "k1", "Grace", "grace@example.test", "mA1", "pA1"))
	a.Create(ctx, ContactIn{Name: "Typed", Emails: []string{"typed@example.test"}}, t0)
	submit(t, b, card(t, "k2", "Bea", "bea@example.test", "mB1", "pB1"))
	bBefore := map[string]int{}
	for _, tb := range PersonTables {
		bBefore[tb] = count(t, s, `SELECT count(*) FROM `+tb+` WHERE owner = ?`, ownerB)
	}
	if err := a.DeleteAll(ctx, t0); err != nil {
		t.Fatal(err)
	}
	for _, tb := range PersonTables {
		q := `SELECT count(*) FROM ` + tb + ` WHERE owner = ?`
		if tb == "idempotency" {
			// Only tombstones stay: a key, no body hash, no contact.
			q += ` AND (request_sha256 != '' OR contact_id IS NOT NULL)`
		}
		if n := count(t, s, q, ownerA); n != 0 {
			t.Errorf("delete everything: %s still holds %d of the owner's rows", tb, n)
		}
		if n := count(t, s, `SELECT count(*) FROM `+tb+` WHERE owner = ?`, ownerB); n != bBefore[tb] {
			t.Errorf("delete everything: %s: another owner's rows went from %d to %d", tb, bBefore[tb], n)
		}
	}
}

// ---- owner scoping ----

// 🔴 Owner A's view, handed owner B's ids, finds nothing and changes
// nothing: each answer is the same as for an unknown id.
func TestOwnerScoping(t *testing.T) {
	s := open(t)
	a, b := view(t, s, ownerA), view(t, s, ownerB)
	bRes := submit(t, b, card(t, "kB", "Beatrix", "beatrix@example.test", "mB", "pB"))
	bTyped, _ := b.Create(ctx, ContactIn{Name: "Bartholomew", Emails: []string{"bart@example.test"}}, t0)
	aMine, _ := a.Create(ctx, ContactIn{Name: "Alma"}, t0)
	const unknown = "UUUUunknownUUUUUUUUUUU"
	for _, id := range []string{bRes.ContactID, bTyped.ID} {
		if _, err := a.Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get of B's contact as A = %v, want ErrNotFound", err)
		}
		name := "stolen"
		if _, err := a.Update(ctx, id, ContactPatch{Name: &name}, t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("Update of B's contact as A = %v, want ErrNotFound", err)
		}
		if err := a.Delete(ctx, id, t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete of B's contact as A = %v, want ErrNotFound", err)
		}
		if _, err := a.Merge(ctx, aMine.ID, id, t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("Merge of B's contact into A's = %v, want ErrNotFound", err)
		}
	}
	if _, err := a.Get(ctx, unknown); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	rows, _, _ := a.List(ctx, "bart", nil, 10)
	if len(rows) != 0 {
		t.Errorf("A's search finds B's contacts: %+v", rows)
	}
	// B's data is untouched.
	got, err := b.Get(ctx, bTyped.ID)
	if err != nil || got.Name != "Bartholomew" {
		t.Fatalf("B's contact after A's attempts: %+v %v", got, err)
	}
	if n := count(t, s, `SELECT count(*) FROM meeting_links WHERE owner = ?`, ownerB); n != 1 {
		t.Fatalf("B's meeting links: %d, want 1", n)
	}
}

// A child row cannot hang under another owner's contact: the composite
// foreign key (contact_id, owner) refuses it.
func TestAChildRowCannotCrossOwners(t *testing.T) {
	s := open(t)
	b := view(t, s, ownerB)
	bc, _ := b.Create(ctx, ContactIn{Name: "Bea"}, t0)
	view(t, s, ownerA).SetCollect(ctx, true, t0)
	_, err := s.db.Exec(`INSERT INTO contact_emails (contact_id, owner, address, address_key, source, created_at) VALUES (?, ?, 'x@example.test', 'x@example.test', 'owner', 0)`, bc.ID, ownerA)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an email for B's contact under A: %v, want a FOREIGN KEY refusal", err)
	}
}

// ---- accepting (decision 8; design: "The owner's standing") ----

func TestAccepting(t *testing.T) {
	s := open(t)
	never := view(t, s, "NNNNneverNNNNNNNNNNNNN")
	st, _ := s.StandingOf(ctx, never.Subject())
	if !st.Accepting || st.Known {
		t.Fatalf("a person never seen: %+v, want accepting", st)
	}
	if res := submit(t, never, card(t, "k1", "Grace", "grace@example.test", "m1", "p1")); res.Outcome != CardCreated || res.ContactID == "" {
		t.Fatalf("a never-opened owner's card: %+v, want created (decision 8)", res)
	}
	st, _ = s.StandingOf(ctx, never.Subject())
	if !st.Known || st.Redeemed || !st.Accepting {
		t.Fatalf("after the card: %+v, want known, not redeemed, accepting", st)
	}

	// The switch off: refused, and nothing is written.
	off := view(t, s, ownerA)
	if err := off.SetCollect(ctx, false, t0); err != nil {
		t.Fatal(err)
	}
	if res := submit(t, off, card(t, "k2", "Grace", "grace@example.test", "m1", "p1")); res.Outcome != CardDropped || res.ContactID != "" {
		t.Fatalf("accepting switch: a card with Add people from Meetings off: %+v, want dropped", res)
	}
	if n := count(t, s, `SELECT count(*) FROM contacts WHERE owner = ?`, ownerA); n != 0 {
		t.Fatalf("a dropped card wrote %d contacts", n)
	}

	// Revoked: refused; expired: accepted.
	if _, err := s.CreateSession(ctx, ownerB, "B", t0); err != nil {
		t.Fatal(err)
	}
	rev := view(t, s, ownerB)
	if _, err := s.Judge(ctx, nil, nil, []string{ownerB}, t0.Add(time.Second), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if res := submit(t, rev, card(t, "k3", "Grace", "grace@example.test", "m1", "p1")); res.Outcome != CardDropped {
		t.Fatalf("accepting revoked: a revoked owner's card: %+v, want dropped", res)
	}
	if _, err := s.Judge(ctx, nil, []string{ownerB}, nil, t0.Add(2*time.Second), t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if res := submit(t, rev, card(t, "k4", "Grace", "grace@example.test", "m1", "p1")); res.Outcome != CardCreated {
		t.Fatalf("an expired owner's card: %+v, want created (expired accepts)", res)
	}
}

// Judge: revoked ends sessions and refuses; expired ends sessions only; an
// open after the check was sent is newer than the answer and is kept.
func TestJudge(t *testing.T) {
	s := open(t)
	tokA, _ := s.CreateSession(ctx, ownerA, "A", t0)
	tokB, _ := s.CreateSession(ctx, ownerB, "B", t0)
	asOf := t0.Add(time.Minute)
	if _, err := s.Judge(ctx, nil, []string{ownerB}, []string{ownerA}, asOf, asOf); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{tokA, tokB} {
		if _, err := s.LookupSession(ctx, tok, asOf); !errors.Is(err, ErrNotFound) {
			t.Errorf("a judged session still opens: %v", err)
		}
	}
	a, _ := s.StandingOf(ctx, ownerA)
	b, _ := s.StandingOf(ctx, ownerB)
	if a.Accepting || !a.Revoked || !b.Accepting {
		t.Fatalf("after revoked A and expired B: A %+v, B %+v", a, b)
	}
	// A reopens the tab after a later check was sent: that check's
	// "revoked" (from before) does not refuse them again.
	s.CreateSession(ctx, ownerA, "A", asOf.Add(2*time.Minute))
	if _, err := s.Judge(ctx, nil, nil, []string{ownerA}, asOf.Add(time.Minute), asOf.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.StandingOf(ctx, ownerA); !a.Accepting {
		t.Fatalf("an answer sent before the reopen refused the person: %+v", a)
	}
}

// ---- idempotency ----

func TestIdempotency(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	c := card(t, "k1", "Grace", "grace@example.test", "m1", "p1")
	first := submit(t, o, c)
	again := submit(t, o, c)
	if again.Outcome != CardReplayed || again.ContactID != first.ContactID {
		t.Fatalf("a replay: %+v, want replayed for the same contact", again)
	}
	for _, q := range []string{`SELECT count(*) FROM contacts WHERE owner = ?`, `SELECT count(*) FROM meeting_links WHERE owner = ?`, `SELECT count(*) FROM provenance WHERE owner = ?`} {
		if n := count(t, s, q, ownerA); n != 1 {
			t.Fatalf("%s = %d after a replay, want 1", q, n)
		}
	}
	other := c
	other.RequestHash = "another body"
	if _, err := o.Submit(ctx, other, admitAll, t0); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("the same key with another body: %v, want ErrKeyReused", err)
	}
	// A replay is not counted by the rate limit: admit is not asked.
	asked := false
	if _, err := o.Submit(ctx, c, func() error { asked = true; return errors.New("limited") }, t0); err != nil || asked {
		t.Fatalf("a replay asked the limit (%v) or failed: %v", asked, err)
	}
}

// ---- sessions ----

func TestSessions(t *testing.T) {
	s := open(t)
	for _, bad := range []string{"", "12345", "ada.lovelace@example.t", strings.Repeat("A", 23)} {
		if _, err := s.CreateSession(ctx, bad, "x", t0); !errors.Is(err, ErrBadSubject) {
			t.Errorf("CreateSession(%q) = %v, want ErrBadSubject", bad, err)
		}
	}
	tok, err := s.CreateSession(ctx, ownerA, "Ada", t0)
	if err != nil || !ValidSessionToken(tok) {
		t.Fatalf("CreateSession = %q, %v", tok, err)
	}
	if _, err := s.LookupSession(ctx, tok, t0.Add(SessionLifetime-time.Second)); err != nil {
		t.Fatalf("a live session: %v", err)
	}
	if _, err := s.LookupSession(ctx, tok, t0.Add(SessionLifetime)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session at its lifetime: %v, want ErrNotFound", err)
	}
	var stored string
	s.db.QueryRow(`SELECT token_sha256 FROM owner_sessions`).Scan(&stored)
	if stored == tok || stored != Hash(tok) {
		t.Fatalf("the session is stored as %q, want only its hash", stored)
	}
}

// The list: newest activity first, search by name or email, paged.
func TestListSearchAndPaging(t *testing.T) {
	s := open(t)
	o := view(t, s, ownerA)
	for i, n := range []string{"Alma Zed", "Bruno Yew", "Cleo Xu"} {
		if _, err := o.Create(ctx, ContactIn{Name: n, Emails: []string{strings.ToLower(strings.Fields(n)[0]) + "@example.test"}}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	rows, next, _ := o.List(ctx, "", nil, 2)
	if len(rows) != 2 || rows[0].Name != "Cleo Xu" || rows[1].Name != "Bruno Yew" || next == nil {
		t.Fatalf("page 1: %+v next %v", rows, next)
	}
	rows, next, _ = o.List(ctx, "", next, 2)
	if len(rows) != 1 || rows[0].Name != "Alma Zed" || next != nil {
		t.Fatalf("page 2: %+v next %v", rows, next)
	}
	for q, want := range map[string]string{"bruno": "Bruno Yew", "CLEO@": "Cleo Xu", "yew": "Bruno Yew"} {
		rows, _, _ := o.List(ctx, q, nil, 10)
		if len(rows) != 1 || rows[0].Name != want {
			t.Errorf("search %q: %+v, want %s", q, rows, want)
		}
	}
	if rows, _, _ := o.List(ctx, "%", nil, 10); len(rows) != 0 {
		t.Errorf("search %%: %d rows, want none (a literal, not a wildcard)", len(rows))
	}
}

var _ = sql.ErrNoRows

// 🔴 G2: a deleted contact's card key outlives the contact as a tombstone
// (the key alone: no body hash, no contact), so meetings' outbox retrying
// the same card within its 72 hours makes nothing again, with the same
// answer; after the 72 hours the sweep drops it.
func TestADeletedContactIsNotMadeAgainByARetry(t *testing.T) {
	for _, how := range []string{"delete", "delete everything"} {
		t.Run(how, func(t *testing.T) {
			s := open(t)
			o := view(t, s, ownerA)
			c := card(t, "kRetry", "Grace", "grace.retry@example.test", "m1", "p1")
			first := submit(t, o, c)
			if how == "delete" {
				if err := o.Delete(ctx, first.ContactID, t0); err != nil {
					t.Fatal(err)
				}
			} else if err := o.DeleteAll(ctx, t0); err != nil {
				t.Fatal(err)
			}
			var hash string
			var contact any
			if err := s.db.QueryRow(`SELECT request_sha256, contact_id FROM idempotency WHERE owner = ? AND key = ?`, ownerA, c.Key).Scan(&hash, &contact); err != nil {
				t.Fatalf("tombstone: the card's key went with the contact: %v", err)
			}
			if hash != "" || contact != nil {
				t.Fatalf("tombstone: the kept key holds hash %q, contact %v: want neither", hash, contact)
			}
			again := submit(t, o, c)
			other := c
			other.RequestHash = "a retry whose body differs"
			changed := submit(t, o, other)
			for _, r := range []CardResult{again, changed} {
				if r.Outcome != CardReplayed || r.ContactID != "" {
					t.Fatalf("tombstone: a retry after the delete: %+v, want a replay that makes nothing", r)
				}
			}
			if n := count(t, s, `SELECT count(*) FROM contacts WHERE owner = ?`, ownerA); n != 0 {
				t.Fatalf("tombstone: a retry after the delete made the contact again (%d contacts)", n)
			}
			// The sweep keeps it until the 72 hours pass, then drops it.
			if err := s.Sweep(ctx, t0.Add(TombstoneLifetime-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, `SELECT count(*) FROM idempotency WHERE owner = ?`, ownerA); n != 1 {
				t.Fatalf("the sweep dropped the tombstone early: %d rows", n)
			}
			if err := s.Sweep(ctx, t0.Add(TombstoneLifetime)); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, `SELECT count(*) FROM idempotency WHERE owner = ?`, ownerA); n != 0 {
				t.Fatalf("the sweep kept the tombstone past 72 hours: %d rows", n)
			}
		})
	}
}
