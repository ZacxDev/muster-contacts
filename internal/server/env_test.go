package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/mustertest"
	"github.com/ZacxDev/muster-contacts/internal/owners"
	"github.com/ZacxDev/muster-contacts/internal/server"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

// The fake muster's two apps: Contacts (this plugin) and Meetings (its
// linked partner), and three people whose fixtures are pairwise distinct.
var (
	app      = mustertest.App{Token: "mpl_contacts_test_server_7c1e", PluginID: 21, PluginName: "contacts-test", Capabilities: []string{"app"}}
	meetings = mustertest.App{Token: "mpl_meetings_test_server_4b9d", PluginID: 12, PluginName: "meetings-test", Capabilities: []string{"app"}}
	ada      = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
	bea      = mustertest.Person{ID: 5181920311, Email: "bea.person@example.test", DisplayName: "Bea P."}
	cy       = mustertest.Person{ID: 6272839400, Email: "cy.person@example.test", DisplayName: "Cy P."}
)

// apiToken is the contacts API's bearer in the tests.
const apiToken = "contacts-api-token-for-tests-0123456789abcdef"

var inFrame = []string{"Sec-Fetch-Dest", "iframe", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "cross-site"}

// syncBuf is a log the server writes from many goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t     *testing.T
	fake  *mustertest.Fake
	st    *store.Store
	app   *owners.App
	srv   *server.Server
	pub   *httptest.Server
	api   *httptest.Server
	log   *syncBuf
	clock *clock
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newEnv runs the plugin against a fake muster with Contacts installed and
// linked to Meetings, as `app install … --link-to` makes it.
func newEnv(t *testing.T, tweak ...func(*server.Config)) *env {
	t.Helper()
	e := &env{t: t, fake: mustertest.New(), log: &syncBuf{}, clock: &clock{t: time.Now()}}
	t.Cleanup(e.fake.Close)
	e.fake.Install(app)
	e.fake.Install(meetings)
	e.fake.Link(meetings.PluginID, app.PluginID)
	for _, p := range []mustertest.Person{ada, bea, cy} {
		e.fake.AddPerson(p)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	c, err := muster.New(e.fake.URL, app.Token)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(e.log, "", 0)
	e.app, err = owners.Boot(context.Background(), c, st, logger, e.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{MusterOrigin: "https://muster.example.test", MeetingsAppID: meetings.PluginID, APIToken: apiToken, Log: logger, Now: e.clock.now}
	for _, f := range tweak {
		f(&cfg)
	}
	e.srv = server.New(cfg, st, e.app)
	e.pub = httptest.NewServer(e.srv)
	e.api = httptest.NewServer(e.srv.Internal())
	t.Cleanup(e.pub.Close)
	t.Cleanup(e.api.Close)
	return e
}

func (e *env) request(base, method, path, bearer string, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// do calls the public listener; apiDo the internal API with its token.
func (e *env) do(method, path, bearer string, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	return e.request(e.pub.URL, method, path, bearer, body, hdr...)
}

func (e *env) apiDo(method, path string, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	return e.request(e.api.URL, method, path, apiToken, body, hdr...)
}

func enterPath(code string, pluginID int64) string {
	return "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(pluginID, 10)
}

var sessionAttr = regexp.MustCompile(`data-session="(mco_[A-Za-z0-9_-]{43})"`)

// session opens the Contacts tab as p (a framed /enter) and answers the
// owner session's bearer.
func (e *env) session(p mustertest.Person) string {
	e.t.Helper()
	resp, body := e.do("GET", enterPath(e.fake.MintApp(app.PluginID, p.ID, ""), app.PluginID), "", nil, inFrame...)
	m := sessionAttr.FindSubmatch(body)
	if resp.StatusCode != 200 || m == nil {
		e.t.Fatalf("enter as %s = %d: %s", p.DisplayName, resp.StatusCode, body)
	}
	return string(m[1])
}

// meetingsSubject is the subject Meetings knows p by: p opens Meetings
// (a redeem on the Meetings app), and the linked pair shares it.
func (e *env) meetingsSubject(p mustertest.Person) string {
	e.t.Helper()
	c, err := muster.New(e.fake.URL, meetings.Token)
	if err != nil {
		e.t.Fatal(err)
	}
	red, err := c.Redeem(context.Background(), e.fake.MintApp(meetings.PluginID, p.ID, ""))
	if err != nil {
		e.t.Fatal(err)
	}
	return red.Owner.Subject
}

// cardBody is a submission for owner, with a fresh card id.
func cardBody(owner, cardID, name, email, meetingID, participantID string) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{"owner": owner, "card_id": cardID,
		"meeting":     map[string]any{"id": meetingID, "title": "Meeting " + meetingID, "started_at": now, "ended_at": now},
		"participant": map[string]any{"id": participantID, "identity": "guest-" + participantID, "typed_name": name, "joined_at": now, "left_at": now},
		"name":        name, "email": email}
}

// cardID is a well-formed card id (43 base64url characters) from a seed.
func cardID(seed string) string {
	s := strings.Repeat(seed, 43/len(seed)+1)
	return s[:43]
}

// submit sends a card: 202, no body.
func (e *env) submit(owner, id, name, email, meetingID, participantID string) {
	e.t.Helper()
	resp, body := e.apiDo("POST", "/v1/submissions", cardBody(owner, id, name, email, meetingID, participantID), "Idempotency-Key", "card:"+id)
	if resp.StatusCode != 202 || len(body) != 0 {
		e.t.Fatalf("submission = %d: %q, want 202 and no body", resp.StatusCode, body)
	}
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}
