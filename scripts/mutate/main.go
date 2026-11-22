// Command mutate is the guard mutation sweep (muster's docs/design/contacts.md:
// "Mutation rows for every guard"). It is muster-meetings' runner with this
// plugin's mutants. Each mutant is one exact text
// replacement in one file; it is applied, the ONE test named to kill it is
// run, and the file is put back byte for byte. A mutant counts as killed only
// when that test fails AND its output carries the mutant's expected message,
// so a mutant that breaks the build, panics elsewhere, or trips a different
// assertion is reported, not counted.
//
// Two controls run in every sweep: a known-caught mutant (it must be killed)
// and a no-op edit (it must survive), so a sweep that cannot tell the two
// apart fails as a whole.
//
//	go run ./scripts/mutate             # every mutant
//	go run ./scripts/mutate -list       # names only
//	go run ./scripts/mutate -match re   # the mutants whose name matches re, and the two controls
//	go run ./scripts/mutate -e2e        # the UI mutants instead, each killed by a Playwright test
//
// A UI mutant (e2e set) is killed by one Playwright test in e2e/ (the
// chromium project, -g its title), run with npx: the sweep needs the e2e
// dependencies installed (npm ci in e2e/) and Chromium. It has its own two
// controls. -e2e runs only those; the default sweep runs none of them.
//
// It exits non-zero unless every security mutant is killed and both controls
// behave. Run it from the repository root. It refuses a dirty tree (`git
// status --porcelain` not empty), so a mutant can never be left behind
// mixed into uncommitted work, and on SIGINT or SIGTERM it puts the file it
// is mutating back before it exits.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// live is the files a mutant is applied to right now, and their original
// bytes: what a signal must put back.
var live struct {
	sync.Mutex
	orig map[string][]byte
}

func setLive(orig map[string][]byte) {
	live.Lock()
	live.orig = orig
	live.Unlock()
}

// restoreOnSignal puts the mutated file back on SIGINT or SIGTERM, then exits.
func restoreOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-ch
		live.Lock() // held to exit: no mutant is applied after this
		for f, b := range live.orig {
			if err := os.WriteFile(f, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "mutate: %v: COULD NOT RESTORE %s: %v\n", sig, f, err)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "mutate: %v: restored %s\n", sig, f)
		}
		os.Exit(130)
	}()
}

type mutant struct {
	name string
	file string
	from string
	to   string
	pkg  string
	test string // the killing test's name
	want string // a substring of the killing failure's message
	// survive marks the no-op control: it must NOT be killed.
	survive bool
	// more are further replacements applied with this one, for a mutant
	// that needs a few lines in several files (in order; each from must
	// occur exactly once when it is applied).
	more []edit
	// e2e, for a UI mutant, is the Playwright spec (in e2e/) whose test
	// titled test (a -g pattern) must fail with want in its output.
	e2e string
}

type edit struct{ file, from, to string }

func (m mutant) edits() []edit { return append([]edit{{m.file, m.from, m.to}}, m.more...) }

const (
	capi    = "contactsapi/contactsapi.go"
	st      = "internal/store/store.go"
	ct      = "internal/store/contacts.go"
	sapi    = "internal/store/api.go"
	srv     = "internal/server/server.go"
	api     = "internal/server/api.go"
	assets  = "internal/server/assets.go"
	own     = "internal/owners/owners.go"
	mainGo  = "main.go"
	netpol  = "deploy/kubernetes/networkpolicy.yaml"
	appJS   = "internal/server/web/static/app.js"
	appCSS  = "internal/server/web/static/app.css"
	e2eSpec = "contacts.spec.ts"
)

