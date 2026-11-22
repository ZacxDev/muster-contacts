package server_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
	"github.com/ZacxDev/muster-contacts/internal/server"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

var bg = context.Background()

// ---- the contract: contactsapi's table against the real server ----

// 🔴 Every case of contactsapi/testdata/contract/routes.json runs against
// the real internal API, every precondition set up (none skipped).
func TestTheContract(t *testing.T) {
	e := newEnv(t)
	given := func(g, owner string) error {
		now := e.clock.now()
		o, err := e.st.ForOwner(owner)
		if err != nil {
			return err
		}
		switch g {
		case contactsapi.GivenOwnerRedeemed:
			_, err = e.st.CreateSession(bg, owner, "", now)
		case contactsapi.GivenOwnerExpired, contactsapi.GivenOwnerRevoked:
			if _, err = e.st.CreateSession(bg, owner, "", now); err != nil {
				return err
			}
			var exp, rev []string
			if g == contactsapi.GivenOwnerExpired {
				exp = []string{owner}
			} else {
				rev = []string{owner}
			}
			_, err = e.st.Judge(bg, nil, exp, rev, now.Add(time.Second), now.Add(time.Second))
		case contactsapi.GivenCollectOff:
			err = o.SetCollect(bg, false, now)
		case contactsapi.GivenCardLimitReached:
			e.srv.SpendCards(owner, server.DefaultCardsPerOwner)
		default:
			return contactsapi.ErrUnsupportedGiven
		}
		return err
	}
	ran := contactsapi.RunContract(t, contactsapi.Target{URL: e.api.URL, Token: apiToken, Given: given})
	c, _ := contactsapi.LoadContract()
	if ran != len(c.Cases) {
		t.Fatalf("the contract ran %d of %d cases: a precondition was skipped", ran, len(c.Cases))
	}
}

// ---- /enter ----

// 🔴 /enter refuses unless loaded in a frame, BEFORE anything is redeemed:
// the code stays unspent for the frame it was minted for.
func TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name string
		hdr  []string
		msg  string
	}{
		{"Sec-Fetch-Dest document", []string{"Sec-Fetch-Dest", "document", "Sec-Fetch-Mode", "navigate"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest empty", []string{"Sec-Fetch-Dest", "empty", "Sec-Fetch-Mode", "cors"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest twice", []string{"Sec-Fetch-Dest", "iframe", "Sec-Fetch-Dest", "iframe"}, server.EnterNotFramed},
		{"no Sec-Fetch at all", nil, server.EnterOldBrowser},
	} {
		code := e.fake.MintApp(app.PluginID, ada.ID, "")
		before := e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem")
		resp, body := e.do("GET", enterPath(code, app.PluginID), "", nil, c.hdr...)
		if resp.StatusCode != 403 {
			t.Errorf("%s: enter = %d", c.name, resp.StatusCode)
		}
		if !strings.Contains(string(body), c.msg) {
			t.Errorf("%s: the page says %q, want %q", c.name, body, c.msg)
		}
		if n := e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem") - before; n != 0 {
			t.Errorf("%s: made %d redeem call: the code was spent", c.name, n)
		}
		// The same code still works in the frame.
		resp, body = e.do("GET", enterPath(code, app.PluginID), "", nil, inFrame...)
		if resp.StatusCode != 200 || !sessionAttr.Match(body) {
			t.Errorf("%s: the unspent code in a frame = %d", c.name, resp.StatusCode)
		}
	}
}

