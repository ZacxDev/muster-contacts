// Command muster-contacts is the contacts plugin for muster: a Contacts tab
// (muster's `app` capability) that keeps the people its owner has met and the
// meetings they met in, and an internal API the meetings plugin sends a
// guest's card and "met at meeting X" to. Phase 1 of muster's
// docs/design/contacts.md: the store, the owner app, and the API.
//
// It runs as one server-installed app, installed linked to Meetings
// (`muster-server app install … --link-to <meetings app id>`), so both apps
// know each person by the same subject.
//
// Configuration is read from the environment only (see .env.example):
//
//	MUSTER_URL                where the plugin calls muster (https; http only to loopback, <svc>.<ns>.svc.<cluster domain>. or CONTACTS_MUSTER_HTTP_HOST)
//	CONTACTS_CLUSTER_DOMAIN   the cluster's DNS domain for that http rule (default cluster.local)
//	CONTACTS_MUSTER_HTTP_HOST one host, matched exactly, to which MUSTER_URL may be plain http
//	MUSTER_PLUGIN_TOKEN       the ONE mpl_… token `muster-server app install` showed
//	MUSTER_ORIGIN             muster's origin as browsers see it: the only page that may frame the app
//	CONTACTS_PUBLIC_URL       this plugin's own origin, e.g. https://contacts.example.test
//	CONTACTS_API_TOKEN        the contacts API's bearer, 32+ bytes (openssl rand -base64 32): the SAME value in meetings' MEETINGS_CONTACTS_TOKEN
//	CONTACTS_MEETINGS_APP_ID  optional: the Meetings app's id on muster, so a contact's meeting opens in the Meetings tab
//	CONTACTS_LISTEN           the public listener (default :8080)
//	CONTACTS_INTERNAL_LISTEN  the internal listener: /healthz and the contacts API (default :8081)
//	STATE_DIR                 where the SQLite database lives (default ./state)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/owners"
	"github.com/ZacxDev/muster-contacts/internal/server"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("muster-contacts: %v", err)
		os.Exit(1)
	}
}

// Config is everything the plugin reads from its environment.
type Config struct {
	URL            string
	Token          string
	MusterOrigin   string
	PublicURL      string
	APIToken       string
	MeetingsAppID  int64
	Listen         string
	InternalListen string
	StateDir       string
}

// MinAPITokenBytes is the shortest CONTACTS_API_TOKEN accepted, and
// MinAPITokenDistinct the fewest distinct bytes in it: 32 random bytes in
// base64 (43 characters) hold about 30; a typed or repeated value far fewer.
const (
	MinAPITokenBytes    = 32
	MinAPITokenDistinct = 16
)

// placeholderAPITokens are .env.example's value, now and before: refused by
// name, so a copied example never boots (today's is also under 32 bytes).
var placeholderAPITokens = []string{"replace-me", "replace-me-with-openssl-rand-base64-32"}

// distinctBytes counts the different bytes in s.
func distinctBytes(s string) int {
	seen := map[byte]bool{}
	for i := 0; i < len(s); i++ {
		seen[s[i]] = true
	}
	return len(seen)
}

// LoadConfig reads and checks the environment through getenv. 🔴 It never
// echoes a token's value in an error.
func LoadConfig(getenv func(string) string) (Config, error) {
	allow, err := httpAllow(getenv("CONTACTS_CLUSTER_DOMAIN"), getenv("CONTACTS_MUSTER_HTTP_HOST"))
	if err != nil {
		return Config{}, err
	}
	base, err := muster.CheckBaseURL(getenv("MUSTER_URL"), allow)
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: base, Listen: ":8080", InternalListen: ":8081", StateDir: "./state"}
	c.Token = strings.TrimSpace(getenv("MUSTER_PLUGIN_TOKEN"))
	if c.Token == "" {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN is not set: an admin installs Contacts with `muster-server app install … --link-to <meetings id>`, which shows the token once")
	}
	if strings.Contains(c.Token, ",") {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN holds a comma: it takes exactly one server-installed app's token, not a list (its value is not shown)")
	}
	if !strings.HasPrefix(c.Token, muster.TokenPrefix) || len(c.Token) <= len(muster.TokenPrefix) {
		return Config{}, fmt.Errorf("MUSTER_PLUGIN_TOKEN does not start with %q (its value is not shown)", muster.TokenPrefix)
	}
	if c.MusterOrigin, err = CheckOrigin("MUSTER_ORIGIN", getenv("MUSTER_ORIGIN")); err != nil {
		return Config{}, err
	}
	if c.PublicURL, err = CheckOrigin("CONTACTS_PUBLIC_URL", getenv("CONTACTS_PUBLIC_URL")); err != nil {
		return Config{}, err
	}
	// Framed by muster with allow-same-origin, a script on muster's own
	// origin would BE muster.
	if c.PublicURL == c.MusterOrigin {
		return Config{}, errors.New("CONTACTS_PUBLIC_URL must be its own origin (e.g. https://contacts-muster.<domain>), never MUSTER_ORIGIN")
	}
	// 🔴 The API's bearer: long enough to be unguessable, and never one of
	// the muster tokens (a leak of one must not open the other).
	c.APIToken = strings.TrimSpace(getenv("CONTACTS_API_TOKEN"))
	switch {
	case c.APIToken == "":
		return Config{}, errors.New("CONTACTS_API_TOKEN is not set: 32 or more random bytes (openssl rand -base64 32), the same value as meetings' MEETINGS_CONTACTS_TOKEN")
	case slices.Contains(placeholderAPITokens, c.APIToken):
		return Config{}, errors.New("CONTACTS_API_TOKEN is the .env.example placeholder: use openssl rand -base64 32")
	case c.APIToken == c.Token || strings.HasPrefix(c.APIToken, muster.TokenPrefix):
		return Config{}, errors.New("CONTACTS_API_TOKEN is a muster plugin token (its value is not shown): it is a separate secret that muster never sees")
	case len(c.APIToken) < MinAPITokenBytes:
		return Config{}, fmt.Errorf("CONTACTS_API_TOKEN is shorter than %d bytes (its value is not shown): use openssl rand -base64 32", MinAPITokenBytes)
	case distinctBytes(c.APIToken) < MinAPITokenDistinct:
		return Config{}, fmt.Errorf("CONTACTS_API_TOKEN has fewer than %d distinct characters: it is not random (its value is not shown): use openssl rand -base64 32", MinAPITokenDistinct)
	}
	if v := strings.TrimSpace(getenv("CONTACTS_MEETINGS_APP_ID")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, errors.New("CONTACTS_MEETINGS_APP_ID is the Meetings app's id on muster (a positive number, from `muster-server app list`)")
		}
		c.MeetingsAppID = n
	}
	if v := getenv("CONTACTS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("CONTACTS_INTERNAL_LISTEN"); v != "" {
		c.InternalListen = v
	}
	if v := getenv("STATE_DIR"); v != "" {
		c.StateDir = v
	}
	return c, nil
}

