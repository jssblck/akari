import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { HoverTip, TokenCard } from "./token-card";

describe("TokenCard", () => {
  it("renders the In/Out/Cache read/Cache write classes", () => {
    render(
      <TokenCard input={1200} output={340} cacheRead={5600} cacheWrite={78} />,
    );
    expect(screen.getByText("In")).toBeInTheDocument();
    expect(screen.getByText("1.2k")).toBeInTheDocument();
    expect(screen.getByText("Out")).toBeInTheDocument();
    expect(screen.getByText("340")).toBeInTheDocument();
    expect(screen.getByText("Cache read")).toBeInTheDocument();
    expect(screen.getByText("5.6k")).toBeInTheDocument();
    expect(screen.getByText("Cache write")).toBeInTheDocument();
    expect(screen.getByText("78")).toBeInTheDocument();
  });

  it("hides the Reasoning row when reasoning is zero", () => {
    render(
      <TokenCard
        input={1}
        output={1}
        cacheRead={0}
        cacheWrite={0}
        reasoning={0}
      />,
    );
    expect(screen.queryByText("Reasoning")).not.toBeInTheDocument();
  });

  it("shows the Reasoning row when reasoning is above zero", () => {
    render(
      <TokenCard
        input={1}
        output={1}
        cacheRead={0}
        cacheWrite={0}
        reasoning={512}
      />,
    );
    expect(screen.getByText("Reasoning")).toBeInTheDocument();
    expect(screen.getByText("512")).toBeInTheDocument();
  });

  it("renders no cost footer when costUSD is not given", () => {
    const { container } = render(
      <TokenCard input={1} output={1} cacheRead={0} cacheWrite={0} />,
    );
    expect(container.querySelector(".tt-cost")).not.toBeInTheDocument();
  });

  it("shows a plain cost footer for a complete figure", () => {
    render(
      <TokenCard
        input={1}
        output={1}
        cacheRead={0}
        cacheWrite={0}
        costUSD={1.5}
      />,
    );
    expect(screen.getByText("$1.50")).toBeInTheDocument();
  });

  it("appends + to the cost footer when the figure is incomplete", () => {
    render(
      <TokenCard
        input={1}
        output={1}
        cacheRead={0}
        cacheWrite={0}
        costUSD={1.5}
        costIncomplete
      />,
    );
    expect(screen.getByText("$1.50+")).toBeInTheDocument();
  });
});

describe("HoverTip", () => {
  // happy-dom has no Popover API; stub the two calls HoverTip makes so the
  // show/hide wiring is observable.
  const showPopover = vi.fn();
  const hidePopover = vi.fn();
  const proto = HTMLElement.prototype as unknown as Record<string, unknown>;

  afterEach(() => {
    delete proto.showPopover;
    delete proto.hidePopover;
    showPopover.mockClear();
    hidePopover.mockClear();
  });

  function renderTip() {
    proto.showPopover = showPopover;
    proto.hidePopover = hidePopover;
    render(
      <HoverTip summary="1.2k">
        <div>card body</div>
      </HoverTip>,
    );
    const trigger = screen.getByText("1.2k");
    expect(trigger.className).toContain("hover-tip");
    return trigger;
  }

  it("opens the card on keyboard focus", () => {
    const trigger = renderTip();
    fireEvent.focus(trigger);
    expect(showPopover).toHaveBeenCalled();
  });

  it("closes the card on Escape without requiring a blur", () => {
    const trigger = renderTip();
    fireEvent.focus(trigger);
    fireEvent.keyDown(trigger, { key: "Escape" });
    expect(hidePopover).toHaveBeenCalled();
  });
});
