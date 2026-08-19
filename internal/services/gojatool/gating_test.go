package gojatool

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// TestUnit_Goja_ScopedNameFollowsTheAllowlistVocabulary pins what an operator can express about this toolset: "*" admits it, "!name" removes it, a bare name grants exactly it, an empty allowlist grants nothing.
func TestUnit_Goja_ScopedNameFollowsTheAllowlistVocabulary(t *testing.T) {
	// The universe PersistentRepo.Supports would report for a machine that has
	// this toolset registered alongside the unscoped ones.
	all := []string{"local_fs", "local_shell", ScopedToolsProviderName}

	t.Run("star admits everything", func(t *testing.T) {
		got := taskengine.ExportedApplyAllowlist([]string{"*"}, all)
		if strings.Join(got, ",") != strings.Join(all, ",") {
			t.Fatalf("a wildcard reached %v, want every connected toolset %v", got, all)
		}
	})

	t.Run("bang removes one set from under the star", func(t *testing.T) {
		got := taskengine.ExportedApplyAllowlist([]string{"*", "!" + ScopedToolsProviderName}, all)
		if strings.Join(got, ",") != "local_fs,local_shell" {
			t.Fatalf("removing one set resolved %v; that is how an operator drops exactly this toolset", got)
		}
	})

	t.Run("a bare name grants exactly it", func(t *testing.T) {
		got := taskengine.ExportedApplyAllowlist([]string{"local_fs", ScopedToolsProviderName}, all)
		if strings.Join(got, ",") != "local_fs,"+ScopedToolsProviderName {
			t.Fatalf("naming the toolsets exactly resolved %v", got)
		}
	})

	t.Run("an explicit exclusion beats an explicit grant", func(t *testing.T) {
		got := taskengine.ExportedApplyAllowlist([]string{ScopedToolsProviderName, "!" + ScopedToolsProviderName}, all)
		if len(got) != 0 {
			t.Fatalf("an explicit exclusion did not win: %v", got)
		}
	})

	t.Run("an empty allowlist grants nothing", func(t *testing.T) {
		if got := taskengine.ExportedApplyAllowlist(nil, all); len(got) != 0 {
			t.Fatalf("no allowlist at all resolved %v", got)
		}
	})

	t.Run("the registered name carries the namespace", func(t *testing.T) {
		if !strings.HasPrefix(ScopedToolsProviderName, nativeScopePrefix) {
			t.Fatalf("%q dropped the native namespace; a declared source could collide with it", ScopedToolsProviderName)
		}
		ts, err := New(Config{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(ts.Shutdown)
		names, err := ts.Supports(context.Background())
		if err != nil {
			t.Fatalf("Supports: %v", err)
		}
		if names[0] != ScopedToolsProviderName {
			t.Fatalf("a default-configured toolset registers as %q, not the scoped name an allowlist addresses", names[0])
		}
	})
}

// The native scope is a sibling of decl-, not a sub-scope of it: no agentID /
// declared-source pair can mint this name, so a declared MCP server can never
// collide with — and be silently substituted by — the native toolset, which
// PersistentRepo.Exec resolves first.
func TestUnit_Goja_ScopedNameIsUnmintableByADeclaredSource(t *testing.T) {
	if runtimetypes.IsDeclaredToolName(ScopedToolsProviderName) {
		t.Fatalf("%q claims to be a row an agent declaration owns, and the reconcile sweep would reap it", ScopedToolsProviderName)
	}

	for _, agentID := range []string{"native", "native-goja", "goja", "Native", "native_goja", "n"} {
		for _, declared := range []string{"goja", "", "native-goja", "eval", "Goja"} {
			if got := runtimetypes.DeclaredToolName(agentID, declared); got == ScopedToolsProviderName {
				t.Fatalf("agent %q declaring %q mints %q, colliding with the native toolset", agentID, declared, got)
			}
		}
	}
}