// The redeem's answer must be an app code of this server app naming a
// person by subject.
func TestEnterRefusesAnythingButAServerAppCodeNamingASubject(t *testing.T) {
	e := newEnv(t)
	good := func() map[string]any {
		return map[string]any{"owner": map[string]any{"subject": "SSSSsubjectSSSSSSSSSSS", "display_name": "X"}, "scope": "server",
			"target": map[string]any{"app": map[string]any{}}, "purpose": "app"}
	}
	for name, mut := range map[string]func(m map[string]any){
		"purpose view":  func(m map[string]any) { m["purpose"] = "view" },
		"target mirror": func(m map[string]any) { m["target"] = map[string]any{"mirror": map[string]any{"session": 1}} },
		"both targets": func(m map[string]any) {
			m["target"] = map[string]any{"app": map[string]any{}, "mirror": map[string]any{"session": 1}}
		},
		"scope person":     func(m map[string]any) { m["scope"] = "person" },
		"subject a number": func(m map[string]any) { m["owner"] = map[string]any{"subject": "4093716825", "display_name": "X"} },
		"subject an email": func(m map[string]any) { m["owner"] = map[string]any{"subject": ada.Email, "display_name": "X"} },
	} {
		m := good()
		mut(m)
		resp, body := e.do("GET", enterPath(e.fake.MintRaw(app.PluginID, m), app.PluginID), "", nil, inFrame...)
		if resp.StatusCode != 403 || sessionAttr.Match(body) {
			t.Errorf("%s: enter = %d", name, resp.StatusCode)
		}
	}
	resp, body := e.do("GET", enterPath(e.fake.MintRaw(app.PluginID, good()), app.PluginID), "", nil, inFrame...)
	if resp.StatusCode != 200 || !sessionAttr.Match(body) {
		t.Fatalf("the good answer: enter = %d (the control)", resp.StatusCode)
	}
}

func TestEnterRedeemsOnlyForThisServerApp(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do("GET", enterPath(e.fake.MintApp(meetings.PluginID, ada.ID, ""), meetings.PluginID), "", nil, inFrame...)
	if resp.StatusCode != 403 {
		t.Fatalf("another plugin id = %d, want 403", resp.StatusCode)
	}
	resp, _ = e.do("GET", enterPath(e.fake.MintApp(meetings.PluginID, ada.ID, ""), app.PluginID), "", nil, inFrame...)
	if resp.StatusCode != 403 {
		t.Fatalf("another app's code = %d, want 403", resp.StatusCode)
	}
}

// The page: no cookie, framed by muster alone, never cached, and a ref only
// for the owner's own contact.
func TestEnterHeadersAndRef(t *testing.T) {
	e := newEnv(t)
	s := e.meetingsSubject(ada)
	e.submit(s, cardID("Ada1"), "Grace", "grace@example.test", "mtg1", "part1")
	tok := e.session(ada)
	list := decode[struct{ Contacts []store.Row }](t, mustOK(t, e)(e.do("GET", "/api/contacts", tok, nil)))
	mine := list.Contacts[0].ID
	beaTok := e.session(bea)
	beas := decode[struct{ Contact store.Contact }](t, mustOK(t, e)(e.do("POST", "/api/contacts", beaTok, map[string]any{"name": "Bea's friend"})))

	for ref, want := range map[string]bool{mine: true, beas.Contact.ID: false, "nope": false} {
		resp, body := e.do("GET", enterPath(e.fake.MintApp(app.PluginID, ada.ID, ref), app.PluginID), "", nil, inFrame...)
		if resp.StatusCode != 200 {
			t.Fatalf("enter = %d", resp.StatusCode)
		}
		if got := strings.Contains(string(body), `data-ref="`+ref+`"`); got != want {
			t.Errorf("ref %s reached the page: %v, want %v (another person's ref must not)", ref, got, want)
		}
		if c := resp.Header.Values("Set-Cookie"); len(c) > 0 {
			t.Errorf("enter set a cookie: %v", c)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "frame-ancestors https://muster.example.test") {
			t.Errorf("CSP %q", csp)
		}
		if resp.Header.Get("X-Frame-Options") != "" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("X-Frame-Options %q, Cache-Control %q", resp.Header.Get("X-Frame-Options"), resp.Header.Get("Cache-Control"))
		}
		if !strings.Contains(string(body), `data-meetings="https://muster.example.test/apps/12"`) {
			t.Errorf("the page has no Meetings link base")
		}
	}
}

func mustOK(t *testing.T, e *env) func(*http.Response, []byte) []byte {
	return func(resp *http.Response, body []byte) []byte {
		t.Helper()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s = %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body)
		}
		return body
	}
}

// ---- the owner API ----

