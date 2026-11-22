package contactsapi

import (
	"fmt"
	"strings"
	"testing"
)

// The field table (testdata/contract/fields.json) is the card's rules, each
// input with its LITERAL expected answer: both plugins check against it.
func TestTheFieldTable(t *testing.T) {
	f, err := LoadFields()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Names) < 10 || len(f.Emails) < 25 {
		t.Fatalf("the field table holds %d names and %d emails: it did not load", len(f.Names), len(f.Emails))
	}
	okNames, okEmails := 0, 0
	for i, c := range f.Names {
		got, err := CleanName(c.Input)
		if (err == nil) != c.OK {
			t.Errorf("name #%d %q: accepted %v, want %v", i, c.Input, err == nil, c.OK)
			continue
		}
		if c.OK {
			okNames++
			if got != c.Clean {
				t.Errorf("name #%d %q: cleaned to %q, want %q", i, c.Input, got, c.Clean)
			}
		}
	}
	for i, c := range f.Emails {
		addr, key, err := CheckEmail(c.Input)
		if (err == nil) != c.OK {
			t.Errorf("email #%d %q: accepted %v, want %v", i, c.Input, err == nil, c.OK)
			continue
		}
		if c.OK {
			okEmails++
			if addr != c.Address || key != c.Key {
				t.Errorf("email #%d %q: (%q, %q), want (%q, %q)", i, c.Input, addr, key, c.Address, c.Key)
			}
		}
	}
	// Both directions are in the table: a rule that accepted or refused
	// everything would fail one half.
	if okNames == 0 || okNames == len(f.Names) || okEmails == 0 || okEmails == len(f.Emails) {
		t.Fatalf("the table is one-sided: %d of %d names and %d of %d emails accepted", okNames, len(f.Names), okEmails, len(f.Emails))
	}
}

// The merge key is the address's key alone: case in the local part and the
// domain is folded, IDNA is applied, plus-addressing and dots are NOT.
func TestTheEmailKey(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"Grace@Example.TEST", "grace@example.test"},
		{"ada@BÜCHER.example", "ada@xn--bcher-kva.example"},
	} {
		ka, err1 := EmailKey(c.a)
		kb, err2 := EmailKey(c.b)
		if err1 != nil || err2 != nil || ka != kb {
			t.Errorf("%q and %q: keys %q, %q (%v, %v), want one key", c.a, c.b, ka, kb, err1, err2)
		}
	}
	for _, c := range []struct{ a, b string }{
		{"ada+work@example.test", "ada@example.test"},
		{"a.da@example.test", "ada@example.test"},
	} {
		ka, _ := EmailKey(c.a)
		kb, _ := EmailKey(c.b)
		if ka == kb {
			t.Errorf("%q and %q: one key %q, want two (not folded)", c.a, c.b, ka)
		}
	}
}

// The contract file loads, every case has a name and steps, and names are
// unique (a subtest's name is how a failure is found).
func TestTheContractLoads(t *testing.T) {
	c, err := LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, cs := range c.Cases {
		if cs.Name == "" || len(cs.Steps) == 0 || seen[cs.Name] {
			t.Errorf("case %q: empty, stepless or duplicated", cs.Name)
		}
		seen[cs.Name] = true
		for _, st := range cs.Steps {
			if !strings.HasPrefix(st.Path, "/v1/") || st.Status == 0 {
				t.Errorf("case %q: a step %s %s answering %d", cs.Name, st.Method, st.Path, st.Status)
			}
		}
	}
	if len(c.Cases) < 50 {
		t.Fatalf("%d cases, want the whole table", len(c.Cases))
	}
}

// A Submission printed by accident shows no name or email.
func TestASubmissionPrintsNoPII(t *testing.T) {
	s := Submission{Name: "Zebulon Fixturename", Email: "zebulon.fixture@example.test", Meeting: Meeting{ID: "m1", Title: "Fixture title"}}
	for _, v := range []string{s.String(), strings.TrimSpace(sprint(s))} {
		if strings.Contains(v, "Zebulon") || strings.Contains(v, "zebulon.fixture") || strings.Contains(v, "Fixture title") {
			t.Fatalf("a submission printed %q", v)
		}
	}
}

func sprint(v any) string { return fmt.Sprintf("%v %+v", v, v) }
