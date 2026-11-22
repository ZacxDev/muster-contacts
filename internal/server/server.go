// Package server is the plugin's HTTP surface: the owner app on the public
// listener (its own origin, framed by muster), and the contacts API on the
// internal one (api.go), reachable only from the meetings plugin.
//
//	GET  /enter            redeem muster's hand-off code, mint an owner session, render the app
//	GET  /kit/v1/{file}    muster's UI kit, proxied (kit.go)
//	GET  /static/…         the page's scripts and styles, versioned (assets.go)
//	     /api/…            the owner API (owner.go), Authorization: Bearer mco_…
//
// Owner routes take the bearer and nothing else: no cookie is ever set or
// read (muster's frame is a third-party context). A missing, unknown or
// expired session is 401, and another person's contact is the same 404 as
// one that does not exist.
//
// 🔴 No log line carries a name, an email, a title, a note, a search, a
// code or a token: the access log is the method, the ROUTE PATTERN (never the
// path, never the query), the status and the duration; and no handler logs
// a request's body (TestNoPIIReachesTheLog).
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/owners"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

//go:embed web
var webFS embed.FS

// pageTmpl is the page; every asset it references is named through
// pageFuncs (assets.go), so it carries its version.
var pageTmpl = template.Must(template.New("").Funcs(pageFuncs).ParseFS(webFS, "web/*.html"))

// Config is what the server needs to know about where it runs.
type Config struct {
	// MusterOrigin is muster's browser-facing origin: the only page allowed
	// to frame the owner app (CSP frame-ancestors).
	MusterOrigin string
	// MeetingsAppID is the Meetings app's id on muster, when set: a contact's
	// meeting line then links to /apps/<id>?ref=<meeting> on MusterOrigin.
	MeetingsAppID int64
	// KitUpstream is muster's base URL as the plugin calls it (MUSTER_URL):
	// the UI kit is fetched from <KitUpstream>/ui-kit/v1/ server-side. ""
	// serves the bundled kit only.
	KitUpstream string
	KitTimeout  time.Duration
	KitTTL      time.Duration
	KitRetry    time.Duration
	// APIToken is CONTACTS_API_TOKEN: the internal API's bearer.
	APIToken string
	// CardsPerOwner and CardsPerInstall cap card submissions in CardWindow
	// (design: 200 per owner and 2,000 per install a day).
	CardsPerOwner   int
	CardsPerInstall int
	CardWindow      time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time
	// Log receives the access logs and refusals; never a name, an email, a
	// title, a note, a search, a code or a token.
	Log *log.Logger
}

// The defaults for the card limits.
const (
	DefaultCardsPerOwner   = 200
	DefaultCardsPerInstall = 2000
	DefaultCardWindow      = 24 * time.Hour
)

// Server is the plugin's public handler; Internal is the API's.
type Server struct {
	kit    *kitProxy
	cfg    Config
	store  *store.Store
	app    *owners.App
	mux    *http.ServeMux
	limits *cardLimiter
}

// New builds the handler.
func New(cfg Config, st *store.Store, app *owners.App) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	if cfg.CardsPerOwner <= 0 {
		cfg.CardsPerOwner = DefaultCardsPerOwner
	}
	if cfg.CardsPerInstall <= 0 {
		cfg.CardsPerInstall = DefaultCardsPerInstall
	}
	if cfg.CardWindow <= 0 {
		cfg.CardWindow = DefaultCardWindow
	}
	s := &Server{cfg: cfg, store: st, app: app, mux: http.NewServeMux(),
		limits: newCardLimiter(cfg.CardsPerOwner, cfg.CardsPerInstall, cfg.CardWindow, cfg.Now)}
	s.kit = newKitProxy(cfg.KitUpstream, cfg.KitTimeout, cfg.KitTTL, cfg.KitRetry, cfg.Now)
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err)
	}
	s.mux.HandleFunc("GET /enter", s.handleEnter)
	s.mux.HandleFunc("GET "+KitRoute+"{file}", s.handleKit)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	s.ownerRoutes()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	})
	return s
}

// The headers every response carries; the page's CSP replaces baseCSP.
const baseCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// pageCSP is the owner app's policy: its own script, style and images only
// (the kit is served from this origin), its own API only, framed by
// muster's exact origin alone.
func (s *Server) pageCSP() string {
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
		"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + s.cfg.MusterOrigin
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	h := w.Header()
	h.Set("Content-Security-Policy", baseCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(sw, r)
	s.cfg.Log.Printf("http %s %s %d %s", r.Method, routeOf(r), sw.status, s.cfg.Now().Sub(start).Round(time.Millisecond))
}

// routeOf is what the access log says of a request's path: the pattern
// the router matched (`/api/contacts/{id}`), never the path or the query,
// which can hold a search, a code or anything a stranger typed.
func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return "(unmatched)"
	}
	if _, p, ok := strings.Cut(r.Pattern, " "); ok {
		return p
	}
	return r.Pattern
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- /enter: the hand-off ----

// handoffCodeRE is muster's code shape: 32 bytes, base64url, 43 characters.
var handoffCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// The /enter refusals: whole sentences the person reads.
const (
	EnterAgain      = "Open Contacts from muster again."
	EnterNotFramed  = "Open Contacts from muster."
	EnterOldBrowser = "This browser is too old for Contacts. Update it, or open muster in a current browser."
	EnterNotSetUp   = "This Contacts server does not serve that muster tab. Ask the person who runs it to check the plugin token."
	EnterRefused    = "That link to Contacts has expired or was already used."
	EnterNoMuster   = "Contacts could not reach muster. Try again in a moment."
	EnterWrongKind  = "That link was not made for the Contacts tab."
)

