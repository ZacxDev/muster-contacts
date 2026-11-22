// Command e2e-harness runs the plugin against the fake muster for the
// browser tests in e2e/: the plugin on one origin, and a muster-like framing
// parent on another, whose /apps/<id> page frames the plugin's /enter
// exactly as muster's does. It prints one JSON line,
//
//	{"plugin": "http://127.0.0.1:<port>", "parent": "http://localhost:<port>", "plugin_id": 21}
//
// and serves until its standard input closes or it is signalled. The two
// origins are different sites, as contacts-<x>.<domain> and muster are.
//
// The fake muster has Contacts installed LINKED to a Meetings app (as
// `muster-server app install … --link-to` makes it), so a card the harness
// sends under the subject Meetings knows a person by is that person's in
// Contacts, even before they first open it.
//
// The parent is also muster's UI kit (docs/ui-kit.md): /ui-kit/v1/* from
// e2e/fixtures/ui-kit, muster's own render at one pinned commit (SHA256SUMS
// checked at start).
//
// The parent's routes, for the tests only:
//
//	GET  /kit-mode?theme=brass|harbor&missing=css,js&slow=<ms>   the kit's theme and failures
//	GET  /mint?person=<name>                 a fresh hand-off code for Contacts
//	GET  /redeems                            how many redeems the fake muster answered
//	GET  /apps/<id>?person=<name>[&code=…][&ref=…]   muster's page: frames /enter
//	POST /seed-card?person=&name=&email=&meeting=&title=  a guest's card, sent to the
//	                                          contacts API as meetings sends it, under the
//	                                          subject Meetings knows the person by; answers
//	                                          the API's status
//
// People: ada, bea, cy, dee (each a separate muster account, so specs that
// run side by side never share a list).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/mustertest"
	"github.com/ZacxDev/muster-contacts/internal/owners"
	"github.com/ZacxDev/muster-contacts/internal/server"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

var (
	app      = mustertest.App{Token: "mpl_e2e_contacts_server_3f8a", PluginID: 21, PluginName: "contacts-e2e", Capabilities: []string{"app"}}
	meetings = mustertest.App{Token: "mpl_e2e_meetings_server_6a0f", PluginID: 12, PluginName: "meetings-e2e", Capabilities: []string{"app"}}
	people   = map[string]mustertest.Person{
		"ada": {ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."},
		"bea": {ID: 5181920311, Email: "bea.person@example.test", DisplayName: "Bea P."},
		"cy":  {ID: 6272839400, Email: "cy.person@example.test", DisplayName: "Cy P."},
		"dee": {ID: 7363748511, Email: "dee.person@example.test", DisplayName: "Dee P."},
	}
)

const apiToken = "e2e-harness-contacts-api-token-0123456789"

func main() {
	logger := log.New(os.Stderr, "e2e-harness: ", 0)
	if err := run(logger); err != nil {
		logger.Fatal(err)
	}
}

func listen(host string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, "http://" + host + ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), nil
}

