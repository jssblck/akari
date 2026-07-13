import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { RequestError, request } from "../api";
import { PublicShell } from "../components/public-shell";

export function AuthPage({ mode }: { mode: "login" | "register" }) {
  const [params] = useSearchParams();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [invite, setInvite] = useState(params.get("invite") ?? "");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const next = safeNext(params.get("next"));
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

function safeNext(value: string | null): string {
  if (!value?.startsWith("/") || value.startsWith("//")) return "/overview";
  return value;
}