type enterPage struct {
	Session  string
	Ref      string
	Meetings string // the Meetings tab's address on muster, "" when not configured
	Message  string
	Again    bool
}

func (s *Server) renderEnter(w http.ResponseWriter, code int, p enterPage) {
	h := w.Header()
	h.Set("Content-Security-Policy", s.pageCSP())
	h.Del("X-Frame-Options") // framed by muster: frame-ancestors decides
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pageTmpl.ExecuteTemplate(w, "app.html", p); err != nil {
		s.cfg.Log.Printf("page app.html: %v", err)
	}
}

func (s *Server) refuseEnter(w http.ResponseWriter, code int, msg, why string) {
	s.cfg.Log.Printf("enter: refused %d: %s", code, why)
	s.renderEnter(w, code, enterPage{Message: msg, Again: msg != EnterNotSetUp})
}

// framed says the browser is loading /enter into a frame: exactly one
// Sec-Fetch-Dest header, and it says iframe.
func framed(r *http.Request) (ok bool, why string) {
	if d := r.Header.Values("Sec-Fetch-Dest"); len(d) == 1 && d[0] == "iframe" {
		return true, ""
	}
	for k := range r.Header {
		if strings.HasPrefix(k, "Sec-Fetch-") {
			return false, EnterNotFramed
		}
	}
	return false, EnterOldBrowser
}

// handleEnter is GET /enter?muster_code=…&muster_plugin=…, framed by
// muster's /apps/{id} page, exactly as meetings' is.
//
// 🔴 It refuses unless the browser says it is loading a frame
// (Sec-Fetch-Dest: iframe), before anything is redeemed: with one server
// token, a link to /enter carrying someone else's unspent code would sign
// whoever opens it in as that someone.
func (s *Server) handleEnter(w http.ResponseWriter, r *http.Request) {
	if ok, why := framed(r); !ok {
		s.refuseEnter(w, http.StatusForbidden, why, "not loaded in a frame (Sec-Fetch-Dest)")
		return
	}
	q := r.URL.Query()
	code, pid := q.Get("muster_code"), q.Get("muster_plugin")
	if code == "" && pid == "" {
		s.refuseEnter(w, http.StatusBadRequest, EnterAgain, "no code (a reload)")
		return
	}
	pluginID, err := strconv.ParseInt(pid, 10, 64)
	if err != nil || pluginID <= 0 || !handoffCodeRE.MatchString(code) {
		s.refuseEnter(w, http.StatusBadRequest, EnterAgain, "a malformed code or plugin id")
		return
	}
	if pluginID != s.app.PluginID || !s.app.Live() {
		s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "plugin "+pid+" is not this server app, or its token is dead")
		return
	}
	red, err := s.app.Client.Redeem(r.Context(), code)
	if err != nil {
		e := muster.AsError(err)
		switch {
		case e != nil && e.Kind == muster.KindOff:
			s.refuseEnter(w, http.StatusForbidden, EnterRefused, "muster: no such code")
		case e != nil && e.Kind == muster.KindAuth:
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			s.app.Revoke(ctx, "the redeem answered 401: the app was uninstalled or its token rotated")
			cancel()
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: the token is revoked")
		case e != nil && e.Kind == muster.KindCapability:
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: the token may not redeem")
		default:
			s.refuseEnter(w, http.StatusBadGateway, EnterNoMuster, "muster: "+kindOf(err))
		}
		return
	}
	// 🔴 The checks that make the code this tab's.
	if red.Purpose != "app" {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed purpose is not app")
		return
	}
	if red.Target.App == nil || len(red.Target.Mirror) > 0 {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed target is not {\"app\": {}}")
		return
	}
	if red.Scope != muster.ScopeServer {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed scope is not server")
		return
	}
	if !store.ValidSubject(red.Owner.Subject) {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed owner has no subject")
		return
	}
	subject := red.Owner.Subject
	session, err := s.store.CreateSession(r.Context(), subject, red.Owner.DisplayName, s.cfg.Now())
	if err != nil {
		s.refuseEnter(w, http.StatusInternalServerError, EnterNoMuster, "session could not be stored")
		return
	}
	// ref is a hint (a contact to open): passed on only when it names one of
	// this person's own contacts.
	ref := ""
	if o, err := s.store.ForOwner(subject); err == nil && red.Target.App.Ref != "" {
		if _, err := o.Get(r.Context(), red.Target.App.Ref); err == nil {
			ref = red.Target.App.Ref
		}
	}
	s.renderEnter(w, http.StatusOK, enterPage{Session: session, Ref: ref, Meetings: s.meetingsURL()})
}

// meetingsURL is the Meetings tab on muster, "" when its id is not set.
func (s *Server) meetingsURL() string {
	if s.cfg.MeetingsAppID <= 0 {
		return ""
	}
	return s.cfg.MusterOrigin + "/apps/" + strconv.FormatInt(s.cfg.MeetingsAppID, 10)
}

// kindOf names an error by its kind only.
func kindOf(err error) string {
	if e := muster.AsError(err); e != nil {
		return string(e.Kind) + " " + strconv.Itoa(e.Status)
	}
	return "error"
}

// ---- owner sessions ----

type ownerHandler func(w http.ResponseWriter, r *http.Request, o *store.Owner)

// owner admits a request carrying a live owner session's bearer and hands
// the handler that person's view of the store, and only it: the owner comes
// from the session row, never from the request.
func (s *Server) owner(next ownerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refuse := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="contacts"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			refuse()
			return
		}
		sess, err := s.store.LookupSession(r.Context(), tok, s.cfg.Now())
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the session could not be read"})
			return
		}
		if !s.app.Live() {
			refuse()
			return
		}
		o, err := s.store.ForOwner(sess.Subject)
		if err != nil {
			refuse()
			return
		}
		next(w, r, o)
	}
}
