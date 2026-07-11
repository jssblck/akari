# Security Policy

akari stores raw coding-agent session logs, which routinely contain private
source code, file contents, and secrets that surfaced in an agent's transcript.
The server authenticates browser and client access and mints unguessable public
links for sessions that an owner chooses to publish. Please report suspected
security issues privately instead of opening a public issue.

Report vulnerabilities through GitHub private vulnerability reporting if it is
available for this repository, or email `security@jessica.black`.

Useful reports include: a way to read an internal (unpublished) session without
authenticating, or to reach a session you are not entitled to; predictable or
enumerable `public_id` values that expose published sessions that were meant to
stay obscure, or a published link that keeps serving after unpublish; mishandling
of the password login, the session cookie, or API tokens (fixation, leakage, or
missing `Secure`/`HttpOnly` where it matters); flaws in the remote MCP server's
OAuth flow that grant a client more access than its token should carry; unsafe
handling of uploaded session bytes on the ingest path; and dependency
vulnerabilities.

There is no bug bounty program. Reports are still appreciated, and responsible
disclosure helps keep real deployments safer.

Only the current main development line is supported.

## Threat model

akari is self-hosted and assumes a small, trusted set of authenticated users.
Authorization is deliberately flat: any authenticated user sees every internal
session on the instance. That is a design choice, not a bug, so "one user can
read another user's internal session" is expected behavior and not a
vulnerability. Operators who need isolation between users should run separate
instances.

The boundary akari does defend is the logged-out boundary. An unauthenticated
request must reach only sessions that an owner has explicitly published, and only
through the unguessable `public_id` minted at publish time; unpublishing must
make that link stop working. A way to cross that boundary (reading internal
sessions without auth, or guessing a public link) is a real vulnerability and
worth reporting.

Browser mutations also enforce the instance's public origin. Login,
registration, session-cookie writes, and trusted-proxy-authenticated writes use
`Origin` and Fetch Metadata, with a double-submit token fallback when those
headers are absent. Bearer-authenticated API, ingest, and MCP calls remain
separate from the browser CSRF mechanism. Operators should set
`AKARI_PUBLIC_URL` and follow the reverse-proxy header rules in the self-hosting
guide.

akari does not attempt to defend a reviewer or server against malicious content
inside the session logs it ingests: transcripts are treated as untrusted data to
store and render safely (escaped, never executed), but the uploading client is
assumed to be an aligned user pushing their own agent's logs.

This is experimental software and has not had a professional security audit. All
usage is at your own risk.