var mutants = []mutant{
	// ---- controls ----
	{name: "control: known-caught (a 41-character name)", file: capi, from: "const MaxNameRunes = 40", to: "const MaxNameRunes = 41",
		pkg: "./contactsapi", test: "TestTheFieldTable", want: "accepted true, want false"},
	{name: "control: a no-op edit (must survive)", file: capi, from: "const MaxNameRunes = 40", to: "const MaxNameRunes = 40 // no-op",
		pkg: "./contactsapi", test: "TestTheFieldTable", survive: true},

	// ---- owner scoping ----
	{name: "owner scoping (door): any bearer opens the newest session", file: st,
		from: "FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,\n\t\tHash(token), ms(now))",
		to:   "FROM owner_sessions WHERE (token_sha256 = ? OR 1) AND expires_at > ? ORDER BY created_at DESC LIMIT 1`,\n\t\tHash(token), ms(now))",
		pkg:  "./internal/server", test: "TestOwnerRoutesNeedALiveSession", want: "= 200, want 401"},
	{name: "owner scoping (store): the contact read drops owner = ?", file: ct,
		from: "FROM contacts WHERE id = ? AND owner = ?`, id, o.subject).\n\t\tScan(",
		to:   "FROM contacts WHERE id = ?`, id).\n\t\tScan(",
		pkg:  "./internal/store", test: "TestOwnerScoping", want: "Get of B's contact as A"},
	{name: "owner scoping (store): the same, through the API, byte for byte", file: ct,
		from: "FROM contacts WHERE id = ? AND owner = ?`, id, o.subject).\n\t\tScan(",
		to:   "FROM contacts WHERE id = ?`, id).\n\t\tScan(",
		pkg:  "./internal/server", test: "TestAnotherOwnersContactIsTheSame404ByteForByte", want: "owner scoping: route 0"},
	{name: "owner scoping (store): the SQL ledger sees the dropped filter", file: ct,
		from: "`DELETE FROM contacts WHERE id = ? AND owner = ?`, id, o.subject)\n\tif err != nil {",
		to:   "`DELETE FROM contacts WHERE id = ?`, id)\n\tif err != nil {",
		pkg:  "./internal/store", test: "TestEverySQLStatementIsOwnerScoped", want: "SQL ledger: 1 person-table references and 0 owner = ? filters"},
	{name: "session lifetime: an expired session still opens", file: st,
		from: "FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,\n\t\tHash(token), ms(now))",
		to:   "FROM owner_sessions WHERE token_sha256 = ? AND ? > 0`,\n\t\tHash(token), ms(now))",
		pkg:  "./internal/server", test: "TestOwnerRoutesNeedALiveSession", want: "a session at its lifetime"},

	// ---- merge ----
	{name: "merge key: match the raw address instead of its key", file: sapi,
		from: "WHERE x.owner = ? AND x.address_key = ?`, o.subject, o.subject, c.AddressKey)",
		to:   "WHERE x.owner = ? AND x.address = ?`, o.subject, o.subject, c.Address)",
		pkg:  "./internal/store", test: "TestMergeRules", want: "case_differs"},
	{name: "merge: no duplicate: drop PRIMARY KEY (owner, address_key)", file: st,
		from: "\t\tPRIMARY KEY (owner, address_key),                        -- 🔴 one contact per email per owner\n",
		to:   "",
		pkg:  "./internal/store", test: "TestOneContactPerEmailPerOwnerIsAConstraint", want: "want a UNIQUE/PRIMARY KEY refusal"},
	{name: "merge: name kept: a card overwrites the owner's name", file: sapi,
		from: `if source != "owner" {`, to: `if true {`,
		pkg: "./internal/store", test: "TestMergeRules", want: "the_owner's_name_is_kept"},

	// ---- uniform answers (merge rule 5) ----
	{name: "no oracle: a merge answers 200, a new contact 202", file: api,
		from: "\taccepted(w)\n}\n\n// accepted is",
		to:   "\tif res.Outcome == store.CardMerged {\n\t\tw.WriteHeader(200)\n\t\treturn\n\t}\n\taccepted(w)\n}\n\n// accepted is",
		more: []edit{{api, "\t_, err = o.Submit(", "\tres, err := o.Submit("}},
		pkg:  "./internal/server", test: "TestTheContract", want: "submission_answers_alike"},
	{name: "no oracle: a dropped card answers 404", file: api,
		from: "\taccepted(w)\n}\n\n// accepted is",
		to:   "\tif res.Outcome == store.CardDropped {\n\t\tapiErr(w, 404, contactsapi.ErrNotAccepting)\n\t\treturn\n\t}\n\taccepted(w)\n}\n\n// accepted is",
		more: []edit{{api, "\t_, err = o.Submit(", "\tres, err := o.Submit("}},
		pkg:  "./internal/server", test: "TestTheContract", want: "revoked_is_dropped"},
	{name: "no oracle: an uninstalled app's dropped card answers 404", file: api,
		from: "\t\taccepted(w)\n\t\treturn\n\t}\n\tadmit :=",
		to:   "\t\tapiErr(w, 404, contactsapi.ErrNotAccepting)\n\t\treturn\n\t}\n\tadmit :=",
		pkg:  "./internal/server", test: "TestMustersWordOnAPersonReachesTheAPI", want: "after uninstall, a card"},

	// ---- deletion ----
	{name: "deletion: drop ON DELETE CASCADE from meeting_links", file: st,
		from: "\t\tFOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner) ON DELETE CASCADE,\n\t\tFOREIGN KEY (owner, meeting_id)",
		to:   "\t\tFOREIGN KEY (contact_id, owner) REFERENCES contacts (id, owner),\n\t\tFOREIGN KEY (owner, meeting_id)",
		pkg:  "./internal/store", test: "TestDeletingAContactLeavesNothingInAnyTable", want: "FOREIGN KEY constraint failed"},
	{name: "deletion: keep the orphaned meeting row", file: ct,
		from: "\tif err := o.dropOrphanMeetings(ctx, tx); err != nil {\n\t\treturn err\n\t}\n",
		to:   "",
		pkg:  "./internal/store", test: "TestDeletingAContactLeavesNothingInAnyTable", want: "deletion: orphaned meeting"},
	{name: "deletion: secure_delete off", file: st, from: `q.Add("_pragma", "secure_delete(1)")`, to: `q.Add("_pragma", "secure_delete(0)")`,
		pkg: "./internal/store", test: "TestADeleteLeavesNoTraceInTheFile", want: "still holds"},
	{name: "deletion: no checkpoint after the delete", file: ct,
		from: "\treturn o.s.checkpoint(ctx)\n}\n\n// dropOrphanMeetings", to: "\treturn nil\n}\n\n// dropOrphanMeetings",
		pkg: "./internal/store", test: "TestADeleteLeavesNoTraceInTheFile", want: "still holds"},
	{name: "delete everything: the meetings rows are kept", file: ct, from: "\t\t`DELETE FROM meetings WHERE owner = ?`,\n", to: "",
		pkg: "./internal/store", test: "TestDeleteEverything", want: "delete everything: meetings still holds"},

	// ---- accepting ----
	{name: "accepting: revoked: accept writes for a revoked owner", file: st,
		from: "st.Accepting = !st.Revoked && st.Collect", to: "st.Accepting = st.Collect",
		pkg: "./internal/store", test: "TestAccepting", want: "accepting revoked"},
	{name: "accepting: revoked: muster's Hide does not reach the API", file: own,
		from: "\t\t\tdefault: // revoked, another state, or absent from the answer\n\t\t\t\trevoked = append(revoked, s)",
		to:   "\t\t\tdefault: // revoked, another state, or absent from the answer\n\t\t\t\tlive = append(live, s)",
		pkg:  "./internal/server", test: "TestMustersWordOnAPersonReachesTheAPI", want: "accepting revoked"},
	{name: "accepting: fail closed: an absent subject is kept", file: own,
		from: "\t\t\tdefault: // revoked, another state, or absent from the answer\n\t\t\t\trevoked = append(revoked, s)",
		to:   "\t\t\tdefault: // revoked, another state, or absent from the answer\n\t\t\t\tlive = append(live, s)",
		pkg:  "./internal/owners", test: "TestASubjectMissingFromTheAnswerIsRevoked", want: "still accepting"},
	{name: "accepting: switch: ignore Add people from Meetings off", file: st,
		from: "st.Accepting = !st.Revoked && st.Collect", to: "st.Accepting = !st.Revoked",
		pkg: "./internal/store", test: "TestAccepting", want: "accepting switch"},
	{name: "expired accepted: refuse writes for expired", file: own,
		from: "\t\t\tcase muster.StateExpired:\n\t\t\t\texpired = append(expired, s)", to: "\t\t\tcase muster.StateExpired:\n\t\t\t\trevoked = append(revoked, s)",
		pkg: "./internal/server", test: "TestMustersWordOnAPersonReachesTheAPI", want: "expired accepted"},
	{name: "decision 8: refuse a person who never opened Contacts", file: st,
		from: "return Standing{Collect: true, Accepting: true}, nil", to: "return Standing{Collect: true, Accepting: false}, nil",
		pkg: "./internal/store", test: "TestAccepting", want: "a person never seen"},
	{name: "uninstalled: the API still accepts", file: api,
		from: "if !st.Accepting || !s.app.Live() {", to: "if !st.Accepting {",
		pkg: "./internal/server", test: "TestMustersWordOnAPersonReachesTheAPI", want: "after uninstall"},

	// ---- the API's door ----
	{name: "API auth: accept any bearer", file: api,
		from: `if s.cfg.APIToken == "" || subtle.ConstantTimeCompare(got, want) != 1 {`, to: `if s.cfg.APIToken == "" || (len(got) < 0 && subtle.ConstantTimeCompare(got, want) != 1) {`,
		pkg: "./internal/server", test: "TestTheContract", want: "auth:_wrong_token_is_401"},
	{name: "API auth: compare a prefix", file: api,
		from: `if s.cfg.APIToken == "" || subtle.ConstantTimeCompare(got, want) != 1 {`, to: `if s.cfg.APIToken == "" || (!bytes.HasPrefix(got, want) && subtle.ConstantTimeCompare(got, want) != 1) {`,
		pkg: "./internal/server", test: "TestTheContract", want: "auth:_extended_token_is_401"},
	{name: "token check: the API token may be the muster token", file: mainGo,
		from: "case c.APIToken == c.Token || strings.HasPrefix(c.APIToken, muster.TokenPrefix):", to: "case false:",
		pkg: ".", test: "TestTheAPITokenIsCheckedAtBoot", want: "another muster token:"},
	{name: "token check: a short API token is accepted", file: mainGo,
		from: "case len(c.APIToken) < MinAPITokenBytes:", to: "case false:",
		pkg: ".", test: "TestTheAPITokenIsCheckedAtBoot", want: "short:"},

	// ---- limits, keys, validation ----
	{name: "rate limit: no per-owner cap", file: api, from: "if len(mine) >= l.perOwner {", to: "if false {",
		pkg: "./internal/server", test: "TestCardRateLimits", want: "rate limit per owner"},
	{name: "rate limit: no per-install cap", file: api, from: "if len(l.all) >= l.perInstall {", to: "if false {",
		pkg: "./internal/server", test: "TestCardRateLimits", want: "rate limit per install"},
	{name: "idempotency: a key reused with another body is a replay", file: sapi,
		from: "\tif h != hash {\n\t\treturn false, 0, contact, ErrKeyReused", to: "\tif false {\n\t\treturn false, 0, contact, ErrKeyReused",
		pkg: "./internal/store", test: "TestIdempotency", want: "want ErrKeyReused"},
	{name: "validation: two addresses separated by a comma", file: capi,
		from: "const localForbidden = `<>()[]\\,;:\"@`", to: "const localForbidden = `<>()[]\\;:\"@`",
		pkg: "./contactsapi", test: "TestTheFieldTable", want: "grace,ada@example.test"},

	// ---- 🔴 no PII in logs ----
	{name: "logs: the card's body is logged", file: api,
		from: "\tname, addr, key, title, err := in.Check()\n", to: "\ts.cfg.Log.Printf(\"api: card from %s <%s>\", in.Name, in.Email)\n\tname, addr, key, title, err := in.Check()\n",
		pkg: "./internal/server", test: "TestNoPIIReachesTheLog", want: "no PII in logs"},
	{name: "logs: the access log records the query", file: srv,
		from: `s.cfg.Log.Printf("http %s %s %d %s", r.Method, routeOf(r),`, to: `s.cfg.Log.Printf("http %s %s %d %s", r.Method, r.URL.RequestURI(),`,
		pkg: "./internal/server", test: "TestNoPIIReachesTheLog", want: "no PII in logs"},

	// ---- /enter ----
	{name: "frame gate: removed", file: srv, from: "if ok, why := framed(r); !ok {", to: "if ok, why := framed(r); false && !ok {",
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "Sec-Fetch-Dest document: enter = 200"},
	{name: "redeem: accept any purpose", file: srv, from: `if red.Purpose != "app" {`, to: `if false {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "purpose view: enter = 200"},
	{name: "redeem: pass another person's ref to the page", file: srv,
		from: "if _, err := o.Get(r.Context(), red.Target.App.Ref); err == nil {", to: "if _, err := o.Get(r.Context(), red.Target.App.Ref); err == nil || true {",
		pkg: "./internal/server", test: "TestEnterHeadersAndRef", want: "another person's ref must not"},

	// ---- versioned assets ----
	{name: "versioned assets: a page names /static/ without ?v=", file: assets,
		from: `return "/static/" + name + "?v=" + v, nil`, to: `return "/static/" + name, nil`,
		pkg: "./internal/server", test: "TestEveryAssetAPageReferencesCarriesItsVersion", want: "is not versioned"},
	{name: "versioned assets: /static/ is cacheable", file: assets,
		from: "\t\th.Set(\"Cache-Control\", \"no-cache\")\n\t\tif v := staticVersion(r.URL.Path)", to: "\t\th.Set(\"Cache-Control\", \"max-age=14400\")\n\t\tif v := staticVersion(r.URL.Path)",
		pkg: "./internal/server", test: "TestPagesAreNeverCachedAndAssetsAreRevalidated", want: "want 200 no-cache"},
	{name: "versioned assets: no ETag", file: assets,
		from: "\t\t\th.Set(\"ETag\", `\"`+v+`\"`)\n", to: "",
		pkg: "./internal/server", test: "TestAStaticFileRevalidatesByItsVersion", want: "want 200 with ETag"},

	// ---- the deployment ----
	{name: "network policy: the API port is open to every pod", file: netpol,
		from: "    - from:\n        - podSelector:\n            matchLabels:\n              app.kubernetes.io/name: muster-meetings\n      ports:",
		to:   "    - ports:",
		pkg:  "./deploy", test: "TestKubernetesNetworkPolicy", want: "the API port :8081 admits"},

	// ---- the UI (e2e: each killed by a Playwright test) ----
	{name: "ui control: known-caught (the sheet has no title)", file: appJS,
		from: "$('sheet-title').textContent = title;", to: "$('sheet-title').textContent = '';",
		e2e: e2eSpec, test: "1280x800: list", want: "toHaveText"},
	{name: "ui control: a no-op edit (must survive)", file: appCSS,
		from: ".more { margin-top: .75rem; }", to: ".more { margin-top: .75rem; /* no-op */ }",
		e2e: e2eSpec, test: "reduced motion", survive: true},
	{name: "ui: search sends no query", file: appJS,
		from: "var path = '/api/contacts?q=' + encodeURIComponent(q.value.trim());", to: "var path = '/api/contacts?q=';",
		e2e: e2eSpec, test: "1280x800: list", want: "toHaveCount"},
	{name: "ui: Delete only hides the row", file: appJS,
		from: "api('DELETE', '/api/contacts/' + encodeURIComponent(c.id)).then(function () {", to: "Promise.resolve().then(function () {",
		e2e: e2eSpec, test: "390x844: list", want: "toHaveCount"},
	{name: "ui: Escape closes the sheet mid-edit", file: appJS,
		from: "if (e.key === 'Escape' && mode !== 'view') {", to: "if (false) {",
		e2e: e2eSpec, test: "Escape leaves the form", want: "toBeVisible"},
	{name: "ui: a row's motion ignores reduced motion", file: appCSS,
		from: ".contact { animation: rise var(--mu-dur-base) var(--mu-ease-enter);", to: ".contact { animation: rise 200ms var(--mu-ease-enter);",
		e2e: e2eSpec, test: "reduced motion", want: "a row under reduced motion"},
}

