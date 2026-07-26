package contenoxcli

import (
	"strings"
	"testing"
)

func TestUnit_RootHelpSuggestsModelDiscovery(t *testing.T) {
	for _, want := range []string{
		"contenox model list",
		"contenox model registry-list",
	} {
		if !strings.Contains(rootCmd.Long, want) {
			t.Fatalf("root help missing %q", want)
		}
	}
}
