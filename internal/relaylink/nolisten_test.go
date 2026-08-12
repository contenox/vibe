package relaylink_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnit_ConnectorNeverListens checks the package source contains no
// listening or accept call.
func TestUnit_ConnectorNeverListens(t *testing.T) {
	t.Parallel()
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