func main() {
	list := flag.Bool("list", false, "print the mutants' names and stop")
	e2eOnly := flag.Bool("e2e", false, "run the UI mutants (killed by Playwright tests) and their controls, instead of the Go ones")
	match := flag.String("match", "", "run only the mutants whose name matches this regular expression (the controls always run)")
	flag.Parse()
	if *match != "" {
		re, err := regexp.Compile(*match)
		if err != nil {
			fatalf("-match: %v", err)
		}
		var keep []mutant
		for _, m := range mutants {
			if strings.HasPrefix(m.name, "control: ") || re.MatchString(m.name) {
				keep = append(keep, m)
			}
		}
		mutants = keep
	}
	var kind []mutant
	for _, m := range mutants {
		if (m.e2e != "") == *e2eOnly {
			kind = append(kind, m)
		}
	}
	mutants = kind
	if *list {
		for _, m := range mutants {
			fmt.Println(m.name)
		}
		return
	}
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		fatalf("git status: %v (run it from the repository root)", err)
	}
	if len(bytes.TrimSpace(out)) > 0 {
		fatalf("the tree is dirty; commit or set your work aside first (a mutant must never mix with uncommitted work):\n%s", out)
	}
	restoreOnSignal()
	sums := map[string][32]byte{}
	for _, m := range mutants {
		for _, e := range m.edits() {
			b, err := os.ReadFile(e.file)
			if err != nil {
				fatalf("%s: %v", e.file, err)
			}
			sums[e.file] = sha256.Sum256(b)
		}
	}
	failed := 0
	fmt.Printf("%-58s %-9s %s\n", "mutant", "result", "killing test: message")
	for _, m := range mutants {
		res, detail := run(m)
		ok := (res == "KILLED" && !m.survive) || (res == "SURVIVED" && m.survive)
		if !ok {
			failed++
		}
		mark := ""
		if !ok {
			mark = "  <-- FAIL"
		}
		fmt.Printf("%-58s %-9s %s: %s%s\n", m.name, res, m.test, detail, mark)
	}
	for f, want := range sums {
		b, _ := os.ReadFile(f)
		if sha256.Sum256(b) != want {
			fatalf("%s was not restored byte for byte", f)
		}
	}
	if failed > 0 {
		fatalf("%d of %d mutants did not behave", failed, len(mutants))
	}
	fmt.Printf("all %d mutants behaved (every security mutant killed, the no-op survived)\n", len(mutants))
}

