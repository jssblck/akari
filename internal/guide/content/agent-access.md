# Agent access

akari serves a remote [Model Context Protocol](https://modelcontextprotocol.io)
endpoint, so a coding agent can read your whole session history without opening a
browser. It exposes the same surface the web UI shows (the overview analytics, the
projects index, the session feed, and a session's full transcript) plus the raw
data behind it: tool-call bodies from the content store, and the lossless bytes a
session was ingested from. It is **read-only** by construction; no tool creates,
changes, or deletes anything.

The endpoint is at `/mcp` on your server, over Streamable HTTP:

```
https://akari.example.com/mcp
```

## Connecting with a browser (recommended)

Connect it once from your harness. In Claude Code:

```sh
claude mcp add --transport http akari https://akari.example.com/mcp
```

On first use the harness opens your browser to akari, which recognizes the session
you are already signed in to and asks you to approve the connection. The browser
sign-in is the authentication; no credential is passed to the agent.

Behind that click is the OAuth 2.1 flow MCP defines, with akari acting as both the
resource and the authorization server. The agent registers itself, redirects
through a PKCE-protected authorization request, and exchanges the result for a
read-only access token that refreshes on its own. The token carries the **read**
scope and nothing more. You can revoke it any time from the Account page's
**Connected apps** section, which disconnects the agent and invalidates its tokens
at once.

For the flow to advertise correct URLs behind a reverse proxy, set
`AKARI_PUBLIC_URL` to the server's external origin
([Self-hosting](./self-hosting.md#configuration)).

## Connecting without a browser

A harness that cannot run the browser flow authenticates with a **read-scope API
token** instead. Create one on the account page (the `read` scope is read-only,
the counterpart of the push-only `ingest` and the read-write `full`) and pass it
as a bearer token:

```sh
claude mcp add --transport http akari https://akari.example.com/mcp \
  --header "Authorization: Bearer <read-token>"
```

A read token reaches only the MCP endpoint: it cannot push sessions or drive the
write surface. It does not expire until you revoke it.

## The tools

Every tool is read-only and sees every internal session, the same surface a
signed-in user sees. Fetch data top-down: `overview` and `list_projects` for the
lay of the land, `list_sessions` to find runs, `get_session` for a transcript,
then `read_tool_body` or `get_session_raw` to go deeper on one.

| Tool | Returns |
| --- | --- |
| `whoami` | The account the credential authenticates as: user id, username, and whether it is an admin. |
| `overview` | Fleet usage for a trailing window: cost, tokens by class, session count, a daily series, and by-model and by-agent breakdowns. Also lists the accounts present, so their ids can scope later calls. |
| `list_projects` | Every project, most recently active first, each with its session count and token and cost totals. |
| `get_project` | One project's identity, its windowed analytics (optionally narrowed by agent, user, or machine), and the agents, users, and machines that ran in it. |
| `list_sessions` | The cross-project session feed with filters and a facet rail, paged. |
| `get_session` | One session's header and a window of its transcript: messages, thinking, tool-call metadata, attachments, and subagents. |
| `read_tool_body` | A tool call's input or result body from the content store, by the hash the tool call carries. |
| `get_session_raw` | The lossless bytes a session was ingested from, behind the parsed projection. |

Parameters that govern paging through a large history:

- **Trailing windows.** `overview`, `get_project`, and `list_sessions` take
  `days`; `0` or omitted means all of history.
- **Paging the feed.** `list_sessions` returns up to 500 rows and a `next_cursor`;
  pass it back as `cursor` to walk the whole feed. It also returns a facet rail
  (busiest agents, users, machines, projects) whose values are the exact strings
  to pass back as filters. A row with an outlier field (an unusually long
  `git_branch`, say) that alone would blow the response budget is never dropped;
  its string fields are shortened in place, each with a `...[truncated]` suffix,
  and it carries `truncated: true`.
- **Paging a transcript.** `get_session` returns a bounded window of messages
  (set `include_transcript: false` for just the header). When
  `transcript.has_more` is true, pass the window's `next_after` as
  `transcript_after` to fetch the next page. `byte_budget_truncated` reports
  that the encoded response limit ended the page before `transcript_limit`.
  If one message field cannot fit, the message carries a preview, its stored
  byte length, and an `akari://` resource link. Reading that resource returns
  the full text through the same authenticated MCP connection. Revoking the
  connection or API token also revokes access to previously returned links.
- **Fetching bodies.** Tool bodies are not inlined in `get_session`; take the
  `input_sha256` or `result_sha256` off a tool call and pass it, with the
  `session_id` that references it, to `read_tool_body`. Text returns as text,
  binary as base64, capped by `max_bytes` and the server's encoded response
  budget.

Tool results carry the complete DTO in `structuredContent`. The text content is
a compact status and paging summary, so clients do not receive a second copy of
the full JSON payload.

## What the MCP sees

The MCP surface mirrors the web UI: the same sessions, the same visibility rule
(every internal session, exactly what a signed-in user sees), plus `get_session_raw`
for the ingested bytes, which the web UI does not surface. It exposes no account or
token management and no way to publish, delete, or write; those stay on the
full-scope web surface.

---

Next: [Self-hosting](./self-hosting.md) -> run the server yourself.
