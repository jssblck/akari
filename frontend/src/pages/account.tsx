import { CopyIcon, PlugsConnectedIcon, TrashIcon } from "@phosphor-icons/react";
import { useState } from "react";

import { request, useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { formatTime } from "../format";
import type { Viewer } from "../types";

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
type Reparse = {
  InProgress: boolean;
  Done: number;
  Total: number;
  Failed: number;
};
type AccountResponse = {
  user: Viewer;
  tokens: Token[] | null;
  connections: Connection[] | null;
  invites: Invite[] | null;
  reparse: Reparse;
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
            const form = new FormData(event.currentTarget);
            const result = await request<{ token: string }>("/api/v1/tokens", {
              method: "POST",
              body: JSON.stringify({
                name: form.get("name"),
                scope: form.get("scope"),
              }),
            });
            setSecret(result.token);
            event.currentTarget.reset();
            refresh();
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
                    await request(`/api/v1/tokens/${token.ID}/revoke`, {
                      method: "POST",
                    });
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
              <a href={`/u/${user.username}`} target="_blank" rel="noreferrer">
                /u/{user.username}
              </a>
            ) : (
              <span>No public overview.</span>
            )}
          </div>
          <button
            className="button secondary"
            type="button"
            onClick={async () => {
              await request("/api/v1/app/account/overview-publication", {
                method: "PUT",
                body: JSON.stringify({ published: !user.overview_public }),
              });
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
                  await request(
                    `/api/v1/app/account/connections/${encodeURIComponent(connection.ClientID)}`,
                    { method: "DELETE" },
                  );
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

function InviteSection({
  invites,
  refresh,
}: {
  invites: Invite[];
  refresh: () => void;
}) {
  const [secret, setSecret] = useState("");
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
            const form = new FormData(event.currentTarget);
            const result = await request<{ invite_token: string }>(
              "/api/v1/invites",
              {
                method: "POST",
                body: JSON.stringify({
                  note: form.get("note"),
                  expires_hours: Number(form.get("expires_hours")),
                }),
              },
            );
            setSecret(result.invite_token);
            event.currentTarget.reset();
            refresh();
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
          {invites.map((invite) => (
            <div className="settings-row" key={invite.ID}>
              <div>
                <strong>{invite.Note || `Invite ${invite.ID}`}</strong>
                <span>
                  {invite.RedeemedAt
                    ? `redeemed by ${invite.RedeemedBy}`
                    : `created by ${invite.CreatedBy} · ${formatTime(invite.CreatedAt)}`}
                </span>
              </div>
              {invite.RedeemedAt ? (
                <span className="tag">used</span>
              ) : (
                <button
                  type="button"
                  className="icon-link danger"
                  aria-label="Revoke invite"
                  onClick={async () => {
                    await request(`/api/v1/app/account/invites/${invite.ID}`, {
                      method: "DELETE",
                    });
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

function ReparseSection({
  status,
  refresh,
}: {
  status: Reparse;
  refresh: () => void;
}) {
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
              {status.InProgress
                ? "Rebuild in progress"
                : "Projection is current"}
            </strong>
            <span>
              {status.Done} / {status.Total} complete
              {status.Failed ? ` · ${status.Failed} failed` : ""}
            </span>
          </div>
          {status.InProgress ? (
            <progress max={Math.max(status.Total, 1)} value={status.Done} />
          ) : (
            <button
              className="button secondary"
              type="button"
              onClick={async () => {
                await request("/api/v1/app/reparse", { method: "POST" });
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
