// Package mustertest is a fake muster for tests: the three /plugin/v1 routes
// the contacts plugin calls as a server-installed app (GET /plugin/v1/self,
// POST /plugin/v1/handoffs/redeem, POST /plugin/v1/owners/check), answered as
// muster core answers them, recording every request it was sent. It is
// muster-meetings' fake cut down to those routes, plus the linked pair
// (muster's `app install … --link-to`): Link makes two apps share one subject
// per person, as core does.
//
//   - A server app is installed once (Install) with one token. Uninstall
//     makes the token 401 on every route and deletes every grant and unspent
//     code.
//   - THE DOOR, in core's order: 401 for a dead or unknown token; a personal
//     token carrying Muster-Act-For is 400; for a server token, a route the
//     door does not serve is 403, then a capability the route needs and the
//     app lacks is 403, then a Muster-Act-For header on a route that takes
//     none is 400.
//   - A person's browser mints a code for the app (MintApp). It lives 60 s,
//     is spent once, redeems only with the token of the app it was minted
//     for. The redeem re-checks the person and upserts the grant: the subject
//     is kept, or (for an app in a pair) the partner's subject for that
//     person, or a new random 128-bit one.
//   - One predicate, grantState, decides live / expired / revoked;
//     owners/check answers EVERY asked subject, one that this app never
//     redeemed as revoked, 60 calls a minute per app (429 with Retry-After).
package mustertest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is the default server app's token (New installs it).
const Token = "mpl_test_server_token_0123456789"

// ActFor is the header a server token names its person with.
const ActFor = "Muster-Act-For"

// Core's numbers.
const (
	GrantLife            = 8 * time.Hour    // how long a redeem lets the app act (fork 3)
	CodeLife             = 60 * time.Second // a hand-off code's life
	OwnersCheckPerMinute = 60               // owners/check calls per app per minute
	MaxOwnersCheck       = 200              // subjects per owners/check
)

// The refusal bodies core writes.
const (
	ServerRouteRefusal = "this is a server app's token: it reads /self, redeems its own hand-offs, checks its people and knocks or files tasks for one of them, and nothing else"
	ActForPersonal     = "Muster-Act-For is a server app's header: a personal plugin token acts as its own owner and names no one"
	ActForShape        = "this route needs exactly one Muster-Act-For header naming a subject (and only an act-for route takes one)"
)

// App is one server-installed app: what GET /plugin/v1/self says of it.
type App struct {
	Token        string
	PluginID     int64
	PluginName   string
	Capabilities []string
}

// DefaultApp is the app New installs under Token.
var DefaultApp = App{Token: Token, PluginID: 3, PluginName: "example-meetings", Capabilities: []string{"app"}}

// Personal is a per-owner plugin token: the old model, kept so a test can
// show the plugin refuses it.
type Personal struct {
	Token        string
	PluginID     int64
	OwnerID      int64
	OwnerLogin   string
	Capabilities []string
}

// Person is a muster account. ID and Email are what the plugin must never
// learn: the fake never sends either in any answer.
type Person struct {
	ID          int64
	Email       string
	DisplayName string
}

type person struct {
	Person
	kind      string // "person", or "team"
	disabled  bool
	signedIn  time.Time // the session the person's browser mints with
	signedOut time.Time // the last Sign out everywhere; zero: none
	notBefore time.Time // sessions_not_before; zero: none
}

type grant struct {
	subject  string
	signinAt *time.Time // nil: a row only Hide made
	actUntil time.Time
	hidden   bool
}

type app struct {
	App
	revoked bool
	scope   string // "server"; anything else revokes every grant
	retired bool   // its origin left MUSTER_PLUGIN_ORIGINS
	grants  map[int64]*grant
	// owners/check's window.
	checks []time.Time
	// partner is the other app of the linked pair this app is in, or nil.
	partner *app
}

type code struct {
	pluginID int64
	userID   int64
	answer   string
	signedIn time.Time
	target   map[string]any
	mintedAt time.Time
	spent    bool
}

