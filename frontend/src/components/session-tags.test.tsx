import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import {
  FallbackTag,
  FanoutTag,
  GradeTag,
  KindTag,
  OutcomeTag,
  SessionPublicTag,
} from "./session-tags";

describe("KindTag", () => {
  it("renders a standalone chip", () => {
    render(<KindTag kind="standalone" />);
    expect(screen.getByText("standalone")).toBeInTheDocument();
  });
  it("renders an orphaned chip", () => {
    render(<KindTag kind="orphaned" />);
    expect(screen.getByText("orphaned")).toBeInTheDocument();
  });
  it("renders nothing for a remote project", () => {
    const { container } = render(<KindTag kind="remote" />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("SessionPublicTag", () => {
  it("renders nothing while private", () => {
    const { container } = render(
      <SessionPublicTag visibility="private" publicID={null} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders a linked chip once a public id is known", () => {
    render(<SessionPublicTag visibility="public" publicID="abc123" />);
    const link = screen.getByRole("link", { name: /public/ });
    expect(link).toHaveAttribute("href", "/s/abc123");
  });

  it("renders the plain marker when nested inside another link", () => {
    render(
      <SessionPublicTag visibility="public" publicID="abc123" linked={false} />,
    );
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.getByText("public")).toBeInTheDocument();
  });

  it("renders the plain marker when no public id is known yet", () => {
    render(<SessionPublicTag visibility="public" publicID={null} />);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.getByText("public")).toBeInTheDocument();
  });
});

describe("GradeTag", () => {
  it("renders the grade letter and lowercases the class", () => {
    render(<GradeTag grade="B" />);
    const chip = screen.getByText("B");
    expect(chip).toHaveClass("tag", "grade", "grade-b");
  });
  it("renders nothing for a null grade", () => {
    const { container } = render(<GradeTag grade={null} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("OutcomeTag", () => {
  it("renders the abandoned note", () => {
    render(<OutcomeTag outcome="abandoned" />);
    expect(screen.getByText("abandoned")).toBeInTheDocument();
  });
  it("renders the errored note", () => {
    render(<OutcomeTag outcome="errored" />);
    expect(screen.getByText("errored")).toBeInTheDocument();
  });
  it("renders nothing for a completed outcome", () => {
    const { container } = render(<OutcomeTag outcome="completed" />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("FallbackTag", () => {
  it("renders nothing for a zero count", () => {
    const { container } = render(<FallbackTag count={0} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the plain label and title for a single fallback", () => {
    render(<FallbackTag count={1} />);
    const chip = screen.getByText("fallback");
    expect(chip).toHaveAttribute(
      "title",
      "1 turn fell back from Fable 5 to a lower model",
    );
  });

  it("renders the counted label and title for multiple fallbacks", () => {
    render(<FallbackTag count={4} />);
    const chip = screen.getByText("fallback ×4");
    expect(chip).toHaveAttribute(
      "title",
      "4 turns fell back from Fable 5 to a lower model",
    );
  });
});

describe("FanoutTag", () => {
  it("renders nothing when there is no subagent fan-out", () => {
    const { container } = render(<FanoutTag subagentCount={0} costUSD={0} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("singularizes the unit for exactly one subagent", () => {
    render(<FanoutTag subagentCount={1} costUSD={0.5} />);
    expect(screen.getByText("1 subagent · $0.50")).toBeInTheDocument();
  });

  it("pluralizes the unit", () => {
    render(<FanoutTag subagentCount={5} costUSD={2.5} />);
    expect(screen.getByText("5 subagents · $2.50")).toBeInTheDocument();
  });
});
