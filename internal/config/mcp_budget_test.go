package config

import "testing"

func TestLoadServerMCPResponseBudget(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_MCP_RESPONSE_BUDGET_BYTES", "")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.MCPResponseBudgetBytes != 8<<20 {
		t.Fatalf("default MCPResponseBudgetBytes = %d, want %d", cfg.MCPResponseBudgetBytes, 8<<20)
	}

	t.Setenv("AKARI_MCP_RESPONSE_BUDGET_BYTES", "12582912")
	cfg, err = LoadServer()
	if err != nil {
		t.Fatalf("LoadServer with explicit budget: %v", err)
	}
	if cfg.MCPResponseBudgetBytes != 12<<20 {
		t.Fatalf("MCPResponseBudgetBytes = %d, want %d", cfg.MCPResponseBudgetBytes, 12<<20)
	}

	for _, invalid := range []string{"0", "1048576", "33554432", "-1", "not-a-number"} {
		t.Setenv("AKARI_MCP_RESPONSE_BUDGET_BYTES", invalid)
		if _, err := LoadServer(); err == nil {
			t.Fatalf("LoadServer accepted AKARI_MCP_RESPONSE_BUDGET_BYTES=%q", invalid)
		}
	}
}