// Fake is a fake muster server.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Now is the fake's clock; default time.Now.
	Now func() time.Time
	// Off makes every route answer 404, as muster does with plugins off.
	Off bool
	// Failing makes every route answer 503.
	Failing bool
	// FailOwnersCheck makes owners/check alone answer 503.
	FailOwnersCheck bool
	// FailOwnersCheckCall makes the nth owners/check call (1-based, counted
	// from New) answer 503, and only it.
	FailOwnersCheckCall int
	// OwnersCheckBody, when set, is owners/check's 200 answer, verbatim.
	OwnersCheckBody string
	// OnOwnersCheck, when set, runs after owners/check's answer is decided
	// and before it is written, with the fake unlocked.
	OnOwnersCheck func()
	// OwnersCheckLimit is owners/check's calls per app per minute (core:
	// OwnersCheckPerMinute, New's default). A test lowers it to reach the
	// limit without thousands of subjects.
	OwnersCheckLimit int
	ownersChecks     int
	checked          map[string]int
	apps             map[string]*app
	byID             map[int64]*app
	personal         map[string]*Personal
	people           map[int64]*person
	codes            map[string]*code
	nextPerson       int64
	calls            []string
}

// New starts a fake muster with DefaultApp installed; Close it when done.
func New() *Fake {
	f := &Fake{apps: map[string]*app{}, byID: map[int64]*app{}, personal: map[string]*Personal{}, people: map[int64]*person{},
		codes: map[string]*code{}, checked: map[string]int{}, Now: time.Now, nextPerson: 7_000_000_000,
		OwnersCheckLimit: OwnersCheckPerMinute}
	f.Install(DefaultApp)
	mux := http.NewServeMux()
	served := map[string]struct {
		need string // "" any; "app"
		h    func(http.ResponseWriter, *http.Request, caller, int64)
	}{
		"GET /plugin/v1/self":             {"", f.self},
		"POST /plugin/v1/handoffs/redeem": {"app", f.redeem},
		"POST /plugin/v1/owners/check":    {"", f.ownersCheck},
	}
	for pattern, r := range served {
		mux.HandleFunc(pattern, f.gate(true, r.need, r.h))
	}
	// Routes core serves personal tokens only: a server token is 403.
	for _, pattern := range []string{"POST /plugin/v1/messages", "GET /plugin/v1/tasks", "POST /plugin/v1/drafts/claim",
		"PUT /plugin/v1/chief/instructions", "POST /plugin/v1/task-writes/claim"} {
		mux.HandleFunc(pattern, f.gate(false, "", nil))
	}
	f.Server = httptest.NewServer(mux)
	return f
}

func newApp(a App) *app {
	x := &app{App: a, scope: "server", grants: map[int64]*grant{}}
	x.Capabilities = slices.Clone(a.Capabilities)
	return x
}

// Install installs a server app.
func (f *Fake) Install(a App) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := newApp(a)
	f.apps[a.Token] = x
	f.byID[a.PluginID] = x
}

// Uninstall revokes the app: its token is 401 everywhere, and its grants,
// unspent codes are deleted, and its pair (if any) ends.
func (f *Fake) Uninstall(pluginID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.byID[pluginID]
	a.revoked = true
	a.grants = map[int64]*grant{}
	if a.partner != nil {
		a.partner.partner = nil
		a.partner = nil
	}
	for k, c := range f.codes {
		if c.pluginID == pluginID && !c.spent {
			delete(f.codes, k)
		}
	}
}

// Rotate gives the app a new token: the old one is 401 at once; subjects
// and grants stay.
func (f *Fake) Rotate(pluginID int64, newToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.byID[pluginID]
	f.apps[old.Token] = &app{App: old.App, revoked: true}
	old.Token = newToken
	f.apps[newToken] = old
}

// RetireOrigin takes the app's origin out of MUSTER_PLUGIN_ORIGINS: every
// grant is revoked (the token still authenticates).
func (f *Fake) RetireOrigin(pluginID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].retired = true
}

// SetScope changes the app row's scope: anything but "server" revokes every
// grant.
func (f *Fake) SetScope(pluginID int64, scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].scope = scope
}

// AddPersonal registers a per-owner token (the old model).
func (f *Fake) AddPersonal(p Personal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.personal[p.Token] = &cp
}

// AddPerson adds an account, signed in now, with one device.
func (f *Fake) AddPerson(p Person) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[p.ID] = &person{Person: p, kind: "person", signedIn: f.Now()}
}

// AddGrants adds n people, each with a live grant under the app (as if
// each had just opened the tab), and returns their subjects.
func (f *Fake) AddGrants(pluginID int64, n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	a := f.byID[pluginID]
	out := make([]string, n)
	for i := range out {
		f.nextPerson++
		f.people[f.nextPerson] = &person{Person: Person{ID: f.nextPerson}, kind: "person", signedIn: now}
		in := now
		g := &grant{subject: newSubject(), signinAt: &in, actUntil: now.Add(GrantLife)}
		a.grants[f.nextPerson] = g
		out[i] = g.subject
	}
	return out
}

