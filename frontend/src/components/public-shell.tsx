import { GithubLogoIcon } from "@phosphor-icons/react";
import type { ReactNode } from "react";

import { useAPI } from "../api";
import type { Viewer } from "../types";
import { NoticeHost } from "./notices";
import "./public-shell.css";

export function PublicShell({
  children,
  compact = false,
}: {
  children: ReactNode;
  compact?: boolean;
}) {
  // Bootstrap tells the topbar whether the visitor already has a session, so
  // a signed-in reader gets a link back into the app instead of "Log in",
  // and carries the running server version for the same badge the app
  // sidebar shows.
  const viewer = useAPI<Viewer>("/api/v1/app/bootstrap");
  const authenticated = viewer.kind === "ready" && viewer.data.authenticated;
  const version = viewer.kind === "ready" ? viewer.data.version : "";
  return (
    <div className={compact ? "public-frame compact" : "public-frame"}>
      <header className="public-topbar">
        <a href="/" className="brand">
          <span className="brand-mark" aria-hidden="true" />
          <span>akari</span>
          {version ? <span className="brandver">{version}</span> : null}
        </a>
        <nav>
          <a href="/guide">Guide</a>
          <a href="/api/docs">API</a>
          <a
            className="gh-link"
            href="https://github.com/jssblck/akari"
            aria-label="akari on GitHub"
            title="akari on GitHub"
            target="_blank"
            rel="noreferrer"
          >
            <GithubLogoIcon size={16} />
          </a>
          {authenticated ? (
            <a className="button secondary" href="/overview">
              Overview
            </a>
          ) : (
            <a className="button secondary" href="/login">
              Log in
            </a>
          )}
        </nav>
      </header>
      <main>{children}</main>
      <NoticeHost />
    </div>
  );
}
