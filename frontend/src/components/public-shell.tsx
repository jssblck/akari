import type { ReactNode } from "react";

export function PublicShell({
  children,
  compact = false,
}: {
  children: ReactNode;
  compact?: boolean;
}) {
  return (
    <div className={compact ? "public-frame compact" : "public-frame"}>
      <header className="public-topbar">
        <a href="/" className="brand">
          <span className="brand-mark" aria-hidden="true" />
          <span>akari</span>
        </a>
        <nav>
          <a href="/guide">Guide</a>
          <a href="/api/docs">API</a>
          <a className="button secondary" href="/login">
            Log in
          </a>
        </nav>
      </header>
      <main>{children}</main>
    </div>
  );
}
