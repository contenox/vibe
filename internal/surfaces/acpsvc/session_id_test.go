package acpsvc

import (
	"strings"
	"sync"
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_NewSessionID_NoCollisionsUnderConcurrency(t *testing.T) {
	const goroutines = 64
	const perG = 2000

	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*perG)
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, perG)
			for i := 0; i < perG; i++ {
				local = append(local, newSessionID("acp"))
			}
			mu.Lock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					mu.Unlock()
					t.Errorf("duplicate session id minted concurrently: %q", id)
					return
				}
				seen[id] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	require.Len(t, seen, goroutines*perG,
		"every concurrently-minted session id must be unique — a collision aliases two ACP clients onto the same DB session")
}

func TestUnit_NewSessionID_NamespacePrefix(t *testing.T) {
	a := newSessionID("zed")
	b := newSessionID("zed")
	require.True(t, strings.HasPrefix(a, "zed-"), "namespace must be a readable prefix, got %q", a)
	require.True(t, strings.HasPrefix(b, "zed-"))
	require.NotEqual(t, a, b, "two ids in the same namespace must still differ")

	other := newSessionID("nvim")
	require.True(t, strings.HasPrefix(other, "nvim-"))
	require.NotEqual(t, strings.TrimPrefix(a, "zed-"), strings.TrimPrefix(other, "nvim-"))
}

func TestUnit_SessionNamespace(t *testing.T) {
	cases := []struct {
		name string
		info *libacp.Implementation
		want string
	}{
		{"nil client info falls back", nil, "acp"},
		{"simple name", &libacp.Implementation{Name: "Zed"}, "zed"},
		{"empty name falls back", &libacp.Implementation{Name: ""}, "acp"},
		{"punctuation/spaces stripped", &libacp.Implementation{Name: "My-Client 2!"}, "myclient2"},
		{"all-junk name falls back", &libacp.Implementation{Name: "—/—"}, "acp"},
		{"long name truncated to 16", &libacp.Implementation{Name: "abcdefghijklmnopqrstuvwxyz"}, "abcdefghijklmnop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &Transport{clientInfo: c.info}
			require.Equal(t, c.want, sessionNamespace(tr))
		})
	}
}
