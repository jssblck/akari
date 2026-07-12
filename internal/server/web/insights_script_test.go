package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestInsightsScriptKeepsDataOutOfRawHTMLSinks(t *testing.T) {
	raw, err := fs.ReadFile(Static, "static/js/insights.js")
	if err != nil {
		t.Fatalf("read insights script: %v", err)
	}
	source := string(raw)
	for lineNumber, line := range strings.Split(source, "\n") {
		if !strings.Contains(line, "innerHTML =") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "template.innerHTML = html;" || strings.Contains(trimmed, "innerHTML = ''") {
			continue
		}
		t.Errorf("line %d writes unsanitized dynamic HTML: %s", lineNumber+1, trimmed)
	}
	for _, required := range []string{
		"template.content.querySelectorAll('*')",
		"A.escapeHTML(tool.name)",
		"A.appendLegendChip(el, D.projectViz[p], p)",
		"A.appendFigure(el, f.v, f.k)",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("insights script is missing safe rendering boundary %q", required)
		}
	}
}

func TestInsightsScriptHandlesEmptyFleetAndChurnPanels(t *testing.T) {
	raw, err := fs.ReadFile(Static, "static/js/insights.js")
	if err != nil {
		t.Fatalf("read insights script: %v", err)
	}
	source := string(raw)
	for _, guard := range []string{"if (models.length)", "if (D.churn.length > 0)", "(D.projects || [])"} {
		if !strings.Contains(source, guard) {
			t.Errorf("insights script is missing sparse-data guard %q", guard)
		}
	}
}

func TestInsightsScriptRegistersIdempotentSwapHydrationHooks(t *testing.T) {
	raw, err := fs.ReadFile(Static, "static/js/insights.js")
	if err != nil {
		t.Fatalf("read insights script: %v", err)
	}
	source := string(raw)
	for _, contract := range []string{
		"const hydratedPayloads = new WeakSet()",
		"hydratedPayloads.has(payload)",
		"hydratedPayloads.add(payload)",
		"document.addEventListener('htmx:afterSwap'",
		"document.addEventListener('htmx:load'",
	} {
		if !strings.Contains(source, contract) {
			t.Errorf("insights script is missing hydration contract %q", contract)
		}
	}
}
