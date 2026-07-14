import { PlugsConnectedIcon } from "@phosphor-icons/react";
import { useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { RequestError, useAPI } from "../api";
import { withBase } from "../base";
import { AsyncView } from "../components/async-view";
import { PublicShell } from "../components/public-shell";

type Consent = {
  client_name: string;
  username: string;
  client_id: string;
  redirect_uri: string;
  state: string;
  code_challenge: string;
  resource: string;
  csrf: string;
  app_csrf: string;
};

export function OAuthConsentPage() {
  const location = useLocation();
  const navigate = useNavigate();
  const state = useAPI<Consent>(
    `/api/v1/app/oauth/authorize${location.search}`,
  );
  const loggedOut =
    state.kind === "error" &&
    state.error instanceof RequestError &&
    state.error.status === 401;

  // A logged-out visitor gets bounced through login rather than a generic
  // error card; next carries the full authorize URL (path plus every OAuth
  // query param) so approving lands back on this exact consent request.
  useEffect(() => {
    if (!loggedOut) return;
    const authorizeURL = location.pathname + location.search;
    navigate(`/login?next=${encodeURIComponent(authorizeURL)}`, {
      replace: true,
    });
  }, [loggedOut, location.pathname, location.search, navigate]);

  if (loggedOut) return null;

  return (
    <PublicShell compact>
      <div className="auth-wrap">
        <AsyncView state={state}>
          {(consent) => (
            <section className="auth-panel consent-panel">
              <PlugsConnectedIcon
                className="consent-icon"
                size={32}
                color="var(--lilac)"
              />
              <h1>Connect {consent.client_name}</h1>
              <p>
                Signed in as <strong>{consent.username}</strong>. This client
                will be able to read projects, sessions, transcripts, and usage
                analytics.
              </p>
              <form method="post" action={withBase("/oauth/authorize")}>
                <input type="hidden" name="_csrf" value={consent.app_csrf} />
                <input type="hidden" name="csrf" value={consent.csrf} />
                <input
                  type="hidden"
                  name="client_id"
                  value={consent.client_id}
                />
                <input
                  type="hidden"
                  name="redirect_uri"
                  value={consent.redirect_uri}
                />
                <input type="hidden" name="state" value={consent.state} />
                <input
                  type="hidden"
                  name="code_challenge"
                  value={consent.code_challenge}
                />
                <input type="hidden" name="resource" value={consent.resource} />
                <div className="consent-actions">
                  <button
                    className="button"
                    type="submit"
                    name="decision"
                    value="approve"
                  >
                    Connect
                  </button>
                  <button
                    className="button secondary"
                    type="submit"
                    name="decision"
                    value="deny"
                  >
                    Deny
                  </button>
                </div>
              </form>
            </section>
          )}
        </AsyncView>
      </div>
    </PublicShell>
  );
}
