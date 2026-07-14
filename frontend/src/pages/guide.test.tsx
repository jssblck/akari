import { renderToStaticMarkup } from "react-dom/server";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { describe, expect, it } from "vitest";

import { guideComponents, rewriteGuideHref } from "./guide";

describe("guide rendering", () => {
  it("rewrites chapter links and preserves fragments", () => {
    expect(
      rewriteGuideHref("./accounts-and-sharing.md#api-tokens", "the-web-ui"),
    ).toBe("/guide/accounts-and-sharing#api-tokens");
    expect(rewriteGuideHref("./index.md", "the-web-ui")).toBe("/guide");
    expect(rewriteGuideHref("https://example.com/doc.md", "the-web-ui")).toBe(
      "https://example.com/doc.md",
    );
  });

  it("uses server heading ids and omits the duplicate chapter title", () => {
    const html = renderToStaticMarkup(
      <Markdown
        remarkPlugins={[remarkGfm]}
        components={guideComponents(
          [
            { Level: 2, ID: "first-section", Text: "First section" },
            { Level: 3, ID: "detail", Text: "Detail" },
          ],
          "example",
        )}
      >
        {
          "# Example\n\n## First section\n\n### Detail\n\n[Next](./next.md#start)"
        }
      </Markdown>,
    );
    expect(html).not.toContain("<h1");
    expect(html).toContain('<h2 id="first-section">');
    expect(html).toContain('<h3 id="detail">');
    expect(html).toContain('href="/guide/next#start"');
  });
});
