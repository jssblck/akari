package main

import (
	"strings"
	"testing"
)

func TestServerUsageDescribesArtifactBasedUpgrades(t *testing.T) {
	for _, want := range []string{"versioned container image or package", "replacing the managed binary"} {
		if !strings.Contains(serverUsage, want) {
			t.Errorf("serverUsage does not contain %q", want)
		}
	}
	for _, line := range strings.Split(serverUsage, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "akari-server" && fields[1] == "update" {
			t.Fatal("serverUsage advertises an unsupported command")
		}
	}
}

func TestUpdateIsNotAServerCommand(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "")

	err := runCommand([]string{"update"})
	if err == nil {
		t.Fatal("runCommand(update) succeeded")
	}
	if got := err.Error(); !strings.Contains(got, `unknown command "update"`) {
		t.Fatalf("runCommand(update) error = %q, want an unknown-command error", got)
	}
}