func run(logger *log.Logger) error {
	fake := mustertest.New()
	defer fake.Close()
	fake.Install(app)
	fake.Install(meetings)
	fake.Link(meetings.PluginID, app.PluginID)
	for _, p := range people {
		fake.AddPerson(p)
	}
	pluginLn, pluginURL, err := listen("127.0.0.1")
	if err != nil {
		return err
	}
	parentLn, parentOrigin, err := listen("localhost")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "contacts-e2e-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "contacts.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := muster.New(fake.URL, app.Token)
	if err != nil {
		return err
	}
	a, err := owners.Boot(context.Background(), c, st, logger, time.Now)
	if err != nil {
		return err
	}
	srv := server.New(server.Config{MusterOrigin: parentOrigin, MeetingsAppID: meetings.PluginID, KitUpstream: parentOrigin,
		KitTTL: time.Millisecond, KitRetry: time.Millisecond, APIToken: apiToken, Log: logger}, st, a)
	plugin := &http.Server{Handler: srv}
	go plugin.Serve(pluginLn)
	apiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	apiURL := "http://" + apiLn.Addr().String()
	internal := &http.Server{Handler: srv.Internal()}
	go internal.Serve(apiLn)

	mc, err := muster.New(fake.URL, meetings.Token)
	if err != nil {
		return err
	}
	var subjMu sync.Mutex
	subjects := map[string]string{}
	// meetingsSubject is the subject Meetings knows a person by: they open
	// Meetings once (a redeem on the Meetings app).
	meetingsSubject := func(name string) (string, error) {
		subjMu.Lock()
		defer subjMu.Unlock()
		if s, ok := subjects[name]; ok {
			return s, nil
		}
		p, ok := people[name]
		if !ok {
			return "", fmt.Errorf("no person %q", name)
		}
		red, err := mc.Redeem(context.Background(), fake.MintApp(meetings.PluginID, p.ID, ""))
		if err != nil {
			return "", err
		}
		subjects[name] = red.Owner.Subject
		return red.Owner.Subject, nil
	}

	mux := http.NewServeMux()
	kit, err := loadKit(filepath.Join("e2e", "fixtures", "ui-kit"))
	if err != nil {
		return err
	}
	kit.routes(mux)
	person := func(r *http.Request) (mustertest.Person, bool) {
		n := r.URL.Query().Get("person")
		if n == "" {
			n = "ada"
		}
		p, ok := people[n]
		return p, ok
	}
	mux.HandleFunc("GET /mint", func(w http.ResponseWriter, r *http.Request) {
		p, ok := person(r)
		if !ok {
			http.Error(w, "no such person", 400)
			return
		}
		io.WriteString(w, fake.MintApp(app.PluginID, p.ID, ""))
	})
	mux.HandleFunc("GET /redeems", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strconv.Itoa(fake.CountCalls("POST", "/plugin/v1/handoffs/redeem")))
	})
	mux.HandleFunc("POST /seed-card", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		owner, err := meetingsSubject(q.Get("person"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		now := time.Now().UTC()
		cardID := random(32)
		sub := contactsapi.Submission{Owner: owner, CardID: cardID,
			Meeting:     contactsapi.Meeting{ID: q.Get("meeting"), Title: q.Get("title"), StartedAt: &now, EndedAt: &now},
			Participant: contactsapi.Participant{ID: "p" + random(6), Identity: "guest-" + random(8), TypedName: q.Get("name"), JoinedAt: &now, LeftAt: &now},
			Name:        q.Get("name"), Email: q.Get("email")}
		body, _ := json.Marshal(sub)
		req, _ := http.NewRequest("POST", apiURL+contactsapi.RouteSubmissions, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(contactsapi.IdempotencyHeader, contactsapi.KeyForCard(cardID))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
	mux.HandleFunc("GET /apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := person(r)
		if !ok {
			http.Error(w, "no such person", 400)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			code = fake.MintApp(app.PluginID, p.ID, r.URL.Query().Get("ref"))
		}
		src := pluginURL + "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(app.PluginID, 10)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// muster's frame attributes for an app tab (internal/ui/app_page.go:
		// PluginChipSandbox, referrerpolicy no-referrer).
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1"><title>muster</title>
<body style="margin:0"><header style="height:44px;font:16px system-ui;line-height:44px;padding:0 8px">muster</header>
<iframe id="app" src="%s" sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals" allow="clipboard-write %s" referrerpolicy="no-referrer" style="display:block;border:0;width:100%%;height:calc(100dvh - 44px)"></iframe></body>`,
			html.EscapeString(src), pluginURL)
	})
	parent := &http.Server{Handler: mux}
	go parent.Serve(parentLn)

	b, _ := json.Marshal(map[string]any{"plugin": pluginURL, "parent": parentOrigin, "plugin_id": app.PluginID, "meetings_id": meetings.PluginID})
	fmt.Println(string(b))

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sig:
	}
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	plugin.Shutdown(sctx)
	parent.Shutdown(sctx)
	internal.Shutdown(sctx)
	return nil
}

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
