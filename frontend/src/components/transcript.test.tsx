import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { Message, TranscriptPage } from "../types";
import { Transcript } from "./transcript";

function baseMessage(overrides: Partial<Message>): Message {
  return {
    Content: "",
    DuplicatePrompt: false,
    HasThinking: false,
    HasToolUse: false,
    Model: "",
    Ordinal: 0,
    PromptDigest: 0,
    PromptFactsCurrent: false,
    PromptNoCode: false,
    PromptShort: false,
    Role: "assistant",
    ThinkingBytes: 0,
    ThinkingText: "",
    Timestamp: "2026-07-22T00:00:00Z",
    Usage: null,
    ...overrides,
  };
}

function pageOf(msgs: Message[]): TranscriptPage {
  return {
    Attachments: [],
    EarlierCount: 0,
    Events: [],
    Fallbacks: [],
    HasEarlier: false,
    More: false,
    Msgs: msgs,
    Seed: [],
    Tools: [],
  };
}

describe("Transcript assistant markdown", () => {
  it("renders markdown formatting instead of literal source", () => {
    const message = baseMessage({
      Ordinal: 1,
      Content:
        "**455 pass, 0 fail**\n\n- one\n- two\n\nRun `go test ./...` to check.",
    });
    render(<Transcript initial={pageOf([message])} blobBase="/blobs" />);

    expect(screen.getByText("455 pass, 0 fail").tagName).toBe("STRONG");
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByText("go test ./...").tagName).toBe("CODE");
  });

  it("renders an http(s) link as a real, safely-attributed hyperlink", () => {
    const message = baseMessage({
      Ordinal: 1,
      Content: "See [the docs](https://example.com/guide) for more.",
    });
    render(<Transcript initial={pageOf([message])} blobBase="/blobs" />);

    const link = screen.getByRole("link", { name: "the docs" });
    expect(link).toHaveAttribute("href", "https://example.com/guide");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  it("does not turn a non-http link target into a live link", () => {
    const message = baseMessage({
      Ordinal: 1,
      Content: "See [app.ts](src/daemon/web/app.ts:133) for the handler.",
    });
    const { container } = render(
      <Transcript initial={pageOf([message])} blobBase="/blobs" />,
    );

    expect(
      screen.queryByRole("link", { name: "app.ts" }),
    ).not.toBeInTheDocument();
    expect(container.querySelector("a")).not.toBeInTheDocument();
    expect(container.querySelector(".content")?.textContent).toBe(
      "See app.ts for the handler.",
    );
  });

  it("leaves non-assistant roles as plain text", () => {
    const message = baseMessage({
      Ordinal: 1,
      Role: "user",
      Content: "**not bold**",
    });
    render(<Transcript initial={pageOf([message])} blobBase="/blobs" />);

    expect(screen.getByText("**not bold**")).toBeInTheDocument();
  });
});

describe("Transcript thinking disclosure", () => {
  it("folds the thinking level into the disclosure label instead of a separate badge", () => {
    const message = baseMessage({
      Ordinal: 1,
      Content: "done",
      HasThinking: true,
      ThinkingText: "reasoning about the approach",
      ThinkingBytes: 200,
    });
    const { container } = render(
      <Transcript initial={pageOf([message])} blobBase="/blobs" />,
    );

    expect(
      container.querySelector(".thinking-summary-label")?.textContent,
    ).toBe("Thinking (low)");
    expect(container.querySelector(".thinking-band")).not.toBeInTheDocument();
  });

  it("keeps the standalone band for redacted thinking with no text to disclose", () => {
    const message = baseMessage({
      Ordinal: 1,
      Content: "done",
      HasThinking: true,
      ThinkingText: "",
      ThinkingBytes: 200,
    });
    const { container } = render(
      <Transcript initial={pageOf([message])} blobBase="/blobs" />,
    );

    expect(container.querySelector(".thinking")).not.toBeInTheDocument();
    expect(container.querySelector(".thinking-band-label")?.textContent).toBe(
      "thinking: low",
    );
  });
});
