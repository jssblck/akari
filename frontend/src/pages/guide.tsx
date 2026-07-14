import {
  ArrowLeftIcon,
  ArrowRightIcon,
  CopyIcon,
  ListIcon,
} from "@phosphor-icons/react";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import Markdown, { type Components } from "react-markdown";
import { useParams } from "react-router-dom";
import remarkGfm from "remark-gfm";

import { useAPI } from "../api";
import { withBase } from "../base";
import { AsyncView } from "../components/async-view";
import { PublicShell } from "../components/public-shell";
import "./guide.css";

type Chapter = { Slug: string; Title: string; Summary: string; Order: number };
type Heading = { Level: number; ID: string; Text: string };
type GuideResponse = {
  slug: string;
  title: string;
  summary: string;
  raw_markdown: string;
  headings: Heading[];
  raw_path: string;
  github_url: string;
  chapters: Chapter[];
};

// flattenText reduces a heading's rendered children (plain text mixed with
// inline formatting like `**bold**` or `code`) back to the bare string the
// guide renderer used to compute each heading's id, so a heading element can
// be matched against the API's headings list without re-implementing
// goldmark's slug algorithm client-side.
function flattenText(node: ReactNode): string {
  if (node === null || node === undefined || typeof node === "boolean")
    return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(flattenText).join("");
  if (typeof node === "object" && "props" in node) {
    const children = (node as { props?: { children?: ReactNode } }).props
      ?.children;
    return flattenText(children);
  }
  return "";
}

// headingComponent builds the react-markdown override for one heading level.
// It looks the rendered text up in the chapter's headings list (a pure,
// order-independent match) rather than counting headings as they render, so
// it stays correct under React's dev-mode double-render without any reset
// bookkeeping.
function headingComponent(level: 2 | 3, headings: Heading[]) {
  const Tag = level === 2 ? "h2" : "h3";
  return function HeadingWithID({ children }: { children?: ReactNode }) {
    const text = flattenText(children);
    const match = headings.find((h) => h.Level === level && h.Text === text);
    return <Tag id={match?.ID}>{children}</Tag>;
  };
}

export function guideComponents(headings: Heading[], slug: string): Components {
  return {
    h1: () => null,
    h2: headingComponent(2, headings),
    h3: headingComponent(3, headings),
    a: ({ children, href }) => (
      <a href={rewriteGuideHref(href, slug)}>{children}</a>
    ),
  };
}