func (f *Fake) withPerson(userID int64, fn func(p *person)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.people[userID])
}

// SignIn is a new browser session for the person, at now.
func (f *Fake) SignIn(userID int64) { f.withPerson(userID, func(p *person) { p.signedIn = f.Now() }) }

// SignOutEverywhere ends every session the person has, at now.
func (f *Fake) SignOutEverywhere(userID int64) {
	f.withPerson(userID, func(p *person) { p.signedOut = f.Now() })
}

// SetSessionsNotBefore moves the person's session cutoff to now (core's
// sessions_not_before).
func (f *Fake) SetSessionsNotBefore(userID int64) {
	f.withPerson(userID, func(p *person) { p.notBefore = f.Now() })
}

// Disable disables the account.
func (f *Fake) Disable(userID int64) { f.withPerson(userID, func(p *person) { p.disabled = true }) }

// MakeTeam makes the account a team's: not a person.
func (f *Fake) MakeTeam(userID int64) { f.withPerson(userID, func(p *person) { p.kind = "team" }) }

// Hide is the person's Hide on Account → Apps: hidden_at and act_until are
// now. A person who never opened the app gets a row that can never act.
func (f *Fake) Hide(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.byID[pluginID]
	g := a.grants[userID]
	if g == nil {
		g = &grant{subject: f.subjectFor(a, userID)}
		a.grants[userID] = g
	}
	g.hidden, g.actUntil = true, f.Now()
}

// Show clears hidden_at only: the app may not act again until a redeem.
func (f *Fake) Show(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.byID[pluginID].grants[userID]; g != nil {
		g.hidden = false
	}
}

// Expire ends the person's grant by its clock alone (act_until = now).
func (f *Fake) Expire(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].grants[userID].actUntil = f.Now()
}

// Subject is the person's subject under the app, "" when there is none.
func (f *Fake) Subject(pluginID, userID int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.byID[pluginID].grants[userID]; g != nil {
		return g.subject
	}
	return ""
}

// MintApp mints a code for the app, as the person's browser does on
// muster's /apps/{id} page. ref "" is {"app": {}}.
func (f *Fake) MintApp(pluginID, userID int64, ref string) string {
	target := map[string]any{"app": map[string]any{}}
	if ref != "" {
		target["app"] = map[string]any{"ref": ref}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCode(&code{pluginID: pluginID, userID: userID, signedIn: f.people[userID].signedIn, target: target})
}

// MintRaw mints a code for pluginID whose redeem answers exactly answer
// (stepped_up and minted_at are added when missing), creating no grant.
func (f *Fake) MintRaw(pluginID int64, answer map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := answer["stepped_up"]; !ok {
		answer["stepped_up"] = false
	}
	if _, ok := answer["minted_at"]; !ok {
		answer["minted_at"] = f.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return f.putCode(&code{pluginID: pluginID, answer: string(raw)})
}

func (f *Fake) putCode(c *code) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	k := base64.RawURLEncoding.EncodeToString(b)
	c.mintedAt = f.Now()
	f.codes[k] = c
	return k
}

// Link pairs two installed server apps, as `muster-server app install …
// --link-to` does: from now on a new grant under either takes the subject the
// person already has under the other. The source's existing subjects are kept.
func (f *Fake) Link(a, b int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, y := f.byID[a], f.byID[b]
	x.partner, y.partner = y, x
}

// subjectFor is core's one subject writer: the partner's subject for this
// person when the app is in a pair and the partner knows them, else a new
// random one. f.mu is held.
func (f *Fake) subjectFor(a *app, userID int64) string {
	if a.partner != nil {
		if g := a.partner.grants[userID]; g != nil {
			return g.subject
		}
	}
	return newSubject()
}

func newSubject() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Calls returns every request so far, as "METHOD path token", with
// " act-for=<v>" for each Muster-Act-For header it carried.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CountCalls counts the requests to path (any token).
func (f *Fake) CountCalls(method, path string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, method+" "+path+" ") {
			n++
		}
	}
	return n
}

// Checked returns how many times owners/check answered each subject.
func (f *Fake) Checked() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.checked))
	for k, v := range f.checked {
		out[k] = v
	}
	return out
}

// SetFailing makes every route answer 503 (on) or serve again (off).
func (f *Fake) SetFailing(on bool) { f.set(func() { f.Failing = on }) }

// SetFailOwnersCheck makes owners/check alone answer 503 (on) or serve.
func (f *Fake) SetFailOwnersCheck(on bool) { f.set(func() { f.FailOwnersCheck = on }) }