// run applies one mutant, runs its killing test, and restores the file.
func run(m mutant) (string, string) {
	orig := map[string][]byte{}
	cur := map[string][]byte{}
	for _, e := range m.edits() {
		if _, ok := orig[e.file]; !ok {
			b, err := os.ReadFile(e.file)
			if err != nil {
				return "ERROR", err.Error()
			}
			orig[e.file], cur[e.file] = b, b
		}
		if n := bytes.Count(cur[e.file], []byte(e.from)); n != 1 {
			return "ERROR", fmt.Sprintf("the text to replace occurs %d times in %s, want exactly 1", n, e.file)
		}
		cur[e.file] = bytes.Replace(cur[e.file], []byte(e.from), []byte(e.to), 1)
	}
	setLive(orig)
	defer func() {
		live.Lock()
		defer live.Unlock()
		for f, b := range orig {
			if err := os.WriteFile(f, b, 0o644); err != nil {
				fatalf("restoring %s: %v", f, err)
			}
		}
		live.orig = nil
	}()
	for f, b := range cur {
		if err := os.WriteFile(f, b, 0o644); err != nil {
			return "ERROR", err.Error()
		}
	}
	if m.e2e != "" {
		return runE2E(m)
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "^"+m.test+"$", m.pkg)
	if m.pkg == "./deploy" {
		// The deployment tests render with kubectl: fail, never skip.
		cmd.Env = append(os.Environ(), "CONTACTS_REQUIRE_KUBECTL=1")
	}
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err == nil {
		// The positive check that the test ran at all: "ok" for the package
		// and no "no tests to run".
		if strings.Contains(s, "no tests to run") {
			return "ERROR", "the -run filter matched no test"
		}
		return "SURVIVED", "test passed"
	}
	if !strings.Contains(s, "--- FAIL: "+m.test) {
		first := firstLine(s)
		return "ERROR", "failed without the named test failing (a build error?): " + first
	}
	if m.want != "" && !strings.Contains(s, m.want) {
		return "ERROR", fmt.Sprintf("the test failed, but not with %q: %s", m.want, failLine(s))
	}
	return "KILLED", failLine(s)
}

