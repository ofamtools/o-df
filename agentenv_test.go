package main

import (
	"os"
	"testing"
)

// TestMain clears the agent-harness markers the framework reads to detect an
// agent caller, so the suite runs the same inside an agent session as outside
// it. Tests that need an agent identity set their own environment.
func TestMain(m *testing.M) {
	for _, key := range []string{
		"OCHAZUKE_ACTOR", "OCHAZUKE_HARNESS", "TUIOS_HARNESS", "AGENT_HARNESS",
		"CODEX_THREAD_ID", "CODEX_CI", "CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_ENTRYPOINT", "PI_SESSION_ID",
	} {
		os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
