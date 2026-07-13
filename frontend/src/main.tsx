import "swagger-ui-react/swagger-ui.css";
import "./styles.css";

import { lazy, type ReactNode, StrictMode, Suspense, useEffect } from "react";
import { createRoot } from "react-dom/client";
import {
  createBrowserRouter,
  Navigate,
  RouterProvider,
} from "react-router-dom";

import { AppShell } from "./app-shell";

const AccountPage = lazy(() =>
  import("./pages/account").then((module) => ({ default: module.AccountPage })),
);
const ApiDocsPage = lazy(() =>
  import("./pages/api-docs").then((module) => ({
    default: module.ApiDocsPage,
  })),
);
const AuthPage = lazy(() =>
  import("./pages/auth").then((module) => ({ default: module.AuthPage })),
);
const GuidePage = lazy(() =>
  import("./pages/guide").then((module) => ({ default: module.GuidePage })),
);
const InsightsPage = lazy(() =>
  import("./pages/insights").then((module) => ({
    default: module.InsightsPage,
  })),
);
const OverviewPage = lazy(() =>
  import("./pages/overview").then((module) => ({
    default: module.OverviewPage,
  })),
);
const OAuthConsentPage = lazy(() =>
  import("./pages/oauth-consent").then((module) => ({
    default: module.OAuthConsentPage,
  })),
);
const ProjectPage = lazy(() =>
  import("./pages/projects").then((module) => ({
    default: module.ProjectPage,
  })),
);
const ProjectsPage = lazy(() =>
  import("./pages/projects").then((module) => ({
    default: module.ProjectsPage,
  })),
);
const PublicOverviewPage = lazy(() =>
  import("./pages/public").then((module) => ({
    default: module.PublicOverviewPage,
  })),
);
const PublicProjectPage = lazy(() =>
  import("./pages/public").then((module) => ({
    default: module.PublicProjectPage,
  })),
);
const PublicSessionPage = lazy(() =>
  import("./pages/public").then((module) => ({
    default: module.PublicSessionPage,
  })),
);
const SessionPage = lazy(() =>
  import("./pages/sessions").then((module) => ({
    default: module.SessionPage,
  })),
);
const SessionsPage = lazy(() =>
  import("./pages/sessions").then((module) => ({
    default: module.SessionsPage,
  })),
);

function TitledRoute({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  useEffect(() => {
    document.title = `${title} · akari`;
  }, [title]);
  return children;
}

const router = createBrowserRouter([
  {
    path: "/login",
    element: (
      <TitledRoute title="Log in">
        <AuthPage mode="login" />
      </TitledRoute>
    ),
  },
  {
    path: "/register",
    element: (
      <TitledRoute title="Register">
        <AuthPage mode="register" />
      </TitledRoute>
    ),
  },
  {
    path: "/guide",
    element: (
      <TitledRoute title="User guide">
        <GuidePage />
      </TitledRoute>
    ),
  },
  {
    path: "/guide/:slug",
    element: (
      <TitledRoute title="User guide">
        <GuidePage />
      </TitledRoute>
    ),
  },
  {
    path: "/api/docs",
    element: (
      <TitledRoute title="API">
        <ApiDocsPage />
      </TitledRoute>
    ),
  },
  {
    path: "/oauth/authorize",
    element: (
      <TitledRoute title="Authorize">
        <OAuthConsentPage />
      </TitledRoute>
    ),
  },
  { path: "/u/:username", element: <PublicOverviewPage /> },
  { path: "/p/:id", element: <PublicProjectPage /> },
  { path: "/s/:publicId", element: <PublicSessionPage /> },
  {
    element: <AppShell />,
    children: [
      {
        path: "/overview",
        element: (
          <TitledRoute title="Overview">
            <OverviewPage />
          </TitledRoute>
        ),
      },
      {
        path: "/insights",
        element: (
          <TitledRoute title="Insights">
            <InsightsPage />
          </TitledRoute>
        ),
      },
      {
        path: "/projects",
        element: (
          <TitledRoute title="Projects">
            <ProjectsPage />
          </TitledRoute>
        ),
      },
      {
        path: "/projects/:id",
        element: (
          <TitledRoute title="Project">
            <ProjectPage />
          </TitledRoute>
        ),
      },
      {
        path: "/sessions",
        element: (
          <TitledRoute title="Sessions">
            <SessionsPage />
          </TitledRoute>
        ),
      },
      {
        path: "/sessions/:id",
        element: (
          <TitledRoute title="Session">
            <SessionPage />
          </TitledRoute>
        ),
      },
      {
        path: "/account",
        element: (
          <TitledRoute title="Account">
            <AccountPage />
          </TitledRoute>
        ),
      },
    ],
  },
  { path: "*", element: <Navigate to="/overview" replace /> },
]);

const root = document.getElementById("root");
if (!root) throw new Error("missing React root");
createRoot(root).render(
  <StrictMode>
    <Suspense
      fallback={
        <div className="route-loading" role="status">
          Loading view...
        </div>
      }
    >
      <RouterProvider router={router} />
    </Suspense>
  </StrictMode>,
);
