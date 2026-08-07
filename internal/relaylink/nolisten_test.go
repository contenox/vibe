package relaylink_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnit_ConnectorNeverListens enforces the rule that this package dials out
// and never accepts, by reading its own source.
//
// A test is the right enforcement, not a comment: nothing in Go stops a later
// edit adding a listener to a package that already imports net, and by the time
// one is noticed in review it is in a release. The scan covers the test files
// too, so the rule cannot be evaded by putting the listener behind a helper
// that only tests call.
//
// This file is the one exemption, because it necessarily contains every
// forbidden spelling.
func TestUnit_ConnectorNeverListens(t *testing.T) {
	t.Parallel()
	// The standard library's whole inbound surface reachable from a
	// package that speaks TCP or TLS, plus the accept calls a listener
	// would need to be useful.
	forbidden := []string{
		"net.Listen", "net.ListenTCP", "net.ListenUDP", "net.ListenUnix",
		"net.ListenIP", "net.ListenPacket", "net.ListenMulticastUDP",
		"net.ListenConfig", "net.FileListener", "net.Listener",
		"tls.Listen", "tls.NewListener",
		"http.ListenAndServe", "http.ListenAndServeTLS", "http.Serve", "http.Server",
		".Accept(", ".AcceptTCP(", ".AcceptUnix(",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) < 5 {
		t.Fatalf("scanned %d files, expected the whole package", len(files))
	}
	self := filepath.Base(mustAbs(t, "nolisten_test.go"))
	checked := 0
	for _, path := range files {
		if filepath.Base(path) == self {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		checked++
		src := string(b)
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q: relaylink dials out and never listens", path, bad)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no files were scanned")
	}
}

func mustAbs(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("abs %s: %v", name, err)
	}
	return p
}