func TestOwnerRoutesNeedALiveSession(t *testing.T) {
	e := newEnv(t)
	tok := e.session(ada)
	for _, bad := range []string{"", "mco_" + strings.Repeat("A", 43), "Bearer", tok + "x"} {
		resp, _ := e.do("GET", "/api/contacts", bad, nil)
		if resp.StatusCode != 401 {
			t.Errorf("bearer %q = %d, want 401", bad, resp.StatusCode)
		}
	}
	if resp, _ := e.do("GET", "/api/contacts", tok, nil); resp.StatusCode != 200 {
		t.Fatalf("the live session = %d", resp.StatusCode)
	}
	e.clock.add(store.SessionLifetime)
	if resp, _ := e.do("GET", "/api/contacts", tok, nil); resp.StatusCode != 401 {
		t.Fatalf("a session at its lifetime = %d, want 401", resp.StatusCode)
	}
}

// 🔴 owner scoping (door): every owner route called as Ada with Bea's
// contact id answers exactly what an unknown id answers, byte for byte.
func TestAnotherOwnersContactIsTheSame404ByteForByte(t *testing.T) {
	e := newEnv(t)
	adaTok, beaTok := e.session(ada), e.session(bea)
	beas := decode[struct{ Contact store.Contact }](t, mustOK(t, e)(e.do("POST", "/api/contacts", beaTok, map[string]any{"name": "Beatrix Kiddo", "emails": []string{"beatrix@example.test"}})))
	mine := decode[struct{ Contact store.Contact }](t, mustOK(t, e)(e.do("POST", "/api/contacts", adaTok, map[string]any{"name": "Alma"})))
	const unknown = "UUUUunknownUUUUUUUUUUU"
	routes := []struct {
		method string
		path   func(id string) string
		body   func(id string) any
	}{
		{"GET", func(id string) string { return "/api/contacts/" + id }, nil},
		{"PATCH", func(id string) string { return "/api/contacts/" + id }, func(string) any { return map[string]any{"name": "stolen"} }},
		{"DELETE", func(id string) string { return "/api/contacts/" + id }, nil},
		{"GET", func(id string) string { return "/api/contacts/" + id + "/export" }, nil},
		{"POST", func(id string) string { return "/api/contacts/" + id + "/merge" }, func(string) any { return map[string]any{"from": mine.Contact.ID} }},
		{"POST", func(string) string { return "/api/contacts/" + mine.Contact.ID + "/merge" }, func(id string) any { return map[string]any{"from": id} }},
	}
	for i, r := range routes {
		var b1, b2 any
		if r.body != nil {
			b1, b2 = r.body(beas.Contact.ID), r.body(unknown)
		}
		theirs, them := e.do(r.method, r.path(beas.Contact.ID), adaTok, b1)
		none, nobody := e.do(r.method, r.path(unknown), adaTok, b2)
		if theirs.StatusCode != 404 || none.StatusCode != 404 || string(them) != string(nobody) {
			t.Errorf("owner scoping: route %d %s: another owner's contact = %d %s; an unknown one = %d %s", i, r.method, theirs.StatusCode, them, none.StatusCode, nobody)
		}
	}
	got := decode[struct{ Contact store.Contact }](t, mustOK(t, e)(e.do("GET", "/api/contacts/"+beas.Contact.ID, beaTok, nil)))
	if got.Contact.Name != "Beatrix Kiddo" {
		t.Fatalf("Bea's contact after Ada's attempts: %+v", got.Contact)
	}
	if list := decode[struct{ Contacts []store.Row }](t, mustOK(t, e)(e.do("GET", "/api/contacts?q=beatrix", adaTok, nil))); len(list.Contacts) != 0 {
		t.Fatalf("Ada's search finds Bea's contact")
	}
}

