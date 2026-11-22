# muster-contacts

A [muster](https://github.com/ZacxDev/muster) plugin that keeps **the people
you meet and the meetings you met them in**. It adds a **Contacts** tab to
muster's sidebar: a list you can search, each person's emails, a note, the
meetings you met in, and where the contact came from. People arrive on their
own: a guest who did not know you fills in a short card after a meeting in
[muster-meetings](https://github.com/ZacxDev/muster-meetings), and becomes a
contact marked **Added by them**. You can add, edit, merge, download and
delete anyone.

This is **phase 1** of muster's design
[`docs/design/contacts.md`](https://github.com/ZacxDev/muster/blob/main/docs/design/contacts.md):
the store, the Contacts tab, and the API the meetings plugin sends a guest's
card to (a card links its contact to the meeting: "met at meeting X"). Phase 2 (scheduled meetings with emailed invites) and
phase 3 (WhatsApp, Discord, Signal and email handles, then messages) add their
own tables and values as later migrations. Receipts (recognising a returning
guest) are phase 1c, minted and kept by meetings in the guest's browser.

## How it fits muster

- **A server app, installed linked to Meetings.** An admin installs it once
  for the whole muster; every person gets the tab. It is installed **linked**
  to the Meetings app (`--link-to`, below), so muster gives both apps the
  same subject for each person: a card Meetings sends for "the owner of this
  meeting" lands in that person's Contacts. The plugin needs nothing else for
  the link; it receives subjects from muster like any server app. Every
  unlinked app keeps muster's pairwise subjects.
- **muster never calls the plugin.** It calls muster for the hand-off
  (`POST /plugin/v1/handoffs/redeem`), `GET /plugin/v1/self` and
  `POST /plugin/v1/owners/check`, every 60 s, exactly as meetings does.
- **Meetings calls Contacts; Contacts never calls Meetings.** The contacts
  API is on the internal listener (`:8081`), reachable only from the meetings
  plugin's pods, behind a token that is not a muster token and that muster
  never sees.
- **Per owner.** Nobody sees anyone else's contacts. Every row that holds a
  person's data carries the owner's subject, and every statement filters on
  it.

```
 owner's browser (muster's frame)          guest's browser (meetings' page)
        │ owner API (bearer mco_…)                  │ the card
        ▼                                           ▼
 ┌──────────────────┐   internal API, bearer   ┌──────────────────┐
 │ muster-contacts  │◄─────────────────────────│ muster-meetings  │
 │  :8080 the tab   │   CONTACTS_API_TOKEN     │                  │
 │  :8081 the API   │                          │                  │
 └────────┬─────────┘                          └────────┬─────────┘
          │ redeem, /self, owners/check                 │ redeem, /self, owners/check, …
          ▼                                             ▼
 ┌───────────────────────────────────────────────────────────────┐
 │ muster-server: two server apps, one linked pair               │
 └───────────────────────────────────────────────────────────────┘
```

## Install

Contacts needs muster with linked server apps (`app install … --link-to`)
and the Meetings app already installed.

1. **Run the plugin** on its own one-label host beside muster
   (`contacts-<x>.<domain>`; never muster's own origin). See
   [Deploying](#deploying).
2. **Add its origin** to muster's `MUSTER_PLUGIN_ORIGINS`.
3. **Install it linked to Meetings** (find Meetings' id with
   `muster-server app list`):

   ```sh
   muster-server app install --name contacts --label Contacts --icon book \
     --origin https://contacts-<x>.<domain> --entry https://contacts-<x>.<domain>/enter \
     --link-to <meetings app id>
   ```

   It prints the app's `mpl_…` token once: it goes in the plugin's Secret as
   `MUSTER_PLUGIN_TOKEN`. The pair exists the moment Contacts does, so nobody
   can open it unlinked, and Meetings keeps every subject it has.
4. **Make the API token** and give the same value to both plugins:

   ```sh
   openssl rand -base64 32   # CONTACTS_API_TOKEN here, MEETINGS_CONTACTS_TOKEN in meetings
   ```

5. **Point Meetings at Contacts** (meetings' phase 1b): its
   `MEETINGS_CONTACTS_URL` is this plugin's internal listener, e.g.
   `http://muster-contacts.<namespace>.svc.cluster.local.:8081`. With it
   unset, Meetings behaves exactly as without Contacts.

Open the tab; add, edit and delete a contact by hand to check it.

## Configuration

Environment only (see [`.env.example`](.env.example)):

| variable | |
|---|---|
| `MUSTER_URL` | where the plugin calls muster: `https://`, or plain `http://` only to loopback, a fully-qualified Service name with its trailing dot (`muster.<ns>.svc.cluster.local.`), or `CONTACTS_MUSTER_HTTP_HOST` |
| `CONTACTS_CLUSTER_DOMAIN` | the cluster's DNS domain for that rule (default `cluster.local`) |
| `CONTACTS_MUSTER_HTTP_HOST` | one host, matched exactly, to which `MUSTER_URL` may be plain http |
| `MUSTER_PLUGIN_TOKEN` | the ONE `mpl_…` token `app install` printed (a comma, a list, is refused) |
| `MUSTER_ORIGIN` | muster's origin as browsers see it: the only page allowed to frame the tab |
| `CONTACTS_PUBLIC_URL` | this plugin's own origin; refused if it equals `MUSTER_ORIGIN` |
| `CONTACTS_API_TOKEN` | the contacts API's bearer: **32 bytes or more**, and never a muster token (`mpl_…`); the plugin refuses to start otherwise, without echoing the value |
| `CONTACTS_MEETINGS_APP_ID` | optional: the Meetings app's id on muster, so a contact's meeting line opens `/apps/<id>?ref=<meeting>` (in a new tab: muster's frame may open a popup, not navigate muster's own window) |
| `CONTACTS_LISTEN` | the public listener (default `:8080`) |
| `CONTACTS_INTERNAL_LISTEN` | the internal listener: `/healthz` and the contacts API (default `:8081`) |
| `STATE_DIR` | where `contacts.db` lives (default `./state`) |

The Kubernetes base reads the non-secret values from the
`muster-contacts-params` ConfigMap (`muster-url`, `cluster-domain`,
`muster-origin`, `public-url`, `meetings-app-id`) and the two tokens from the
`muster-contacts` Secret.

## The Contacts tab

- **List:** search (by name or email, on the server, as you type), **Add**
  (`plus`), **Settings** (`more`). Newest activity first; a chip only when it
  says something (**Added by them**).
- **A contact:** a sheet (a bottom sheet on a phone, a dialog above 640 px)
  with the emails (each with **Copy**; a self-submitted one says **not
  confirmed**), the note, **Met** (each meeting, with **email not confirmed**
  on a line that came from a card), and where the contact came from. Icon
  buttons: **Edit**, **Download**, **Delete**.
- **Edit** is the same sheet: name, emails, note; **Save**, and Escape
  cancels. An email another contact holds offers **Merge**.
- **Delete** asks first, and says that Meetings keeps its own record of the
  calls.
- **Settings:** **Add people from Meetings** (on: cards are kept), **Download
  all** (JSON), **Delete everything** (type "delete everything").

The page links muster's [UI kit](https://github.com/ZacxDev/muster/blob/main/docs/ui-kit.md)
served from the plugin's own origin at `/kit/v1/` (fetched from muster
server-side, with the last copy, then the bundled kit, when muster cannot be
reached), and follows its theme. Every colour is a kit token. Icons the kit
does not have yet (person, pencil, bin, download, envelope, calendar) are a
small sprite on the kit's grid. Motion uses the kit's duration tokens, so
`prefers-reduced-motion` stops all of it. Every script and stylesheet URL
carries its content hash (`?v=`), and every `/static/` answer is `no-cache`
with a strong ETag, so a deploy is a new URL and a revalidation is a `304`.

The session lives in the page's memory only (`Authorization: Bearer mco_…`):
no cookie. `/enter` refuses unless the browser says it is loading a frame
(`Sec-Fetch-Dest: iframe`), before anything is redeemed.

## The contacts API

On the internal listener only. The contract is data:
[`contactsapi/testdata/contract/routes.json`](contactsapi/testdata/contract/routes.json)
(every route; each case a list of requests, each with its exact status and
body) and
[`contactsapi/testdata/contract/fields.json`](contactsapi/testdata/contract/fields.json)
(the card's name and email rules, each input with its literal answer). The Go
package [`contactsapi`](contactsapi) carries the types, the field checks
(`CleanName`, `CheckEmail`), and `RunContract`, which runs the table against
a running server. This repository runs it against its real server; the
meetings plugin imports `github.com/ZacxDev/muster-contacts/contactsapi` at a
pinned version and runs the same table, so a change on either side that
breaks the other is red in both.

Every request carries `Authorization: Bearer <CONTACTS_API_TOKEN>` (compared
in constant time; missing or wrong: `401 {"error": "unauthorised"}`). Bodies
are JSON, at most 4 KiB (`413`), and an unknown field is `400`. Every write
carries an `Idempotency-Key`: the same key and body answer again and change
nothing; the same key and another body is `409`. Keys are kept 7 days.

| route | body → answer |
|---|---|
| `GET /v1/owners/{owner}` | `200 {"accepting": true}` or `404 {"error": "not accepting"}`: may meetings offer the card? |
| `POST /v1/submissions` | `{owner, card_id, meeting: {id, title, started_at, ended_at}, participant: {id, identity, typed_name, joined_at, left_at}, name, email}`, key `card:<card_id>` → `202`, no body |

- **`owner`** is the subject meetings holds for the person, which is this
  plugin's subject for them too (the linked pair). A malformed one is `400`.
- **Accepting:** a person who has never opened Contacts **is** accepting
  (their cards are kept from their very first meeting). A person muster
  answers `revoked` (hid the app, disabled, signed out everywhere) is not;
  `expired` (has not opened the tab in 8 hours, the normal state) is. **Add
  people from Meetings** off is not accepting; nor is anyone once the app is
  uninstalled.
- 🔴 **A submission answers the same `202` with no body whether it made a
  contact, merged into one, or was dropped** (not accepting, or the app
  uninstalled), so nothing a guest's card produces can tell whether an email
  was already a contact.
- **Limits:** 200 cards per owner and 2,000 per install in any 24 hours;
  past either, `429` with `Retry-After`. A replay is not counted.
- **Fields:** the name is meetings' knock rule (1 to 40 characters, no control
  or text-direction characters, something visible); the email is one
  address: trimmed, at most 254 bytes, exactly one `@`, a local part of 1 to 64
  with no space, angle bracket, comma, semicolon or quote, and a domain of DNS
  labels with a dot, converted by IDNA. A bad field is
  `400 {"error": "invalid field", "field": "<path>"}`.

### Merge rules

The email key (the address trimmed, NFC, local part lower-cased, domain in
lower-case ASCII by IDNA) is the only match key; plus-addressing and dots are
**not** folded. A card naming a known key links the meeting to that contact
(and says "Also sent their details after *meeting*"); it never creates a
second contact. A name the owner typed is never overwritten by a card (the
card's goes in `said_name`); a name that came from a card takes a newer
card's. One contact per email per owner is a constraint of the table, not a
lookup. A merge by hand moves every email, meeting link, idempotency key and
provenance line onto the contact kept, in one transaction.

## Privacy

- **This is personal data in a public-code plugin.** The code is public; the
  data never is. Fixtures use `example.test` addresses and invented names.
- **What is stored** per contact: a name, emails, a note, and links to the
  meetings by meetings' ids with the name typed at the knock. No IP address,
  no user agent, no transcript, no meeting without a linked contact. Of the
  owner: their subject and display name, never a muster user id or an email
  (the plugin is never told either).
- 🔴 **No PII in logs.** Log lines carry the method, the route **pattern**
  (never the path or the query), the status, counts and event names; never a
  name, email, title, note, search, token, code or subject. A test
  drives every route with distinctive fixtures and fails if one reaches the
  log (with a positive control that a fixture logged on purpose is seen).
- **Delete really deletes.** A contact's delete removes its row and every
  row hanging from it (emails, provenance, meeting links,
  idempotency keys) and every meeting row no contact is left on; the database
  runs with `secure_delete` and checkpoints the WAL after a delete, so the
  bytes are not left on disk. **Delete everything** does it for every contact
  of the owner. Meetings' own records of the calls are meetings', and follow
  meetings' retention.
- **Backups:** if the deployment backs up the volume, a deleted contact
  survives in backups until they expire: the backup retention is the outer
  bound of "deleted".
- **Export:** **Download** on a contact gives every row about them (JSON, the
  API's shape), so the owner can answer a person's request for their data.
- **Uninstall** in muster makes the data unreachable but does not delete the
  file: delete the volume (`muster-contacts-state`).
- **First deployment only.** Per muster's decision 3, this ships to the
  operator's own deployment; a deployment whose guests may be a client's
  people waits for an explicit decision (the design's "Before a second
  deployment").

## Deploying

The image is `ghcr.io/zacxdev/muster-contacts`, built by
[`image.yml`](.github/workflows/image.yml) as `:sha-<commit>` and `:main` only
after `ci` passed on that exact commit on `main` (and `:<version>` on a `v*`
tag). Pin a `sha-<commit>` tag or a digest; never run `:main`.

[`deploy/kubernetes`](deploy/kubernetes) is a base: one replica, `Recreate`
(one SQLite writer), nonroot, read-only root filesystem, every capability
dropped, no service account token; SQLite on a PVC and its temporary files on
an emptyDir; probes on the internal `/healthz`. Its NetworkPolicy is
default-deny: `:8080` from the ingress controller's namespace, `:8081` from
the meetings plugin's pods alone, and out only to muster and DNS.
[`deploy/example-overlay`](deploy/example-overlay) shows every value a tenant
sets. Create the Secret first:

```sh
kubectl -n <muster namespace> create secret generic muster-contacts \
  --from-literal=MUSTER_PLUGIN_TOKEN=<the mpl_… token> \
  --from-literal=CONTACTS_API_TOKEN="$(openssl rand -base64 32)"
```

Meetings' own NetworkPolicy must admit its egress to this plugin's `:8081`.

## Rollback

- **Meetings:** unset `MEETINGS_CONTACTS_URL` and `MEETINGS_CONTACTS_TOKEN`
  and redeploy it: it behaves as without Contacts.
- **Contacts:** hide or uninstall the app in muster; the data stays on its
  volume until you delete it. A meetings plugin still configured finds
  Contacts not accepting or unreachable and offers no card.
- **The link:** `muster-server app unlink <id>` stops people who first open
  either app from then on sharing a subject; it re-keys nobody.
- **The plugin version:** pin the previous `sha-<commit>` image. The schema
  has one migration; a newer database refuses to open with an older build.

## Tests

```sh
go test -race ./...            # unit, store, contract, owner API, logs, limits, assets, deploy
go run ./scripts/mutate         # every guard's mutant must turn its named test red
cd e2e && npm ci && npx playwright test   # the tab, at 390x844 and 1280x800
go run ./scripts/mutate -e2e    # the UI mutants, each killed by a Playwright test
```

CI (`ci.yml`) runs all four. The sweep applies one mutant at a time, runs
the ONE test named to kill it, and counts it only when that test fails with
the mutant's own message; a known-caught mutant and a no-op edit are its
controls. On a workstation whose package manager ships Chromium, set
`PLAYWRIGHT_CHROMIUM` to its binary. Deploy tests render with `kubectl`
(`CONTACTS_REQUIRE_KUBECTL=1` makes them fail instead of skip).

## Layout

| path | what |
|---|---|
| `main.go` | configuration and the two listeners |
| `contactsapi/` | the API's types, field rules and contract table (importable by meetings) |
| `internal/store/` | SQLite: schema, contacts, merges, deletes, the API's writes |
| `internal/owners/` | the muster token: `/self` and `owners/check` |
| `internal/server/` | `/enter`, the owner API, the contacts API, the kit proxy, versioned assets, the page |
| `internal/muster/`, `internal/mustertest/` | muster's plugin protocol, and a fake muster for tests |
| `scripts/e2e-harness/`, `e2e/` | the browser tests' harness and Playwright specs |
| `scripts/mutate/` | the mutation sweep |
| `deploy/` | the Kubernetes base and an example overlay |

## Licence

MIT (see [LICENSE](LICENSE)).