// httpAllow reads where plain http to muster is allowed besides loopback.
func httpAllow(domain, host string) (muster.HTTPAllow, error) {
	a := muster.HTTPAllow{ClusterDomain: strings.TrimSuffix(strings.TrimSpace(domain), "."), Host: strings.TrimSpace(host)}
	if a.ClusterDomain == "" {
		a.ClusterDomain = "cluster.local"
	}
	if !muster.DNSName(a.ClusterDomain) {
		return muster.HTTPAllow{}, errors.New("CONTACTS_CLUSTER_DOMAIN is not a DNS name such as cluster.local")
	}
	if a.Host != "" && !muster.DNSName(a.Host) {
		return muster.HTTPAllow{}, errors.New("CONTACTS_MUSTER_HTTP_HOST names ONE host, exactly (e.g. muster): no wildcard, port, scheme or trailing dot")
	}
	return a, nil
}

// hostPort is a URL's host: a DNS name or IPv4 address, or a bracketed IPv6
// address, with an optional port.
var hostPort = regexp.MustCompile(`^(?i:(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.?|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// CheckOrigin validates an origin variable: https, or http to a loopback
// host; one host (no wildcard: it is written into the page's CSP); no user,
// path, query or fragment. It returns scheme://host, lower-cased.
func CheckOrigin(name, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not set to an absolute https:// URL", name)
	}
	if !hostPort.MatchString(u.Host) {
		return "", fmt.Errorf("%s must name one host (letters, digits, '-' and '.', or an [IPv6] address, and a port): no wildcard and no ; , or quotes", name)
	}
	h := u.Hostname()
	loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("%s must be https:// (http:// only for localhost, 127.0.0.1 or [::1])", name)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s must be a scheme and host alone: no user, path, query or fragment", name)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func run(logger *log.Logger) error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	client, err := muster.New(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "contacts.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	app, err := owners.Boot(bootCtx, client, st, logger, time.Now)
	cancel()
	if err != nil {
		return err
	}
	logger.Print(startupLine(cfg, app.Name, app.PluginID))
	srv := server.New(server.Config{MusterOrigin: cfg.MusterOrigin, MeetingsAppID: cfg.MeetingsAppID, KitUpstream: cfg.URL,
		APIToken: cfg.APIToken, Log: logger}, st, app)

	go app.Run(ctx, owners.PollInterval)
	go sweep(ctx, st, logger)

	pub := &http.Server{Addr: cfg.Listen, Handler: srv,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	internal := &http.Server{Addr: cfg.InternalListen, Handler: srv.Internal(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 2)
	go func() { errc <- pub.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = internal.Shutdown(shut)
	return pub.Shutdown(shut)
}

// sweep drops expired sessions and old idempotency keys, every minute.
func sweep(ctx context.Context, st *store.Store, logger *log.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Sweep(ctx, time.Now()); err != nil {
				logger.Printf("sweep: %v", err)
			}
		}
	}
}

// startupLine is the first line the plugin logs: muster by scheme and host
// only (muster.Redact), and no token.
func startupLine(cfg Config, name string, pluginID int64) string {
	meetings := "no Meetings app id (meeting lines are not links)"
	if cfg.MeetingsAppID > 0 {
		meetings = "Meetings is app " + strconv.FormatInt(cfg.MeetingsAppID, 10)
	}
	return fmt.Sprintf("muster-contacts: talking to %s as server app %q (plugin %d); serving %s on %s (API on %s), framed by %s; %s",
		muster.Redact(cfg.URL), name, pluginID, cfg.PublicURL, cfg.Listen, cfg.InternalListen, cfg.MusterOrigin, meetings)
}