func TestTheOwnerAPIRoundTrip(t *testing.T) {
	e := newEnv(t)
	tok := e.session(ada)
	ok := mustOK(t, e)
	grace := decode[struct{ Contact store.Contact }](t, ok(e.do("POST", "/api/contacts", tok, map[string]any{"name": "Grace", "emails": []string{"grace@example.test"}, "note": "met at the fair"}))).Contact
	if grace.NameSource != "owner" || grace.Provenance[0].Kind != "owner_entered" || grace.Note != "met at the fair" {
		t.Fatalf("created %+v", grace)
	}
	// An email another contact holds: 409, naming that contact.
	resp, body := e.do("POST", "/api/contacts", tok, map[string]any{"name": "Other", "emails": []string{"GRACE@example.test"}})
	conflict := decode[struct {
		Conflict struct{ ID, Name string }
	}](t, body)
	if resp.StatusCode != 409 || conflict.Conflict.ID != grace.ID || conflict.Conflict.Name != "Grace" {
		t.Fatalf("a held email = %d %s", resp.StatusCode, body)
	}
	// A bad email: 400 naming the field and which.
	resp, body = e.do("POST", "/api/contacts", tok, map[string]any{"name": "X", "emails": []string{"ok@example.test", "not an email"}})
	if resp.StatusCode != 400 || !strings.Contains(string(body), `"field":"emails","index":1`) {
		t.Fatalf("a bad email = %d %s", resp.StatusCode, body)
	}
	// An unknown field is refused.
	if resp, _ := e.do("POST", "/api/contacts", tok, map[string]any{"name": "X", "owner": "someone"}); resp.StatusCode != 400 {
		t.Fatalf("an unknown field = %d", resp.StatusCode)
	}
	other := decode[struct{ Contact store.Contact }](t, ok(e.do("POST", "/api/contacts", tok, map[string]any{"name": "Hopper", "emails": []string{"hopper@example.test"}}))).Contact
	merged := decode[struct{ Contact store.Contact }](t, ok(e.do("POST", "/api/contacts/"+grace.ID+"/merge", tok, map[string]any{"from": other.ID}))).Contact
	if len(merged.Emails) != 2 {
		t.Fatalf("merged %+v", merged)
	}
	name := "Grace B. Hopper"
	patched := decode[struct{ Contact store.Contact }](t, ok(e.do("PATCH", "/api/contacts/"+grace.ID, tok, map[string]any{"name": name}))).Contact
	if patched.Name != name {
		t.Fatalf("patched %+v", patched)
	}
	resp, body = e.do("GET", "/api/contacts/"+grace.ID+"/export", tok, nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(string(body), "hopper@example.test") {
		t.Fatalf("export = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Disposition"), body)
	}
	if resp, _ := e.do("DELETE", "/api/contacts/"+grace.ID, tok, nil); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/contacts/"+grace.ID, tok, nil); resp.StatusCode != 404 {
		t.Fatalf("after delete = %d", resp.StatusCode)
	}
	// Settings and Delete everything.
	ok(e.do("PUT", "/api/settings", tok, map[string]any{"collect": false}))
	st := decode[store.Settings](t, ok(e.do("GET", "/api/settings", tok, nil)))
	if st.Collect {
		t.Fatalf("collect still on")
	}
	ok(e.do("POST", "/api/contacts", tok, map[string]any{"name": "Left over"}))
	if resp, _ := e.do("DELETE", "/api/all", tok, map[string]any{"confirm": "yes"}); resp.StatusCode != 400 {
		t.Fatalf("delete everything without the typed confirmation = %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "/api/all", tok, map[string]any{"confirm": server.DeleteEverything}); resp.StatusCode != 204 {
		t.Fatalf("delete everything = %d", resp.StatusCode)
	}
	if list := decode[struct{ Contacts []store.Row }](t, ok(e.do("GET", "/api/contacts", tok, nil))); len(list.Contacts) != 0 {
		t.Fatalf("after delete everything: %+v", list.Contacts)
	}
}

// ---- the linked pair, and muster's word on a person ----

// A person who has never opened Contacts holds a meeting; a guest's card
// arrives under the subject Meetings knows them by; the first time they
// open Contacts it is there. Another person's list stays empty.
func TestALinkedPairOwnerFindsCardsSentBeforeTheirFirstOpen(t *testing.T) {
	e := newEnv(t)
	s := e.meetingsSubject(ada)
	e.submit(s, cardID("First"), "Grace", "grace@example.test", "mtgFirst", "partFirst")
	tok := e.session(ada)
	if got := e.fake.Subject(app.PluginID, ada.ID); got != s {
		t.Fatalf("Contacts knows Ada as %q, Meetings as %q: the fake did not link them", got, s)
	}
	list := decode[struct{ Contacts []store.Row }](t, mustOK(t, e)(e.do("GET", "/api/contacts", tok, nil)))
	if len(list.Contacts) != 1 || list.Contacts[0].Name != "Grace" || list.Contacts[0].Origin != "self_submitted" || list.Contacts[0].OriginTitle != "Meeting mtgFirst" {
		t.Fatalf("Ada's first open: %+v", list.Contacts)
	}
	beaList := decode[struct{ Contacts []store.Row }](t, mustOK(t, e)(e.do("GET", "/api/contacts", e.session(bea), nil)))
	if len(beaList.Contacts) != 0 {
		t.Fatalf("Bea sees %+v", beaList.Contacts)
	}
}

