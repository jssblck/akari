import { ArrowLeftIcon, ArrowRightIcon, CopyIcon } from "@phosphor-icons/react";
import Markdown from "react-markdown";
import { useParams } from "react-router-dom";
import remarkGfm from "remark-gfm";

import { useAPI } from "../api";
import { AsyncView } from "../components/async-view";
import { PublicShell } from "../components/public-shell";

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

export function GuidePage() {
  const { slug = "" } = useParams();
  const state = useAPI<GuideResponse>(
    `/api/v1/app/guide/${encodeURIComponent(slug)}`,
  );
  return (
    <PublicShell>
      <div className="guide-layout">
        <AsyncView state={state}>
          {(data) => {
            const index = data.chapters.findIndex(
              (chapter) => chapter.Slug === data.slug,
            );
            const previous = index > 0 ? data.chapters[index - 1] : undefined;
            const next = index >= 0 ? data.chapters[index + 1] : undefined;
            return (
              <>
                <aside className="guide-nav">
                  <span className="label">User guide</span>
                  <nav>
                    {data.chapters.map((chapter) => (
                      <a
                        key={chapter.Slug}
                        className={chapter.Slug === data.slug ? "active" : ""}
                        href={
                          chapter.Order === 0
                            ? "/guide"
                            : `/guide/${chapter.Slug}`
                        }
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
                        onClick={async () =>
                          navigator.clipboard.writeText(
                            await (await fetch(data.raw_path)).text(),
                          )
                        }
                      >
                        <CopyIcon /> Copy page
                      </button>
                      <a className="button secondary" href={data.raw_path}>
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
                    <Markdown remarkPlugins={[remarkGfm]}>
                      {data.raw_markdown}
                    </Markdown>
                  </div>
                  <footer className="guide-pager">
                    {previous ? (
                      <a
                        href={
                          previous.Order === 0
                            ? "/guide"
                            : `/guide/${previous.Slug}`
                        }
                      >
                        <ArrowLeftIcon /> {previous.Title}
                      </a>
                    ) : (
                      <span />
                    )}
                    {next ? (
                      <a href={`/guide/${next.Slug}`}>
                        {next.Title} <ArrowRightIcon />
                      </a>
                    ) : null}
                  </footer>
                </article>
                {data.headings.length > 0 ? (
                  <aside className="guide-toc">
                    <span className="label">On this page</span>
                    {data.headings.map((heading) => (
                      <a
                        className={heading.Level === 3 ? "nested" : ""}
                        href={`#${heading.ID}`}
                        key={heading.ID}
                      >
                        {heading.Text}
                      </a>
                    ))}
                  </aside>
                ) : null}
              </>
            );
          }}
        </AsyncView>
      </div>
    </PublicShell>
  );
}
