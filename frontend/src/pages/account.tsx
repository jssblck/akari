import {
  CheckIcon,
  CopyIcon,
  PlugsConnectedIcon,
  TrashIcon,
} from "@phosphor-icons/react";
import { useEffect, useRef, useState } from "react";

import { type FleetStatus, RequestError, request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { attempt, notify } from "../components/notices";
import { formatTime } from "../format";
import type { Viewer } from "../types";
import "./account.css";
import { withBase } from "../base";

type Token = {
  ID: number;
  Name: string;
  Scope: string;
  CreatedAt: string;
  LastUsedAt: string | null;
  RevokedAt: string | null;
};
type Connection = {
  ClientID: string;
  ClientName: string;
  Scope: string;
  ConnectedAt: string;
  LastUsedAt: string;
};
type Invite = {
  ID: number;
  Note: string;
  CreatedBy: string;
  CreatedAt: string;
  ExpiresAt: string | null;
  RedeemedBy: string | null;
  RedeemedAt: string | null;
};
type AccountResponse = {
  user: Viewer;
  tokens: Token[] | null;
  connections: Connection[] | null;
  invites: Invite[] | null;
  reparse: FleetStatus;
};

export function AccountPage() {
  const [revision, setRevision] = useState(0);
  const state = useAPI<AccountResponse>(
    `/api/v1/app/account?revision=${revision}`,
  );
  const refresh = () => setRevision((value) => value + 1);
  return (
    <div className="page account-page">
      <header className="page-head">
        <div>
          <h1>Account</h1>
          <p>
            Credentials, connected agents, sharing, and instance maintenance.
          </p>
        </div>
      </header>
      <AsyncView state={state}>
        {(data) => (
          <div className="account-sections">
            <TokenSection tokens={data.tokens ?? []} refresh={refresh} />
            <PublicationSection user={data.user} refresh={refresh} />
            <ConnectionSection
              connections={data.connections ?? []}
              refresh={refresh}
            />
            {data.user.is_admin ? (
              <InviteSection invites={data.invites ?? []} refresh={refresh} />
            ) : null}
            {data.user.is_admin ? (
              <ReparseSection status={data.reparse} refresh={refresh} />
            ) : null}
          </div>
        )}
      </AsyncView>
    </div>
  );
}

function TokenSection({
  tokens,
  refresh,
}: {
  tokens: Token[];
  refresh: () => void;
}) {
  const [secret, setSecret] = useState("");
  return (
    <section className="settings-section">
      <div className="settings-copy">
        <h2>API tokens</h2>
        <p>
          Ingest tokens upload sessions. Read tokens are for MCP. Full tokens
          can manage the account.
        </p>
      </div>
      <div className="settings-control">
        <form
          className="inline-form"
          onSubmit={async (event) => {
            event.preventDefault();
            const form = event.currentTarget;
            const data = new FormData(form);
            try {
              const result = await request<{ token: string }>(
                "/api/v1/tokens",
                {
                  method: "POST",
                  body: JSON.stringify({
                    name: data.get("name"),
                    scope: data.get("scope"),
                  }),
                },
              );
              setSecret(result.token);
              form.reset();
              notify("Token created", "ok");
              refresh();
            } catch (error) {
              notify(
                error instanceof RequestError
                  ? error.message
                  : "Could not create the token.",
                "err",
              );
            }
          }}
        >
          <input name="name" required placeholder="Token name" />
          <select name="scope" defaultValue="ingest">
            <option value="ingest">Ingest</option>
            <option value="read">Read</option>
            <option value="full">Full</option>
          </select>
          <button className="button" type="submit">
            Create
          </button>
        </form>
        {secret ? (
          <div className="secret">
            <code>{secret}</code>
            <button
              type="button"
              className="icon-link"
              aria-label="Copy token"
              onClick={() => navigator.clipboard.writeText(secret)}
            >
              <CopyIcon />
            </button>
            <p>Copy this token now. Akari stores only its hash.</p>
          </div>
        ) : null}
        <div className="settings-list">
          {tokens.map((token) => (
            <div
              className={
                token.RevokedAt ? "settings-row revoked" : "settings-row"
              }
              key={token.ID}
            >
              <div>
                <strong>{token.Name}</strong>
                <span>
                  {token.Scope} · created {formatTime(token.CreatedAt)}
                </span>
              </div>
              {token.RevokedAt ? (
                <span className="tag">revoked</span>
              ) : (
                <button
                  type="button"
                  className="icon-link danger"
                  aria-label={`Revoke ${token.Name}`}
                  onClick={async () => {
                    if (
                      await attempt(
                        request(`/api/v1/tokens/${token.ID}/revoke`, {
                          method: "POST",
                        }),
                        "Token revoked",
                      )
                    )
                      refresh();
                  }}
                >
                  <TrashIcon />
                </button>
              )}
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

function PublicationSection({
  user,
  refresh,
}: {
  user: Viewer;
  refresh: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const publicURL = `${window.location.origin}${withBase(`/u/${user.username}`)}`;
  return (
    <section className="settings-section">
      <div className="settings-copy">
        <h2>Public overview</h2>
        <p>
          Share aggregate usage at a stable URL. Session content stays private.
        </p>
      </div>
      <div className="settings-control">
        <div className="settings-row">
          <div>
            <strong>{user.overview_public ? "Published" : "Private"}</strong>
            {user.overview_public ? (
              <span className="share-link-row">
                <a
                  className="share-link"
                  href={publicURL}
                  target="_blank"
                  rel="noreferrer"
                >
                  {publicURL}
                </a>
                <button
                  type="button"
                  className="icon-link"
                  aria-label="Copy public link"
                  title="Copy link"
                  onClick={async () => {
                    await navigator.clipboard.writeText(publicURL);
                    setCopied(true);
                    setTimeout(() => setCopied(false), 1200);
                  }}
                >
                  {copied ? <CheckIcon /> : <CopyIcon />}
                </button>
              </span>
            ) : (
              <span>No public overview.</span>
            )}
          </div>
          <button
            className="button secondary"
            type="button"
            onClick={async () => {
              if (
                await attempt(
                  request("/api/v1/app/account/overview-publication", {
                    method: "PUT",
                    body: JSON.stringify({ published: !user.overview_public }),
                  }),
                  user.overview_public
                    ? "Overview made private"
                    : "Overview published",
                )
              )
                refresh();
            }}
          >
            {user.overview_public ? "Make private" : "Publish"}
          </button>
        </div>
      </div>
    </section>
  );
}

function ConnectionSection({
  connections,
  refresh,
}: {
  connections: Connection[];
  refresh: () => void;
}) {
  return (
    <section className="settings-section">
      <div className="settings-copy">
        <h2>Connected apps</h2>
        <p>OAuth clients currently authorized to read this account.</p>
      </div>
      <div className="settings-control settings-list">
        {connections.length === 0 ? (
          <p className="empty-inline">No connected apps.</p>
        ) : (
          connections.map((connection) => (
            <div className="settings-row" key={connection.ClientID}>
              <div>
                <strong>
                  <PlugsConnectedIcon /> {connection.ClientName}
                </strong>
                <span>
                  {connection.Scope} · last used{" "}
                  {formatTime(connection.LastUsedAt)}
                </span>
              </div>
              <button
                type="button"
                className="button secondary"
                onClick={async () => {
                  if (
                    await attempt(
                      request(
                        `/api/v1/app/account/connections/${encodeURIComponent(connection.ClientID)}`,
                        { method: "DELETE" },
                      ),
                      "App disconnected",
                    )
                  )
                    refresh();
                }}
              >
                Disconnect
              </button>
            </div>
          ))
        )}
      </div>
    </section>
  );
}

// classifyInvite mirrors the server's classifyInvite (account.templ): the
// same three states, keyed on the same field (RedeemedAt) so the label and
// the presence of the Revoke button never disagree. An invite past its
// expiry is dead the same way a redeemed one is, so it gets a status label
// instead of a button that would 404.
function classifyInvite(
  invite: Invite,
  now: Date,
): { status: string; revocable: boolean } {
  if (invite.RedeemedAt) {
    return {
      status: invite.RedeemedBy
        ? `redeemed by ${invite.RedeemedBy}`
        : "redeemed",
      revocable: false,
    };
  }
  if (invite.ExpiresAt && new Date(invite.ExpiresAt) <= now) {
    return { status: "expired", revocable: false };
  }
  return { status: "unused", revocable: true };
}

function InviteSection({
  invites,
  refresh,
}: {
  invites: Invite[];
  refresh: () => void;
}) {
  const [secret, setSecret] = useState("");
  const now = new Date();
  return (
    <section className="settings-section">
      <div className="settings-copy">
        <h2>Invitations</h2>
        <p>Issue one-time registration credentials for new accounts.</p>
      </div>
      <div className="settings-control">
        <form
          className="inline-form"
          onSubmit={async (event) => {
            event.preventDefault();
            const form = event.currentTarget;
            const data = new FormData(form);
            try {
              const result = await request<{ invite_token: string }>(
                "/api/v1/invites",
                {
                  method: "POST",
                  body: JSON.stringify({
                    note: data.get("note"),
                    expires_hours: Number(data.get("expires_hours")),
                  }),
                },
              );
              setSecret(result.invite_token);
              form.reset();
              notify("Invite created", "ok");
              refresh();
            } catch (error) {
              notify(
                error instanceof RequestError
                  ? error.message
                  : "Could not create the invite.",
                "err",
              );
            }
          }}
        >
          <input name="note" placeholder="For Grace Hopper" />
          <input
            name="expires_hours"
            type="number"
            min="0"
            defaultValue="168"
            aria-label="Expiry in hours"
          />
          <button className="button" type="submit">
            Create
          </button>
        </form>
        {secret ? (
          <div className="secret">
            <code>{secret}</code>
            <button
              type="button"
              className="icon-link"
              aria-label="Copy invite"
              onClick={() => navigator.clipboard.writeText(secret)}
            >
              <CopyIcon />
            </button>
            <p>Send this value to the invitee. It is shown once.</p>
          </div>
        ) : null}
        <div className="settings-list">
          {invites.map((invite) => {
            const { status, revocable } = classifyInvite(invite, now);
            return (
              <div className="settings-row" key={invite.ID}>
                <div>
                  <strong>{invite.Note || `Invite ${invite.ID}`}</strong>
                  <span>
                    created by {invite.CreatedBy} ·{" "}
                    {formatTime(invite.CreatedAt)}
                  </span>
                </div>
                {revocable ? (
                  <button
                    type="button"
                    className="icon-link danger"
                    aria-label="Revoke invite"
                    onClick={async () => {
                      if (
                        await attempt(
                          request(`/api/v1/app/account/invites/${invite.ID}`, {
                            method: "DELETE",
                          }),
                          "Invite revoked",
                        )
                      )
                        refresh();
                    }}
                  >
                    <TrashIcon />
                  </button>
                ) : (
                  <span className="tag">{status}</span>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </section>
  );
}

function ReparseSection({
  status,
  refresh,
}: {
  status: FleetStatus;
  refresh: () => void;
}) {
  const [live, setLive] = useState(status);
  const refreshRef = useRef(refresh);
  refreshRef.current = refresh;

  // The account payload's reparse figure is a point-in-time snapshot; adopt
  // a fresh one whenever the page reloads it (a manual refresh, or the
  // revision bump after triggering a rebuild).
  useEffect(() => setLive(status), [status]);

  useEffect(() => {
    if (!live.in_progress) return;
    const source = new EventSource(withBase("/api/v1/reparse/events"));
    const onStatus = (event: MessageEvent<string>) => {
      const next = JSON.parse(event.data) as FleetStatus;
      setLive(next);
      if (!next.in_progress) {
        source.close();
        refreshRef.current();
      }
    };
    source.addEventListener("status", onStatus as EventListener);
    return () => source.close();
  }, [live.in_progress]);

  return (
    <section className="settings-section">
      <div className="settings-copy">
        <h2>Rebuild projections</h2>
        <p>Reparse every stored session after parser or signal changes.</p>
      </div>
      <div className="settings-control">
        <div className="reparse-status">
          <div>
            <strong>
              {live.in_progress
                ? "Rebuild in progress"
                : "Projection is current"}
            </strong>
            <span>
              {live.done} / {live.total} complete
              {live.failed ? ` · ${live.failed} failed` : ""}
            </span>
          </div>
          {live.in_progress ? (
            <progress max={Math.max(live.total, 1)} value={live.done} />
          ) : (
            <button
              className="button secondary"
              type="button"
              onClick={async () => {
                if (
                  await attempt(
                    request("/api/v1/app/reparse", { method: "POST" }),
                    "Rebuild started",
                  )
                )
                  refresh();
              }}
            >
              Rebuild all
            </button>
          )}
        </div>
      </div>
    </section>
  );
}
