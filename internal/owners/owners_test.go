package owners

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
	"github.com/ZacxDev/muster-contacts/internal/muster"
	"github.com/ZacxDev/muster-contacts/internal/mustertest"
	"github.com/ZacxDev/muster-contacts/internal/store"
)

var (
	ctx   = context.Background()
	quiet = log.New(io.Discard, "", 0)
	app   = mustertest.App{Token: "mpl_contacts_owners_test_51d2", PluginID: 21, PluginName: "contacts", Capabilities: []string{"app"}}
	ada   = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
)

func setup(t *testing.T) (*mustertest.Fake, *store.Store, *muster.Client) {
	t.Helper()
	f := mustertest.New()
	t.Cleanup(f.Close)
	f.Install(app)
	f.AddPerson(ada)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _ := muster.New(f.URL, app.Token)
	return f, st, c
}

// opened is ada opening Contacts: a redeem and a session, as /enter does.
func opened(t *testing.T, f *mustertest.Fake, st *store.Store, c *muster.Client) string {
	t.Helper()
	red, err := c.Redeem(ctx, f.MintApp(app.PluginID, ada.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(ctx, red.Owner.Subject, red.Owner.DisplayName, time.Now()); err != nil {
		t.Fatal(err)
	}
	return red.Owner.Subject
}

func TestBootRefusesATokenThatIsNotAServerAppsWithApp(t *testing.T) {
	f, st, _ := setup(t)
	f.Install(mustertest.App{Token: "mpl_no_app_capability_000", PluginID: 30, Capabilities: []string{"tasks"}})
	c, _ := muster.New(f.URL, "mpl_no_app_capability_000")
	if _, err := Boot(ctx, c, st, quiet, time.Now); err == nil || !strings.Contains(err.Error(), `lacks the "app" capability`) {
		t.Fatalf("a token without app: %v", err)
	}
	f.AddPersonal(mustertest.Personal{Token: "mpl_personal_token_00000", PluginID: 31, OwnerID: ada.ID, Capabilities: []string{"app"}})
	c, _ = muster.New(f.URL, "mpl_personal_token_00000")
	if _, err := Boot(ctx, c, st, quiet, time.Now); err == nil || !strings.Contains(err.Error(), "not a server-installed app's") {
		t.Fatalf("a personal token: %v", err)
	}
	c, _ = muster.New(f.URL, "mpl_unknown_token_000000")
	if _, err := Boot(ctx, c, st, quiet, time.Now); !errors.Is(err, ErrRevoked) {
		t.Fatalf("an unknown token: %v, want ErrRevoked", err)
	}
}

// 🔴 Fail closed: a subject absent from an answered batch is revoked.
func TestASubjectMissingFromTheAnswerIsRevoked(t *testing.T) {
	f, st, c := setup(t)
	s := opened(t, f, st, c)
	a, err := Boot(ctx, c, st, quiet, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.SetOwnersCheck(0, `{"states": {}}`, nil)
	a.PollOnce(ctx)
	if got, _ := st.StandingOf(ctx, s); got.Accepting {
		t.Fatalf("a subject absent from the answer is still accepting: %+v", got)
	}
}

// Only an answer changes a person: a failing owners/check keeps them.
func TestNoAnswerChangesNothing(t *testing.T) {
	f, st, c := setup(t)
	s := opened(t, f, st, c)
	a, err := Boot(ctx, c, st, quiet, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.Hide(app.PluginID, ada.ID)
	f.SetFailOwnersCheck(true)
	a.PollOnce(ctx)
	if got, _ := st.StandingOf(ctx, s); !got.Accepting {
		t.Fatalf("a 503 refused the person: %+v", got)
	}
	f.SetFailOwnersCheck(false)
	a.PollOnce(ctx)
	if got, _ := st.StandingOf(ctx, s); got.Accepting {
		t.Fatalf("the hide, answered, did not refuse: %+v", got)
	}
}

// A person who never opened Contacts (their card arrived through the pair)
// is not asked about: muster would answer "revoked" for a subject the app
// never redeemed (decision 8 keeps their cards).
func TestANeverOpenedOwnerIsNotAskedAbout(t *testing.T) {
	f, st, c := setup(t)
	const never = "NNNNneverNNNNNNNNNNNNN"
	o, _ := st.ForOwner(never)
	addr, key, _ := contactsapi.CheckEmail("grace@example.test")
	if _, err := o.Submit(ctx, store.Card{Key: "card:x", RequestHash: "h", Name: "Grace", Address: addr, AddressKey: key,
		Meeting: contactsapi.Meeting{ID: "m1"}, Participant: contactsapi.Participant{ID: "p1", Identity: "guest-p1", TypedName: "Grace"}}, func() error { return nil }, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := opened(t, f, st, c) // a redeemed one, as the control
	a, err := Boot(ctx, c, st, quiet, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	a.PollOnce(ctx)
	checked := f.Checked()
	if checked[never] != 0 || checked[s] == 0 {
		t.Fatalf("owners/check asked %v: want the redeemed subject and not the never-opened one", checked)
	}
	if got, _ := st.StandingOf(ctx, never); !got.Accepting {
		t.Fatalf("the never-opened owner was refused: %+v", got)
	}
}

// A 401 on the poll ends everything: the app is dead.
func TestA401EndsTheApp(t *testing.T) {
	f, st, c := setup(t)
	opened(t, f, st, c)
	a, err := Boot(ctx, c, st, quiet, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.Uninstall(app.PluginID)
	a.PollOnce(ctx)
	if a.Live() {
		t.Fatal("the app is live after a 401")
	}
}
