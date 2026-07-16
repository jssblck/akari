import {
  BookOpenTextIcon,
  ChartLineUpIcon,
  CirclesThreePlusIcon,
  FolderOpenIcon,
  GaugeIcon,
  GithubLogoIcon,
  ListIcon,
  ListMagnifyingGlassIcon,
  SignOutIcon,
  UserCircleIcon,
  XIcon,
} from "@phosphor-icons/react";
import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";

import { request, setCSRFToken, useAPI } from "./api";
import { withBase } from "./base";
import { AsyncView } from "./components/async-view";
import { attempt, NoticeHost } from "./components/notices";
import type { Viewer } from "./types";

const nav = [
  { to: "/overview", label: "Overview", icon: GaugeIcon },
  { to: "/projects", label: "Projects", icon: FolderOpenIcon },
  { to: "/sessions", label: "Sessions", icon: ListMagnifyingGlassIcon },
  {
    to: "/insights",
    label: "Insights",
    icon: ChartLineUpIcon,
    hideOnMobile: true,
  },
];

export function AppShell() {
  const viewer = useAPI<Viewer>("/api/v1/app/bootstrap");
  const location = useLocation();
  const navigate = useNavigate();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);

  useEffect(() => {
    if (viewer.kind !== "ready") return;
    setCSRFToken(viewer.data.csrf_token);
    if (!viewer.data.authenticated) {
      // next is always an external path (the server's login bounce builds it
      // the same way), and the router strips the basename from pathname, so
      // put it back before handing the value off.
      navigate(
        `/login?next=${encodeURIComponent(withBase(location.pathname + location.search))}`,
        { replace: true },
      );
    }
  }, [location.pathname, location.search, navigate, viewer]);

  useEffect(() => {
    if (!mobileNavOpen) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setMobileNavOpen(false);
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [mobileNavOpen]);

  return (
    <AsyncView state={viewer}>
      {(user) =>
        user.authenticated ? (
          <div className="app-frame">
            <header className="mobile-topbar">
              <AppBrand version={user.version} />
              <button
                type="button"
                className="nav-toggle"
                aria-controls="primary-sidebar"
                aria-expanded={mobileNavOpen}
                onClick={() => setMobileNavOpen(true)}
              >
                <ListIcon size={22} aria-hidden="true" />
                <span className="sr-only">Open navigation</span>
              </button>
            </header>
            {mobileNavOpen ? (
              <button
                type="button"
                className="sidebar-backdrop"
                aria-label="Close navigation"
                onClick={() => setMobileNavOpen(false)}
              />
            ) : null}
            <aside
              className={mobileNavOpen ? "sidebar mobile-open" : "sidebar"}
              id="primary-sidebar"
            >
              <div className="sidebar-head">
                <AppBrand version={user.version} />
                <button
                  type="button"
                  className="nav-toggle sidebar-close"
                  onClick={() => setMobileNavOpen(false)}
                >
                  <XIcon size={20} aria-hidden="true" />
                  <span className="sr-only">Close navigation</span>
                </button>
              </div>
              <nav aria-label="Primary navigation">
                {nav.map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    className={({ isActive }) =>
                      [
                        isActive ? "active" : "",
                        item.hideOnMobile ? "mobile-nav-hidden" : "",
                      ]
                        .filter(Boolean)
                        .join(" ")
                    }
                    onClick={() => setMobileNavOpen(false)}
                  >
                    <item.icon size={17} weight="regular" />
                    {item.label}
                  </NavLink>
                ))}
              </nav>
              <div className="sidebar-foot">
                <a
                  href="https://github.com/jssblck/akari"
                  target="_blank"
                  rel="noreferrer"
                >
                  <GithubLogoIcon size={17} /> GitHub
                </a>
                <a
                  href={withBase("/api/docs")}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <CirclesThreePlusIcon size={17} /> API
                </a>
                <a
                  href={withBase("/guide")}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <BookOpenTextIcon size={17} /> Guide
                </a>
                <NavLink to="/account" onClick={() => setMobileNavOpen(false)}>
                  <UserCircleIcon size={17} /> {user.username}
                </NavLink>
                <button
                  type="button"
                  className="nav-button"
                  onClick={async () => {
                    const ok = await attempt(
                      request("/api/v1/auth/logout", { method: "POST" }),
                    );
                    if (ok) window.location.assign(withBase("/login"));
                  }}
                >
                  <SignOutIcon size={17} /> Sign out
                </button>
              </div>
            </aside>
            <main className="app-main">
              <Outlet context={user} />
            </main>
            <NoticeHost />
          </div>
        ) : null
      }
    </AsyncView>
  );
}

function AppBrand({ version }: { version: string }) {
  return (
    <a href={withBase("/")} className="brand" aria-label="Akari homepage">
      <img
        className="brand-mark"
        src={withBase("/static/favicon.svg")}
        width="18"
        height="18"
        alt=""
      />
      <span>akari</span>
      {version ? <span className="brandver">{version}</span> : null}
    </a>
  );
}
