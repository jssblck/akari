import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";

import { RequestError, request, useAPI } from "../api";
import { basePath, withBase } from "../base";
import { PublicShell } from "../components/public-shell";
import type { Viewer } from "../types";

export function AuthPage({ mode }: { mode: "login" | "register" }) {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const viewer = useAPI<Viewer>("/api/v1/app/bootstrap");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [invite, setInvite] = useState(params.get("invite") ?? "");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const next = safeNext(params.get("next"));

  // A signed-in visitor has nothing to do here: bounce straight to the app,
  // the same redirect the server-rendered login page used to issue.
  useEffect(() => {
    if (viewer.kind === "ready" && viewer.data.authenticated) {
      navigate("/overview", { replace: true });
    }
  }, [viewer, navigate]);

  if (viewer.kind === "ready" && viewer.data.authenticated) return null;

  return (
    <PublicShell compact>
      <div className="auth-wrap">
        <section className="auth-panel">
          <span className="brand-mark large" aria-hidden="true" />
          <h1>{mode === "login" ? "Log in to akari" : "Create an account"}</h1>
          <p>
            {mode === "login"
              ? "Open the shared history on this instance."
              : "Use the invitation issued by an instance administrator."}
          </p>
          <form
            onSubmit={async (event) => {
              event.preventDefault();
              setBusy(true);
              setError("");
              try {
                await request(`/api/v1/auth/${mode}`, {
                  method: "POST",
                  body: JSON.stringify(
                    mode === "login"
                      ? { username, password }
                      : { username, password, invite_token: invite },
                  ),
                });
                window.location.assign(next);
              } catch (problem) {
                setError(
                  problem instanceof RequestError
                    ? problem.message
                    : "The request failed.",
                );
              } finally {
                setBusy(false);
              }
            }}
          >
            <label>
              Username
              <input
                required
                autoComplete="username"
                value={username}
                onChange={(event) => setUsername(event.target.value)}
              />
            </label>
            <label>
              Password
              <input
                required
                type="password"
                autoComplete={
                  mode === "login" ? "current-password" : "new-password"
                }
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
            </label>
            {mode === "register" ? (
              <label>
                Invite token
                <input
                  required
                  value={invite}
                  onChange={(event) => setInvite(event.target.value)}
                />
              </label>
            ) : null}
            {error ? (
              <p className="form-error" role="alert">
                {error}
              </p>
            ) : null}
            <button className="button" type="submit" disabled={busy}>
              {busy ? "Working..." : mode === "login" ? "Log in" : "Register"}
            </button>
          </form>
          <p className="auth-switch">
            {mode === "login" ? (
              <>
                Have an invitation? <Link to="/register">Register</Link>
              </>
            ) : (
              <>
                Already registered? <Link to="/login">Log in</Link>
              </>
            )}
          </p>
        </section>
      </div>
    </PublicShell>
  );
}

// safeNext mirrors the server's safeNext (web.go): reject anything that is
// not a same-origin absolute path, so a crafted next cannot bounce a
// signed-in user off-site. Browsers treat a backslash as a path separator
// equivalent to "/" when resolving a URL (the WHATWG URL spec's "special
// scheme" handling), so a value like "/\evil.com" would otherwise parse as
// same-origin here while still redirecting off-site; the explicit backslash
// check closes that gap the same way the server's does.
function safeNext(value: string | null): string {
  // next is always an external (prefix-carrying) path: the server's login
  // bounce and the app shell both prefix it before handing it here, so a
  // valid value passes through untouched and only the fallback needs the
  // base put back.
  const fallback = withBase("/overview");
  if (!value?.startsWith("/") || value.startsWith("//")) return fallback;
  if (value.includes("\\")) return fallback;
  let parsed: URL;
  try {
    parsed = new URL(value, window.location.origin);
  } catch {
    return fallback;
  }
  if (parsed.origin !== window.location.origin) return fallback;
  let decodedPath: string;
  try {
    decodedPath = decodeURIComponent(parsed.pathname);
  } catch {
    return fallback;
  }
  if (decodedPath.includes("\\") || decodedPath.startsWith("//"))
    return fallback;
  // A prefixed deployment only routes paths under its mount point, so a next
  // minted before the prefix existed would land outside the proxy's mount
  // after login; send those to the fallback instead of off the app.
  if (
    basePath &&
    parsed.pathname !== basePath &&
    !parsed.pathname.startsWith(`${basePath}/`)
  )
    return fallback;
  return value;
}