// SetOwnersCheck sets how owners/check answers: failCall fails the nth call
// (0: none), body is a verbatim 200 answer ("": the real one), and hook runs
// before the answer is written (nil: none).
func (f *Fake) SetOwnersCheck(failCall int, body string, hook func()) {
	f.set(func() { f.FailOwnersCheckCall, f.OwnersCheckBody, f.OnOwnersCheck = failCall, body, hook })
}

// SetOwnersCheckLimit sets owners/check's calls per app per minute.
func (f *Fake) SetOwnersCheckLimit(n int) { f.set(func() { f.OwnersCheckLimit = n }) }

func (f *Fake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func write(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	write(w, code, string(b))
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// tooMany answers 429 with Retry-After, core's shape.
func tooMany(w http.ResponseWriter, until time.Duration, msg string) {
	wait := int(math.Ceil(until.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	writeJSON(w, 429, map[string]any{"error": msg, "retry_after": wait})
}

// trim drops the times at least window before now (oldest first).
func trim(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	return ts[i:]
}

// over is how long until ts (trimmed) holds fewer than limit, or 0.
func over(ts []time.Time, now time.Time, window time.Duration, limit int) time.Duration {
	if len(ts) < limit {
		return 0
	}
	return window - now.Sub(ts[len(ts)-limit])
}

// caller is the authenticated token: a server app or a personal token.
type caller struct {
	app      *app
	personal *Personal
}

// gate is the door. served is whether a server token is served the route;
// need is the capability the route needs (any, "app"). No route the
// contacts plugin calls acts for a person, so a Muster-Act-For header is 400.
func (f *Fake) gate(served bool, need string, next func(http.ResponseWriter, *http.Request, caller, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hdr := r.Header.Values(ActFor)
		f.mu.Lock()
		line := r.Method + " " + r.URL.Path + " " + tok
		for _, v := range hdr {
			line += " act-for=" + v
		}
		f.calls = append(f.calls, line)
		off, failing := f.Off, f.Failing
		a := f.apps[tok]
		p := f.personal[tok]
		f.mu.Unlock()
		if off {
			http.NotFound(w, r)
			return
		}
		if failing {
			writeErr(w, 503, "unavailable")
			return
		}
		if !bearer || (a == nil && p == nil) || (a != nil && a.revoked) {
			writeErr(w, 401, "this route takes a plugin token (mpl_…)")
			return
		}
		if p != nil {
			if len(hdr) > 0 {
				writeErr(w, 400, ActForPersonal)
				return
			}
			if next == nil {
				writeErr(w, 501, "the fake serves this route to no personal token")
				return
			}
			next(w, r, caller{personal: p}, 0)
			return
		}
		if !served {
			writeErr(w, 403, ServerRouteRefusal)
			return
		}
		if need != "" && !slices.Contains(a.Capabilities, need) {
			writeErr(w, 403, "this plugin token lacks the "+need+" capability")
			return
		}
		if len(hdr) > 0 {
			writeErr(w, 400, ActForShape)
			return
		}
		next(w, r, caller{app: a}, 0)
	}
}

func (f *Fake) self(w http.ResponseWriter, _ *http.Request, c caller, _ int64) {
	if c.personal != nil {
		p := c.personal
		// A personal token's /self as core writes it: no scope field.
		writeJSON(w, 200, map[string]any{"plugin_id": p.PluginID, "plugin_name": "personal", "owner_id": p.OwnerID,
			"owner_login": p.OwnerLogin, "owner_kind": "person", "capabilities": p.Capabilities, "deliver_to": nil})
		return
	}
	f.mu.Lock()
	a := c.app
	out := map[string]any{"plugin_id": a.PluginID, "plugin_name": a.PluginName, "scope": "server", "owner_id": nil, "owner_login": nil,
		"owner_kind": "server", "capabilities": slices.Clone(a.Capabilities), "deliver_to": nil, "act_for_hours": 8}
	f.mu.Unlock()
	writeJSON(w, 200, out)
}

// sec is a time in whole seconds: the cutoff rule compares these.
func sec(t time.Time) int64 { return t.Unix() }

// after is the whole-second rule: a sign-in strictly after the cutoff
// passes (at-or-before is refused); no cutoff passes.
func after(signin, cutoff time.Time) bool { return cutoff.IsZero() || sec(signin) > sec(cutoff) }

// standing is everything but the clock that must hold for a person: the
// account enabled and a person, and the sign-in after both cutoffs.
func standing(p *person, signin time.Time) bool {
	return p != nil && !p.disabled && p.kind == "person" && after(signin, p.signedOut) && after(signin, p.notBefore)
}

// grantState is core's serverGrantLive, split as core splits it: the ONE
// predicate that decides whether the app may act for a person. "expired" is
// a grant whose only failing gate is act_until; every other failing gate is
// "revoked". owners/check answers it; the act-for door admits only "live".
func (f *Fake) grantState(a *app, userID int64, g *grant, now time.Time) string {
	if a.revoked || a.scope != "server" || a.retired || g.hidden || g.signinAt == nil || !standing(f.people[userID], *g.signinAt) {
		return "revoked"
	}
	if !g.actUntil.After(now) {
		return "expired"
	}
	return "live"
}

func (f *Fake) redeem(w http.ResponseWriter, r *http.Request, c caller, _ int64) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		writeErr(w, 400, `the body is {"code": "<muster_code>"}`)
		return
	}
	const missing = "no such hand-off code (unknown, used, expired, or another plugin's)"
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	k, ok := f.codes[strings.TrimSpace(in.Code)]
	if c.personal != nil {
		writeErr(w, 404, missing) // a personal token can spend no server app's code
		return
	}
	a := c.app
	// expires_at > now(): a code is refused at exactly CodeLife.
	if !ok || k.spent || k.pluginID != a.PluginID || !k.mintedAt.Add(CodeLife).After(now) {
		writeErr(w, 404, missing)
		return
	}
	k.spent = true
	if k.answer != "" {
		write(w, 200, k.answer)
		return
	}
	// The person, re-checked on a fresh read against every revoke cause.
	p := f.people[k.userID]
	g := a.grants[k.userID]
	if a.scope != "server" || a.retired || (g != nil && g.hidden) || !standing(p, k.signedIn) {
		writeErr(w, 404, missing)
		return
	}
	if g == nil {
		g = &grant{subject: f.subjectFor(a, k.userID)}
		a.grants[k.userID] = g
	}
	signin := k.signedIn
	g.signinAt = &signin // the code's sign-in, never now
	if until := now.Add(GrantLife); until.After(g.actUntil) {
		g.actUntil = until
	}
	writeJSON(w, 200, map[string]any{
		"owner": map[string]any{"subject": g.subject, "display_name": p.DisplayName},
		"scope": "server", "target": k.target, "purpose": "app", "stepped_up": false,
		"minted_at": k.mintedAt.UTC().Format(time.RFC3339), "act_until": g.actUntil.UTC().Format(time.RFC3339),
	})
}

var subjectRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

func (f *Fake) ownersCheck(w http.ResponseWriter, r *http.Request, c caller, _ int64) {
	if c.personal != nil {
		writeErr(w, 403, "owners/check is for a server-installed app's token")
		return
	}
	var in struct {
		Subjects []string `json:"subjects"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxOwnersCheck*26+64))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || len(in.Subjects) < 1 || len(in.Subjects) > MaxOwnersCheck {
		writeErr(w, 400, `the body is {"subjects": [1-200 subjects]}`)
		return
	}
	for _, s := range in.Subjects {
		if !subjectRE.MatchString(s) {
			writeErr(w, 400, "each subject is the 22-character subject a redeem gave")
			return
		}
	}
	f.mu.Lock()
	now := f.Now()
	a := c.app
	a.checks = trim(a.checks, now, time.Minute)
	if until := over(a.checks, now, time.Minute, f.OwnersCheckLimit); until > 0 {
		f.mu.Unlock()
		tooMany(w, until, "too many owners checks this minute")
		return
	}
	a.checks = append(a.checks, now)
	f.ownersChecks++
	if f.FailOwnersCheck || f.ownersChecks == f.FailOwnersCheckCall {
		f.mu.Unlock()
		writeErr(w, 503, "could not check the subjects")
		return
	}
	if f.OwnersCheckBody != "" {
		body := f.OwnersCheckBody
		f.mu.Unlock()
		write(w, 200, body)
		return
	}
	// EVERY asked subject, once: one this app never redeemed is "revoked",
	// as any revoked one is, so the answer cannot tell "never seen" from
	// "ended".
	states := make(map[string]string, len(in.Subjects))
	for _, s := range in.Subjects {
		states[s] = "revoked"
		for uid, g := range a.grants {
			if g.subject == s {
				states[s] = f.grantState(a, uid, g, now)
			}
		}
		f.checked[s]++
	}
	hook := f.OnOwnersCheck
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	writeJSON(w, 200, map[string]any{"states": states})
}
