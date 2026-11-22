// Package owners holds the plugin's one muster token, a server-installed
// app's, and learns from muster when it may no longer act: for anyone (the
// app uninstalled, or its token rotated away) or for one person (they hid the
// app, were disabled, or signed out everywhere).
//
// muster never calls the plugin, so all of it is learned by asking, as
// meetings does. Every PollInterval:
//
//   - GET /plugin/v1/self. A 401 ends everything: every owner session is
//     deleted, and the app refuses every owner request, every /enter and
//     every API write from then on.
//   - POST /plugin/v1/owners/check for every person who has opened Contacts.
//     🔴 "revoked" (hidden, disabled, signed out everywhere), any other
//     state, or no answer for the subject ends their sessions and refuses
//     every API write for them. "expired" ends their sessions only: it
//     means they have not opened the tab in 8 hours, the normal state, and
//     cards are still kept for them. "live" or "expired" after "revoked"
//     (they showed the app again) accepts again. Only an answer changes
//     anything: a 5xx or no answer at all keeps what is held.
//
// A person who never opened Contacts is not asked about: muster answers
// "revoked" for a subject the app never redeemed, the same as for one that
// was hidden, so the answer could not tell the two apart (design, open
// question 1; decision 8 keeps their cards).
//
// Boot does the same once, before the plugin serves anyone.
package owners

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

// PollInterval is how often /self and owners/check are asked (60 s).
const PollInterval = 60 * time.Second

// Capability is the muster capability the token must hold.
const Capability = "app"

// App is the plugin's one server token and what muster said of it. It is
// safe for concurrent use.
type App struct {
	// PluginID is the app's id on muster: muster's frame names it in
	// muster_plugin, and /enter refuses any other.
	PluginID int64
	// Name is the app's name on muster.
	Name string
	// Client calls muster with the token.
	Client *muster.Client

	store *store.Store
	log   *log.Logger
	now   func() time.Time
	dead  atomic.Bool

	checkMu sync.Mutex
	resume  string
	retryAt time.Time
}

// sleep waits d, or until ctx ends: Boot waits out a Retry-After with it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrRevoked is muster's 401 for the token.
var ErrRevoked = errors.New("muster refused MUSTER_PLUGIN_TOKEN (401): the app was uninstalled, or its token rotated; every owner session was deleted")

// Boot asks /self, checks the token is a server app's with `app`, and
// judges every person who opened Contacts before the plugin serves anyone.
func Boot(ctx context.Context, c *muster.Client, st *store.Store, logger *log.Logger, now func() time.Time) (*App, error) {
	a := &App{Client: c, store: st, log: logger, now: now}
	s, err := c.Self(ctx)
	if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
		if _, err := st.EndAll(ctx, "boot: /self answered 401", now()); err != nil {
			return nil, fmt.Errorf("%w (and the sessions could not be deleted: %v)", ErrRevoked, err)
		}
		return nil, ErrRevoked
	}
	if err != nil {
		return nil, fmt.Errorf("GET /plugin/v1/self: %w", err)
	}
	if s.Scope != muster.ScopeServer || s.OwnerKind != muster.ScopeServer {
		return nil, fmt.Errorf("MUSTER_PLUGIN_TOKEN is a %q token (plugin %d), not a server-installed app's: an admin installs Contacts with `muster-server app install`", s.OwnerKind, s.PluginID)
	}
	if !slices.Contains(s.Capabilities, Capability) {
		return nil, fmt.Errorf("the server app (plugin %d) lacks the %q capability", s.PluginID, Capability)
	}
	a.PluginID, a.Name = s.PluginID, s.PluginName
	held, err := st.RedeemedSubjects(ctx)
	if err != nil {
		return nil, err
	}
	for covered := 0; covered < len(held); {
		n, wait, err := a.checkOwners(ctx)
		if errors.Is(err, ErrRevoked) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("boot: POST /plugin/v1/owners/check: %w", err)
		}
		covered += n
		if covered < len(held) && wait > 0 {
			logger.Printf("owners: boot: %d of %d people checked; muster asks to wait %s", covered, len(held), wait)
			if err := sleep(ctx, wait); err != nil {
				return nil, fmt.Errorf("boot: waiting for owners/check: %w", err)
			}
		} else if n == 0 {
			break
		}
	}
	return a, nil
}

