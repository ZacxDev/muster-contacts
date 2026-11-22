package contactsapi

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The contract, as data: testdata/contract/routes.json (every route, each
// case a list of requests and the exact status and body each must answer)
// and testdata/contract/fields.json (the card's name and email rules, each
// input with its literal expected answer). Both plugins' tests read them:
// contacts runs RunContract against its real server, and meetings runs it
// against the same server binary in its integration job and checks its own
// card validation with Fields.
//
//go:embed testdata/contract/routes.json testdata/contract/fields.json
var contractFS embed.FS

// Contract is routes.json.
type Contract struct {
	Version int    `json:"version"`
	Cases   []Case `json:"cases"`
}

// Case is one scenario: the preconditions it needs (Given), then its
// requests in order.
type Case struct {
	Name  string   `json:"name"`
	Given []string `json:"given,omitempty"`
	Steps []Step   `json:"steps"`
}

// The preconditions a case may name, each for the case's {owner}.
const (
	GivenOwnerRedeemed    = "owner_redeemed"     // the owner opened Contacts; muster answers them live
	GivenOwnerRevoked     = "owner_revoked"      // the owner opened Contacts, then muster answered them revoked
	GivenOwnerExpired     = "owner_expired"      // the owner opened Contacts; muster answers them expired
	GivenCollectOff       = "collect_off"        // the owner turned "Add people from Meetings" off
	GivenCardLimitReached = "card_limit_reached" // the owner's daily cards are spent
)

// Step is one request and what it must answer.
//
// Strings in Path, Key and Body may hold placeholders: {owner} and {owner2}
// (two random subjects, fresh per case), {card} and {card2} (two random
// card ids), {t1} and {t2} (an hour ago and now, RFC 3339), and any name an
// earlier step captured. RawBody "$big" is a body over MaxBody. Auth is how
// the request authenticates: "" (the token), "none", "wrong" (another
// 43-character token), "prefix" (the token less its last character),
// "extended" (the token and one more), "scheme" (the token as Basic).
//
// Expect is the answer's JSON, compared exactly, except that a string
// "$receipt" or "$contact_ref" matches that value's shape and "$any"
// anything; a nil Expect is an empty body (204). Headers must be present
// on the answer ("$int" matches a positive integer). Capture names a
// top-level string of the answer to keep for later steps.
type Step struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Auth    string            `json:"auth,omitempty"`
	Key     string            `json:"key,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	RawBody string            `json:"raw_body,omitempty"`
	Status  int               `json:"status"`
	Expect  json.RawMessage   `json:"expect"`
	Headers map[string]string `json:"headers,omitempty"`
	Capture map[string]string `json:"capture,omitempty"`
}

// Fields is fields.json: the card's two fields' rules.
type Fields struct {
	Version int         `json:"version"`
	Names   []NameCase  `json:"names"`
	Emails  []EmailCase `json:"emails"`
}

// NameCase is one name: is it accepted, and as what.
type NameCase struct {
	Input string `json:"input"`
	OK    bool   `json:"ok"`
	Clean string `json:"clean,omitempty"`
}

// EmailCase is one address: is it accepted, and its address and key.
type EmailCase struct {
	Input   string `json:"input"`
	OK      bool   `json:"ok"`
	Address string `json:"address,omitempty"`
	Key     string `json:"key,omitempty"`
}

// LoadContract reads routes.json.
func LoadContract() (Contract, error) {
	var c Contract
	b, err := contractFS.ReadFile("testdata/contract/routes.json")
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// LoadFields reads fields.json.
func LoadFields() (Fields, error) {
	var f Fields
	b, err := contractFS.ReadFile("testdata/contract/fields.json")
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// ErrUnsupportedGiven is what a Target's Given answers for a precondition it
// cannot set up: the case is skipped, by name.
var ErrUnsupportedGiven = errors.New("this target cannot set up that precondition")

// Target is a running contacts internal API: its base URL (scheme and
// host), its token, and how to put an owner into a precondition.
type Target struct {
	URL    string
	Token  string
	Client *http.Client
	// Given sets up one precondition for owner, or answers
	// ErrUnsupportedGiven. Nil supports none.
	Given func(given, owner string) error
}

// RunContract runs every case of routes.json against target, each as a
// subtest named by the case, and answers how many ran (not skipped).
func RunContract(t *testing.T, target Target) int {
	t.Helper()
	c, err := LoadContract()
	if err != nil {
		t.Fatalf("routes.json: %v", err)
	}
	if target.Client == nil {
		target.Client = &http.Client{Timeout: 10 * time.Second}
	}
	ran := 0
	for _, cs := range c.Cases {
		t.Run(cs.Name, func(t *testing.T) {
			runCase(t, target, cs, &ran)
		})
	}
	return ran
}

// runCase runs one case; ran counts the cases whose preconditions were set
// up (a skipped case is not counted).
func runCase(t *testing.T, target Target, cs Case, ran *int) {
	now := time.Now().UTC().Truncate(time.Second)
	vars := map[string]string{
		"owner": randomB64(16), "owner2": randomB64(16),
		"card": randomB64(32), "card2": randomB64(32),
		"t1": now.Add(-time.Hour).Format(time.RFC3339), "t2": now.Format(time.RFC3339),
	}
	for _, g := range cs.Given {
		if target.Given == nil {
			t.Skipf("precondition %q: the target supports none", g)
		}
		if err := target.Given(g, vars["owner"]); errors.Is(err, ErrUnsupportedGiven) {
			t.Skipf("precondition %q is not supported by this target", g)
		} else if err != nil {
			t.Fatalf("precondition %q: %v", g, err)
		}
	}
	*ran++
	for i, st := range cs.Steps {
		where := fmt.Sprintf("step %d (%s %s)", i+1, st.Method, st.Path)
		var body io.Reader
		switch {
		case st.RawBody == "$big":
			body = strings.NewReader(`{"owner":"` + strings.Repeat("A", MaxBody) + `"}`)
		case st.RawBody != "":
			body = strings.NewReader(fill(st.RawBody, vars))
		case len(st.Body) > 0:
			body = strings.NewReader(fill(string(st.Body), vars))
		}
		req, err := http.NewRequest(st.Method, strings.TrimRight(target.URL, "/")+fill(st.Path, vars), body)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if st.Key != "" {
			req.Header.Set(IdempotencyHeader, fill(st.Key, vars))
		}
		switch st.Auth {
		case "":
			req.Header.Set("Authorization", "Bearer "+target.Token)
		case "none":
		case "wrong":
			req.Header.Set("Authorization", "Bearer "+randomB64(32))
		case "prefix":
			req.Header.Set("Authorization", "Bearer "+target.Token[:len(target.Token)-1])
		case "extended":
			req.Header.Set("Authorization", "Bearer "+target.Token+"A")
		case "scheme":
			req.Header.Set("Authorization", "Basic "+target.Token)
		default:
			t.Fatalf("%s: unknown auth %q", where, st.Auth)
		}
		resp, err := target.Client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != st.Status {
			t.Fatalf("%s: status %d, want %d: %s", where, resp.StatusCode, st.Status, raw)
		}
		for h, want := range st.Headers {
			got := resp.Header.Get(h)
			if want == "$int" {
				if !positiveInt.MatchString(got) {
					t.Fatalf("%s: header %s = %q, want a positive integer", where, h, got)
				}
			} else if got != fill(want, vars) {
				t.Fatalf("%s: header %s = %q, want %q", where, h, got, fill(want, vars))
			}
		}
		if len(st.Expect) == 0 || string(st.Expect) == "null" {
			if len(bytes.TrimSpace(raw)) != 0 {
				t.Fatalf("%s: answered a body %s, want none", where, raw)
			}
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: Content-Type %q, want application/json", where, ct)
		}
		var got, want any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: answer is not JSON: %s", where, raw)
		}
		if err := json.Unmarshal([]byte(fill(string(st.Expect), vars)), &want); err != nil {
			t.Fatalf("%s: the case's expect is not JSON: %v", where, err)
		}
		if msg := match(want, got, "$"); msg != "" {
			t.Fatalf("%s: answer %s: %s", where, raw, msg)
		}
		for name, field := range st.Capture {
			m, _ := got.(map[string]any)
			v, ok := m[field].(string)
			if !ok {
				t.Fatalf("%s: no string %q to capture in %s", where, field, raw)
			}
			vars[name] = v
		}
	}
}

var (
	positiveInt  = regexp.MustCompile(`^[1-9][0-9]*$`)
	placeholder  = regexp.MustCompile(`\{([a-z0-9_]+)\}`)
	shapeMatcher = map[string]*regexp.Regexp{
		"$receipt":     regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`),
		"$contact_ref": regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`),
		"$any":         regexp.MustCompile(``),
	}
)

// fill replaces every {name} in s with its value; an unknown name is left.
func fill(s string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// match compares an answer with the expected JSON, answering "" when they
// agree and what differs when not.
func match(want, got any, at string) string {
	switch w := want.(type) {
	case string:
		if re, ok := shapeMatcher[w]; ok {
			g, isStr := got.(string)
			if !isStr || !re.MatchString(g) {
				return fmt.Sprintf("%s = %v, want %s", at, got, w)
			}
			return ""
		}
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s is %T, want an object", at, got)
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				return fmt.Sprintf("%s.%s is in the answer and not in the contract", at, k)
			}
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Sprintf("%s.%s is missing", at, k)
			}
			if m := match(wv, gv, at+"."+k); m != "" {
				return m
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s = %v, want %v", at, got, w)
		}
		for i := range w {
			if m := match(w[i], g[i], fmt.Sprintf("%s[%d]", at, i)); m != "" {
				return m
			}
		}
		return ""
	}
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if !bytes.Equal(wb, gb) {
		return fmt.Sprintf("%s = %s, want %s", at, gb, wb)
	}
	return ""
}

func randomB64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
