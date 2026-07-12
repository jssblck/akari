package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInsightsRuntimeHydratesEachSwappedPayloadOnce(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required in CI to exercise the insights runtime")
		}
		t.Skip("node is not installed; skipping the insights runtime regression test")
	}

	script, err := os.ReadFile(filepath.Join("static", "js", "insights.js"))
	if err != nil {
		t.Fatalf("read insights.js: %v", err)
	}

	const prelude = `
globalThis.window = globalThis;

const listeners = new Map();
let payload = { textContent: "{}" };
let fleetMixRenders = 0;

const liveInsights = {
  querySelector(selector) {
    return selector === "#insights-data" ? payload : null;
  },
};

globalThis.document = {
  readyState: "loading",
  addEventListener(type, listener) {
    const registered = listeners.get(type) || [];
    registered.push(listener);
    listeners.set(type, registered);
  },
  getElementById(id) {
    if (id === "insights-data") return payload;
    if (id === "insights") return liveInsights;
    if (id === "fleetmix") return {};
    return null;
  },
  querySelectorAll() {
    return [];
  },
};

window.addEventListener = () => {};
window.matchMedia = () => ({ matches: false });
`

	const assertions = `
window.AK_FLEETMIX = {
  renderFleetMix() {
    fleetMixRenders += 1;
  },
};
window.AK_CHURN = {
  resetDrill() {},
  renderChurn() {},
};

function fire(type, event = {}) {
  for (const listener of listeners.get(type) || []) {
    listener(event);
  }
}

function expectRenders(expected, context) {
  if (fleetMixRenders !== expected) {
    throw new Error(context + ": expected " + expected + " renders, got " + fleetMixRenders);
  }
}

fire("DOMContentLoaded");
expectRenders(1, "initial document load");

fire("htmx:afterSwap", { detail: { target: { id: "insights" } } });
fire("htmx:load", { target: liveInsights });
expectRenders(1, "duplicate hooks for the initial payload");

payload = { textContent: "{}" };
fire("htmx:load", { target: liveInsights });
expectRenders(2, "replacement discovered by htmx:load");
fire("htmx:afterSwap", { detail: { target: { id: "insights" } } });
expectRenders(2, "afterSwap following htmx:load");

payload = { textContent: "{}" };
fire("htmx:afterSwap", { detail: { target: { id: "insights" } } });
expectRenders(3, "replacement discovered by afterSwap");
fire("htmx:load", { target: liveInsights });
expectRenders(3, "htmx:load following afterSwap");
`

	harnessPath := filepath.Join(t.TempDir(), "insights-runtime-test.js")
	harness := prelude + string(script) + assertions
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write insights runtime harness: %v", err)
	}

	command := exec.Command(node, harnessPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("insights runtime regression failed: %v\n%s", err, output)
	}
}