// Live says muster has not refused the token. Once false it stays false.
func (a *App) Live() bool { return !a.dead.Load() }

// Revoke ends everything: the app refuses every request from now on, and
// every owner session is deleted.
func (a *App) Revoke(ctx context.Context, why string) {
	a.dead.Store(true)
	n, err := a.store.EndAll(ctx, why, a.now())
	if err != nil {
		a.log.Printf("owners: %s, but the sessions could not be deleted: %v", why, err)
		return
	}
	a.log.Printf("owners: %s: %d sessions deleted", why, n)
}

// checkOwners is one owners/check pass over every person who opened
// Contacts, in batches, resuming after the last subject the previous pass
// judged (so past muster's 60 calls a minute no tail is starved). It
// returns how many subjects it judged or sent, and the wait muster asked
// for. A 401 revokes the app and returns ErrRevoked.
func (a *App) checkOwners(ctx context.Context) (int, time.Duration, error) {
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	held, err := a.store.RedeemedSubjects(ctx)
	if err != nil || len(held) == 0 {
		return 0, 0, err
	}
	start := sort.SearchStrings(held, a.resume)
	if start < len(held) && held[start] == a.resume {
		start++
	}
	order := append(slices.Clone(held[start:]), held[:start]...)
	var live, revoked, expired, failures []string
	var wait time.Duration
	var first error
	sent, done := a.now(), 0
	for done < len(order) {
		batch := order[done:min(done+muster.MaxCheckSubjects, len(order))]
		states, err := a.Client.OwnersCheck(ctx, batch)
		e := muster.AsError(err)
		if e != nil && e.Kind == muster.KindAuth {
			a.checkMu.Unlock()
			a.Revoke(ctx, "owners/check answered 401")
			a.checkMu.Lock()
			return done, 0, ErrRevoked
		}
		if e != nil && e.Kind == muster.KindRateLimited {
			wait = e.RetryAfter
			a.retryAt = a.now().Add(wait)
			break
		}
		done += len(batch)
		if err != nil {
			failures = append(failures, err.Error())
			if first == nil {
				first = err
			}
			continue // only an answer changes a person
		}
		for _, s := range batch {
			switch states[s] {
			case muster.StateLive:
				live = append(live, s)
			case muster.StateExpired:
				expired = append(expired, s)
			default: // revoked, another state, or absent from the answer
				revoked = append(revoked, s)
			}
		}
	}
	if done > 0 {
		a.resume = order[done-1]
	}
	if len(live)+len(expired)+len(revoked) > 0 {
		res, err := a.store.Judge(ctx, live, expired, revoked, sent, a.now())
		if err != nil {
			return done, wait, err
		}
		if res.Sessions > 0 || res.Revoked > 0 {
			a.log.Printf("owners: %d of %d people expired, %d revoked: %d sessions deleted, %d people refused", len(expired), len(held), len(revoked), res.Sessions, res.Revoked)
		}
	}
	if wait > 0 {
		a.log.Printf("owners: owners/check: muster's limit reached after %d of %d people; resuming in %s", done, len(held), wait)
	}
	if first != nil {
		a.log.Printf("owners: POST /plugin/v1/owners/check: %s (those people are kept: only an answer changes a person)", strings.Join(failures, "; "))
		return done, wait, first
	}
	return done, wait, nil
}

// PollOnce asks /self, then owners/check. A 401 from either ends
// everything; any other failure is logged and changes nothing.
func (a *App) PollOnce(ctx context.Context) {
	if !a.Live() {
		return
	}
	_, err := a.Client.Self(ctx)
	if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
		a.Revoke(ctx, "/self answered 401: the app was uninstalled or its token rotated")
		return
	}
	if err != nil {
		a.log.Printf("owners: GET /plugin/v1/self: %v (keeping everything: only a 401 ends it)", err)
		return
	}
	a.checkMu.Lock()
	wait := a.now().Before(a.retryAt)
	a.checkMu.Unlock()
	if wait {
		return
	}
	_, _, _ = a.checkOwners(ctx)
}

// Run polls every interval until ctx ends.
func (a *App) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.PollOnce(ctx)
		}
	}
}