// runE2E runs a UI mutant's one Playwright test. Killed: it failed, exactly
// one test failed, and the output carries want. Survived: exactly one test
// ran and passed (a -g that matched nothing is an error, not a pass).
func runE2E(m mutant) (string, string) {
	cmd := exec.Command("npx", "playwright", "test", m.e2e, "--project=chromium", "--reporter=line", "--retries=0", "-g", m.test)
	cmd.Dir = "e2e"
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err == nil {
		if !regexp.MustCompile(`\b1 passed\b`).MatchString(s) {
			return "ERROR", "the -g filter did not run exactly one test: " + lastLine(s)
		}
		return "SURVIVED", "test passed"
	}
	if !regexp.MustCompile(`\b1 failed\b`).MatchString(s) {
		return "ERROR", "failed without exactly one test failing (a broken page?): " + lastLine(s)
	}
	// want is looked for in the failure itself (from "Error:" to the code
	// excerpt), never in the excerpt's neighbouring lines: a want that only
	// matched the source around a different failing assertion would be a
	// kill for the wrong reason.
	if m.want != "" && !strings.Contains(e2eFailure(s), m.want) {
		return "ERROR", fmt.Sprintf("the test failed, but not with %q: %s", m.want, e2eFailLine(s))
	}
	return "KILLED", e2eFailLine(s)
}

// e2eFailure is a Playwright failure's report: from its first "Error:" line
// up to the code excerpt (the first "  NN |" line).
func e2eFailure(s string) string {
	i := strings.Index(s, "Error:")
	if i < 0 {
		return ""
	}
	s = s[i:]
	if loc := regexp.MustCompile(`(?m)^\s*>?\s*\d+ \|`).FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return s
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

// e2eFailLine is the first "Error:" line of a Playwright failure and the
// locator or expectation under it.
func e2eFailLine(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), "Error:") {
			out := strings.TrimSpace(l)
			for _, n := range ls[i+1 : min(i+6, len(ls))] {
				if t := strings.TrimSpace(n); strings.HasPrefix(t, "Locator:") || strings.HasPrefix(t, "Expected") {
					out += " | " + t
					break
				}
			}
			if len(out) > 200 {
				out = out[:200] + "…"
			}
			return out
		}
	}
	return lastLine(s)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// failLine is the first assertion line of the failure output.
func failLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "_test.go:") {
			if len(l) > 160 {
				l = l[:160] + "…"
			}
			return l
		}
	}
	return firstLine(s)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mutate: "+f+"\n", a...)
	os.Exit(1)
}