export function GuidePage() {
  const { slug = "" } = useParams();
  const state = useAPI<GuideResponse>(
    `/api/v1/app/guide/${encodeURIComponent(slug)}`,
  );
  const [navOpen, setNavOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const [activeHeading, setActiveHeading] = useState("");

  const headings = state.kind === "ready" ? state.data.headings : [];
  const components = useMemo<Components>(
    () => guideComponents(headings, slug),
    [headings, slug],
  );

  // The drawer is mobile-only chrome; a chapter switch should never leave it
  // open over the new page.
  // biome-ignore lint/correctness/useExhaustiveDependencies: slug is a navigation trigger, not a value read in the effect body.
  useEffect(() => setNavOpen(false), [slug]);

  useEffect(() => {
    if (!navOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setNavOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [navOpen]);

  // Scroll-spy: highlight whichever heading is nearest the top of the
  // viewport's reading band. The negative bottom margin means a heading only
  // "arrives" once its section is actually the one in view, not the instant
  // it enters the bottom of the screen.
  useEffect(() => {
    if (headings.length < 2) return;
    const targets = headings
      .map((heading) => document.getElementById(heading.ID))
      .filter((el): el is HTMLElement => el !== null);
    if (targets.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((entry) => entry.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        const nearest = visible[0];
        if (nearest) setActiveHeading(nearest.target.id);
      },
      { rootMargin: "-64px 0px -70% 0px", threshold: 0 },
    );
    for (const target of targets) observer.observe(target);
    return () => observer.disconnect();
  }, [headings]);

  return (
    <PublicShell>
      <div className={navOpen ? "guide-shell nav-open" : "guide-shell"}>
        <AsyncView state={state}>
          {(data) => {
            const index = data.chapters.findIndex(
              (chapter) => chapter.Slug === data.slug,
            );
            const previous = index > 0 ? data.chapters[index - 1] : undefined;
            const next = index >= 0 ? data.chapters[index + 1] : undefined;
            return (
              <>
                <button
                  type="button"
                  className="guide-menu"
                  aria-label="Open navigation"
                  aria-expanded={navOpen}
                  aria-controls="guide-sidebar"
                  onClick={() => setNavOpen((open) => !open)}
                >
                  <ListIcon size={16} />
                </button>
                <button
                  type="button"
                  className="guide-scrim"
                  aria-label="Close navigation"
                  tabIndex={navOpen ? 0 : -1}
                  onClick={() => setNavOpen(false)}
                />
                <div className="guide-layout">
                  <aside className="guide-nav" id="guide-sidebar">
                    <span className="label">User guide</span>
                    <nav>
                      {data.chapters.map((chapter) => (
                        <a
                          key={chapter.Slug}
                          className={chapter.Slug === data.slug ? "active" : ""}
                          href={withBase(
                            chapter.Order === 0
                              ? "/guide"
                              : `/guide/${chapter.Slug}`,
                          )}
                          onClick={() => setNavOpen(false)}
                        >
                          <span>{String(chapter.Order).padStart(2, "0")}</span>
                          {chapter.Order === 0 ? "Overview" : chapter.Title}
                        </a>
                      ))}
                    </nav>
                  </aside>
                  <article className="guide-article">
                    <header>
                      <h1>{data.title}</h1>
                      <p>{data.summary}</p>
                      <div className="guide-actions">
                        <button
                          className="button secondary"
                          type="button"
                          onClick={async () => {
                            const markdown =
                              data.raw_markdown ||
                              (await (
                                await fetch(withBase(data.raw_path))
                              ).text());
                            await navigator.clipboard.writeText(markdown);
                            setCopied(true);
                            setTimeout(() => setCopied(false), 1600);
                          }}
                        >
                          <CopyIcon /> {copied ? "Copied" : "Copy page"}
                        </button>
                        <a
                          className="button secondary"
                          href={withBase(data.raw_path)}
                        >
                          Markdown
                        </a>
                        <a
                          className="button secondary"
                          href={data.github_url}
                          target="_blank"
                          rel="noreferrer"
                        >
                          GitHub
                        </a>
                      </div>
                    </header>
                    <div className="prose">
                      <Markdown
                        remarkPlugins={[remarkGfm]}
                        components={components}
                      >
                        {data.raw_markdown}
                      </Markdown>
                    </div>
                    <footer className="guide-pager">
                      {previous ? (
                        <a
                          href={withBase(
                            previous.Order === 0
                              ? "/guide"
                              : `/guide/${previous.Slug}`,
                          )}
                        >
                          <ArrowLeftIcon /> {previous.Title}
                        </a>
                      ) : (
                        <span />
                      )}
                      {next ? (
                        <a href={withBase(`/guide/${next.Slug}`)}>
                          {next.Title} <ArrowRightIcon />
                        </a>
                      ) : null}
                    </footer>
                  </article>
                  {data.headings.length > 1 ? (
                    <aside className="guide-toc">
                      <span className="label">On this page</span>
                      {data.headings.map((heading) => (
                        <a
                          className={[
                            heading.Level === 3 ? "nested" : "",
                            heading.ID === activeHeading ? "active" : "",
                          ]
                            .filter(Boolean)
                            .join(" ")}
                          href={`#${heading.ID}`}
                          key={heading.ID}
                        >
                          {heading.Text}
                        </a>
                      ))}
                    </aside>
                  ) : null}
                </div>
              </>
            );
          }}
        </AsyncView>
      </div>
    </PublicShell>
  );
}

export function rewriteGuideHref(
  href: string | undefined,
  currentSlug: string,
): string | undefined {
  if (
    !href ||
    href.startsWith("#") ||
    href.startsWith("/") ||
    /^[a-z][a-z\d+.-]*:/i.test(href)
  ) {
    return href;
  }
  const target = new URL(
    href,
    `https://guide.invalid/guide/${currentSlug || "index"}.md`,
  );
  if (
    !target.pathname.startsWith("/guide/") ||
    !target.pathname.endsWith(".md")
  )
    return href;
  const targetSlug = target.pathname.slice("/guide/".length, -3);
  const route = targetSlug === "index" ? "/guide" : `/guide/${targetSlug}`;
  return `${withBase(route)}${target.search}${target.hash}`;
}