// 🔴 muster's owners/check, through the poll: revoked refuses the API and
// ends the session; showing the app again (live or expired) accepts again;
// expired ends the session and keeps accepting.
func TestMustersWordOnAPersonReachesTheAPI(t *testing.T) {
	e := newEnv(t)
	tok := e.session(ada)
	s := e.fake.Subject(app.PluginID, ada.ID)
	standing := func() int {
		resp, _ := e.apiDo("GET", "/v1/owners/"+s, nil)
		return resp.StatusCode
	}
	if standing() != 200 {
		t.Fatalf("a live owner is not accepting")
	}
	e.fake.Hide(app.PluginID, ada.ID)
	e.app.PollOnce(bg)
	if got := standing(); got != 404 {
		t.Fatalf("accepting revoked: after Hide and a poll, owners = %d, want 404", got)
	}
	if resp, _ := e.do("GET", "/api/contacts", tok, nil); resp.StatusCode != 401 {
		t.Fatalf("the hidden person's session still works: %d", resp.StatusCode)
	}
	receipt := e.submit(s, cardID("Hid"), "Grace", "grace@example.test", "m1", "p1")
	e.fake.Show(app.PluginID, ada.ID)
	e.app.PollOnce(bg)
	if got := standing(); got != 200 {
		t.Fatalf("expired accepted: after Show and a poll (the grant is expired: Hide ended its clock), owners = %d, want 200", got)
	}
	resp, body := e.apiDo("POST", "/v1/receipts/check", map[string]any{"owner": s, "receipt": receipt})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"known":false`) {
		t.Fatalf("the card sent while hidden was kept: %d %s", resp.StatusCode, body)
	}
	tok = e.session(ada)
	e.fake.Expire(app.PluginID, ada.ID)
	e.app.PollOnce(bg)
	if got := standing(); got != 200 {
		t.Fatalf("expired accepted: after Expire and a poll, owners = %d, want 200", got)
	}
	if resp, _ := e.do("GET", "/api/contacts", tok, nil); resp.StatusCode != 401 {
		t.Fatalf("an expired person's session still works: %d", resp.StatusCode)
	}
	// The app uninstalled: nothing is accepted.
	e.fake.Uninstall(app.PluginID)
	e.app.PollOnce(bg)
	if got := standing(); got != 404 {
		t.Fatalf("after uninstall, owners = %d, want 404", got)
	}
}

// ---- the limits ----

func TestCardRateLimits(t *testing.T) {
	e := newEnv(t, func(c *server.Config) { c.CardsPerOwner, c.CardsPerInstall = 2, 3 })
	a, b := e.meetingsSubject(ada), e.meetingsSubject(bea)
	send := func(owner, id string) *http.Response {
		resp, _ := e.apiDo("POST", "/v1/submissions", cardBody(owner, cardID(id), "Grace", id+"@example.test", "m1", "p"+id), "Idempotency-Key", "card:"+cardID(id))
		return resp
	}
	for _, id := range []string{"aa", "ab"} {
		if r := send(a, id); r.StatusCode != 202 {
			t.Fatalf("card %s = %d", id, r.StatusCode)
		}
	}
	r := send(a, "ac")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit per owner: the owner's third card = %d (Retry-After %q), want 429", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if r := send(a, "aa"); r.StatusCode != 202 {
		t.Fatalf("a replay past the limit = %d, want 202 (a replay is not counted)", r.StatusCode)
	}
	if r := send(b, "ba"); r.StatusCode != 202 {
		t.Fatalf("another owner's first card = %d", r.StatusCode)
	}
	if r := send(b, "bb"); r.StatusCode != 429 {
		t.Fatalf("rate limit per install: the install's fourth card = %d, want 429", r.StatusCode)
	}
	e.clock.add(server.DefaultCardWindow)
	if r := send(a, "ad"); r.StatusCode != 202 {
		t.Fatalf("after the window = %d", r.StatusCode)
	}
}

// ---- 🔴 no PII in logs ----

// fixtures are distinctive strings that may appear in no log line.
var piiFixtures = []string{"Zebulon Quarterfixture", "zq.fixture-7731@example.test", "Planning with Zebulon", "a note about zebulon", "zebulon-search", "Zebby"}

func leaks(log string) []string {
	var out []string
	for _, f := range piiFixtures {
		if strings.Contains(strings.ToLower(log), strings.ToLower(f)) {
			out = append(out, f)
		}
	}
	return out
}

// Every route that takes a name, an email, a title, a note or a search is
// driven with the fixtures, success and refusal; the log holds none of
// them, nor a token, a code, a receipt or a subject.
func TestNoPIIReachesTheLog(t *testing.T) {
	e := newEnv(t)
	s := e.meetingsSubject(ada)
	body := cardBody(s, cardID("Pii"), piiFixtures[0], piiFixtures[1], "mPii", "pPii")
	body["meeting"].(map[string]any)["title"] = piiFixtures[2]
	resp, raw := e.apiDo("POST", "/v1/submissions", body, "Idempotency-Key", "card:"+cardID("Pii"))
	if resp.StatusCode != 202 {
		t.Fatalf("submission = %d %s", resp.StatusCode, raw)
	}
	receipt := decode[contactsapi.SubmissionAnswer](t, raw).Receipt
	bad := cardBody(s, cardID("Bad"), piiFixtures[0], "not-an-email "+piiFixtures[1], "mPii", "pBad")
	e.apiDo("POST", "/v1/submissions", bad, "Idempotency-Key", "card:"+cardID("Bad"))
	e.apiDo("POST", "/v1/submissions", `{"owner": "`+piiFixtures[1]+`", "x": 1}`, "Idempotency-Key", "card:x")
	e.apiDo("GET", "/v1/owners/"+piiFixtures[1], nil)
	e.apiDo("POST", "/v1/receipts/check", map[string]any{"owner": s, "receipt": receipt})
	tok := e.session(ada)
	e.do("GET", "/api/contacts?q="+piiFixtures[4], tok, nil)
	c := decode[struct{ Contact store.Contact }](t, mustOK(t, e)(e.do("POST", "/api/contacts", tok, map[string]any{"name": piiFixtures[5], "note": piiFixtures[3]})))
	e.do("PATCH", "/api/contacts/"+c.Contact.ID, tok, map[string]any{"emails": []string{piiFixtures[1]}}) // 409: held by the card's contact
	e.do("PATCH", "/api/contacts/"+c.Contact.ID, tok, map[string]any{"emails": []string{"bad " + piiFixtures[1]}})
	e.do("GET", "/api/export", tok, nil)
	e.do("GET", "/api/"+piiFixtures[1], tok, nil)
	logged := e.log.String()
	if l := leaks(logged); len(l) > 0 {
		t.Fatalf("no PII in logs: the log holds %q:\n%s", l, logged)
	}
	for _, secret := range []string{tok, receipt, s, apiToken, app.Token, cardID("Pii")} {
		if strings.Contains(logged, secret) {
			t.Fatalf("no PII in logs: the log holds a token, receipt, subject or card id %q", secret)
		}
	}
	if !strings.Contains(logged, "api POST /v1/submissions 202") || !strings.Contains(logged, "http GET /api/contacts 200") {
		t.Fatalf("the log has no access lines (the test drove nothing?):\n%s", logged)
	}
	// The positive control: a fixture logged on purpose IS found.
	log.New(e.log, "", 0).Print("control: " + piiFixtures[1])
	if l := leaks(e.log.String()); len(l) != 1 {
		t.Fatalf("the control was not seen (%v): the leak check cannot see a fixture", l)
	}
}

// The page draws no colour of its own (every colour is a kit token).
func TestThePluginCSSWritesNoColour(t *testing.T) {
	e := newEnv(t)
	_, css := e.do("GET", "/static/app.css", "", nil)
	lit := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(\s*\d|\bhsla?\(|(?:^|[^-\w])(?:white|black|red|blue|green|gray|grey)(?:[^-\w]|$)`)
	if m := lit.FindAllString(string(css), -1); len(m) > 0 || len(css) < 1000 {
		t.Fatalf("app.css writes colours %q (or did not load: %d bytes)", m, len(css))
	}
}

var _ = json.Marshal
