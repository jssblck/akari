import {
  BookOpenTextIcon,
  ChartLineUpIcon,
  CirclesThreePlusIcon,
  FolderOpenIcon,
  GaugeIcon,
  GithubLogoIcon,
  ListMagnifyingGlassIcon,
  SignOutIcon,
  UserCircleIcon,
} from "@phosphor-icons/react";
import { useEffect } from "react";
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
  { to: "/insights", label: "Insights", icon: ChartLineUpIcon },
];

export function AppShell() {
  const viewer = useAPI<Viewer>("/api/v1/app/bootstrap");
  const location = useLocation();
  const navigate = useNavigate();

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

  return (
    <AsyncView state={viewer}>
      {(user) =>
        user.authenticated ? (
          <div className="app-frame">
            <aside className="sidebar">
              <a
                href={withBase("/")}
                className="brand"
                aria-label="Akari homepage"
              >
                <img
                  className="brand-mark"
                  src={withBase("/static/favicon.svg")}
                  width="18"
                  height="18"
                  alt=""
                />
                <span>akari</span>
                {user.version ? (
                  <span className="brandver">{user.version}</span>
                ) : null}
              </a>
              <nav aria-label="Primary navigation">
                {nav.map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    className={({ isActive }) => (isActive ? "active" : "")}
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
                <NavLink to="/account">
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
