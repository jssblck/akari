# Accounts and sharing

akari's authorization is deliberately flat. Signed in to a server, you see every
session on it; there is no per-user wall to reason about. What this chapter covers
is the thin layer around that: how accounts are created, the three token scopes
that grant machine access, and how you deliberately share something outward.

## Accounts and invites

Membership is closed and invite-gated, with one exception that bootstraps a fresh
server:

- **The first account is the admin.** The very first account registered on a new
  server needs no invite and becomes an admin. This is how a new server gets its
  first user.
- **Every later account needs an invite.** Registration otherwise requires a valid,
  unredeemed invite token. An admin mints one from the Account page and hands it to
  the new teammate, who enters it when registering. An invalid, already-redeemed,
  or expired token is rejected.
- **Admins** can mint invites, force a [reparse](./self-hosting.md#reparse), and
  delete any session. A normal user manages only their own sessions and tokens.

You sign in with a username and password; a successful login sets an
`HttpOnly`, `SameSite=Lax` session cookie (marked `Secure` unless the server is
running in plain-HTTP development mode). Only a hash of the session secret is
stored, the same discipline as tokens and invites. Logging out deletes the session
and clears the cookie.

Password hashing and verification use a bounded process-wide worker pool. Invalid
logins follow the same response and Argon2 path whether the username is unknown,
federated, or paired with the wrong password. A full worker queue or an expired
queue wait also fails as invalid credentials. Registration uses the same worker
pool and returns a retryable unavailable response when it cannot admit password
work.

## API tokens

Beyond the browser session, akari issues **API tokens**: bearer credentials for
machines. Every token has one of three **scopes**, and the scope alone determines
what the token can do. Create and revoke them from the Account page; the plaintext is
shown once, at creation, then only its metadata (name, scope, last used) is
visible.

| Capability | `ingest` | `read` | `full` |
| --- | --- | --- | --- |
| Push sessions and blobs | yes | no | yes |
| Read the web UI | no | no | yes |
| Reach the [MCP](./agent-access.md) endpoint | no | yes | yes |
| Publish / unpublish / delete, mint tokens | no | no | yes |

The intent behind each:

- **`ingest`** is push-only. It is what the [client](./the-client.md) uses: it can
  upload sessions and their blobs and nothing else, so it is safe to leave on a
  laptop or bake into a deployment.
- **`read`** is read-only. It sees everything a signed-in user sees but can mutate
  nothing (no publish, delete, or token creation), which is exactly what you want
  to hand an untrusted coding agent. It is the scope the OAuth connect flow
  issues. A full-scope token also reaches the MCP endpoint, but it carries the
  whole write surface with it, so `read` is the one to hand out.
- **`full`** is read and write: the browser session's level of access, as a token.
  Treat it as a real credential; it is rarely the right thing to hand a third
  party.

A useful consequence: the server-rendered UI requires a full-scope credential, so
pointing an ingest- or read-scope token at a browser page just bounces to the
login screen.

## Session visibility

A session's visibility is **internal** by default: any signed-in user of the
server can read it. There is no state below that; on one server, signed in means
you see everything.

### Sharing a session

From a session's page, its owner can:

- **Publish** it, which mints an unguessable public id and serves the session at
  `/s/<public-id>` to logged-out viewers. The public page never exposes the
  numeric session id, and it links only to subagents that are themselves
  published, so publishing a parent does not leak its children.
- **Unpublish** it, which clears the link at once; the old URL stops resolving.
- **Delete** it (owner or an admin). Deleting cascades the transcript and the raw
  bytes; any content-addressed blobs it referenced are reclaimed by the next
  background sweep, unless another session still points at them.

Published or not, a tool body is always fetched through a session that references
it, so a public link exposes only that session's own bodies, never an internal
one that happens to share a hash.

A published session also unfurls with an Open Graph preview card at
`/s/<public-id>/og.png`: the session's title, an activity strip that plots the
session's own usage over its span (the session card's take on the overview heatmap,
each cell a slice of the run's length), and four foot figures (total tokens, message
count, its quality grade, and its duration), rendered in the house style as a pure-Go
PNG. A figure with nothing to show (an unscored session's grade, or an undated
session's duration) reads as a dash rather than a misleading zero. Like the overview
card below it is rendered on demand the first time it is fetched and served from cache
until the TTL expires.

### Sharing a project's overview

Any signed-in user can publish a **project's usage overview** at `/p/<id>` from the
project's page. Projects are shared across the whole server rather than owned, so
there is no per-owner gate: the public page is the same aggregate panel and quality
band the signed-in project page shows (totals, the activity heatmap, the by-model and
by-agent breakdowns, and the grades/outcomes/tools band) scoped to that one repo. It
lists no sessions and names no accounts, so it shares the repo's usage shape without
exposing a session or which people ran in it. The address is the project id, so
unpublishing hides the page without changing the link.

Like the user overview, a published project overview gets an Open Graph preview card
at `/p/<id>/og.png`, the same simplified heatmap in the house style, with three foot
figures: total tokens, session count, and a single representative quality grade (the
mean score across the project's graded sessions, rounded to a letter). The grade reads
as a dash when no session in scope is scored.

### Sharing your usage overview

From the Account page's Publicity control you can publish your own **usage
overview** at `/u/<username>`. The public page is the same aggregate panel you see
(totals, the activity heatmap, the by-model and by-agent breakdowns) scoped to your
account alone: it carries no session links and no one else's numbers, so it shares
your usage shape without exposing any session or any other user. The address is
your username, so unpublishing hides the page without changing the link, and
re-publishing brings the same URL back.

A published overview also gets an Open Graph preview card at `/u/<username>/og.png`,
so a shared link unfurls with an image: a simplified copy of your activity heatmap
plus the headline figures, rendered in the house style as a pure-Go PNG (no
headless browser). All three per-entity cards (this one, the project overview's, and
a session's) work the same way: rendered on demand the first time the card is fetched
(typically when a share unfurls), then served from cache until the TTL expires
([Self-hosting](./self-hosting.md#configuration) documents it, an hour by default);
the next fetch after that renders a fresh one. A background sweep prunes expired
cards. A card may trail the live page by up to the TTL. The overview and project
cards represent the default trailing-year window, so a link viewed under a narrower
range carries no image rather than a mismatched one.

## Connected agents

When you connect a coding agent over MCP, that connection appears on the Account
page under **Connected apps**, with when it connected and when it was last used.
Disconnecting one revokes every token it holds at once. The connection itself is a
read-only OAuth grant; the mechanics are [Agent access](./agent-access.md).

---

Next: [Agent access](./agent-access.md) -> reading your history from a coding
agent over MCP.
