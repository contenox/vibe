package modeldinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/contenox/contenox/libtransport"
)

func TestUnit_ArtifactFromBuild(t *testing.T) {
	const base = "https://example.test/modeld"
	cases := []struct {
		goos, goarch string
		wantName     string
	}{
		{"linux", "amd64", "modeld-v0.32.5-linux-amd64.tar.gz"},
		{"darwin", "arm64", "modeld-v0.32.5-darwin-arm64.tar.gz"},
		{"windows", "amd64", "modeld-v0.32.5-windows-amd64.zip"},
	}
	for _, c := range cases {
		t.Run(c.goos+"-"+c.goarch, func(t *testing.T) {
			platform := c.goos + "-" + c.goarch
			b := indexBuild{
				Version:  "v0.32.5",
				Platform: platform,
				Protocol: transport.ProtocolVersion,
				Backends: []string{"llama", "openvino"},
				Channel:  "stable",
				Archive:  "v0.32.5/" + c.wantName,
				SHA256:   "v0.32.5/" + c.wantName + ".sha256",
			}
			a, err := artifactFromBuild(base, b, c.goos)
			if err != nil {
				t.Fatalf("artifactFromBuild: %v", err)
			}
			if a.name != c.wantName {
				t.Fatalf("name = %q, want %q", a.name, c.wantName)
			}
			wantURL := base + "/v0.32.5/" + c.wantName
			if a.archiveURL != wantURL {
				t.Fatalf("archiveURL = %q, want %q", a.archiveURL, wantURL)
			}
			if a.sumURL != wantURL+".sha256" {
				t.Fatalf("sumURL = %q, want %q", a.sumURL, wantURL+".sha256")
			}
			if a.topLevelDir() != "modeld-v0.32.5-"+c.goos+"-"+c.goarch {
				t.Fatalf("topLevelDir = %q", a.topLevelDir())
			}
		})
	}
}

func TestUnit_ArtifactFromBuild_Rejections(t *testing.T) {
	b := indexBuild{Version: "v1.0.0", Platform: "plan9-amd64", Archive: "v1/modeld-v1-plan9-amd64.bin", SHA256: "v1/modeld-v1-plan9-amd64.bin.sha256"}
	if _, err := artifactFromBuild(DefaultBaseURL, b, "plan9"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("plan9 err = %v, want ErrUnsupportedPlatform", err)
	}
	b = indexBuild{Version: "v1.0.0", Platform: "linux-amd64", Archive: "../evil.tar.gz", SHA256: "v1/evil.tar.gz.sha256"}
	if _, err := artifactFromBuild(DefaultBaseURL, b, "linux"); err == nil {
		t.Fatalf("artifactFromBuild accepted unsafe archive path")
	}
}

func TestUnit_SelectBuild(t *testing.T) {
	idx := indexDocument{Schema: 1, Builds: []indexBuild{
		{Version: "v1.0.0", Platform: "linux-amd64", Protocol: transport.ProtocolVersion, Backends: []string{"llama"}, Channel: "stable", Archive: "v1.0.0/modeld-v1.0.0-linux-amd64.tar.gz", SHA256: "v1.0.0/modeld-v1.0.0-linux-amd64.tar.gz.sha256"},
		{Version: "v1.2.0", Platform: "linux-amd64", Protocol: transport.ProtocolVersion + 1, Backends: []string{"llama", "openvino"}, Channel: "stable", Archive: "v1.2.0/modeld-v1.2.0-linux-amd64.tar.gz", SHA256: "v1.2.0/modeld-v1.2.0-linux-amd64.tar.gz.sha256"},
		{Version: "v1.1.0", Platform: "linux-amd64", Protocol: transport.ProtocolVersion, Backends: []string{"llama", "openvino"}, Channel: "beta", Archive: "v1.1.0/modeld-v1.1.0-linux-amd64.tar.gz", SHA256: "v1.1.0/modeld-v1.1.0-linux-amd64.tar.gz.sha256"},
		{Version: "v1.0.10", Platform: "linux-amd64", Protocol: transport.ProtocolVersion, Backends: []string{"llama", "openvino"}, Channel: "stable", Archive: "v1.0.10/modeld-v1.0.10-linux-amd64.tar.gz", SHA256: "v1.0.10/modeld-v1.0.10-linux-amd64.tar.gz.sha256"},
		{Version: "v9.0.0", Platform: "darwin-arm64", Protocol: transport.ProtocolVersion, Backends: []string{"llama", "openvino"}, Channel: "stable", Archive: "v9.0.0/modeld-v9.0.0-darwin-arm64.tar.gz", SHA256: "v9.0.0/modeld-v9.0.0-darwin-arm64.tar.gz.sha256"},
	}}
	got, err := selectBuild(idx, "linux-amd64", "openvino")
	if err != nil {
		t.Fatalf("selectBuild: %v", err)
	}
	if got.Version != "v1.0.10" {
		t.Fatalf("selected version = %q, want v1.0.10", got.Version)
	}

	if _, err := selectBuild(idx, "linux-amd64", "missing"); !errors.Is(err, ErrNoCompatibleArtifact) {
		t.Fatalf("missing backend err = %v, want ErrNoCompatibleArtifact", err)
	}
}

func TestUnit_ParseSHA256(t *testing.T) {
	const sum = "e4fcaf5a6ffba8c2e28d7182914c48aeb767b7c46a3ffa0d60b19267487e0066"
	cases := map[string]string{
		"real sha256sum format": sum + "  modeld-v0.32.5-linux-amd64.tar.gz\n",
		"leading blank line":    "\n" + sum + "  file.tar.gz\n",
		"backslash prefix":      `\` + sum + "  weird name.tar.gz\n",
		"uppercase":             strings.ToUpper(sum) + "  file\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseSHA256(text)
			if err != nil {
				t.Fatalf("parseSHA256: %v", err)
			}
			if got != sum {
				t.Fatalf("got %q, want %q", got, sum)
			}
		})
	}

	for name, text := range map[string]string{
		"empty":     "",
		"too short": "abc123  file\n",
		"non-hex":   strings.Repeat("z", 64) + "  file\n",
	} {
		t.Run("reject "+name, func(t *testing.T) {
			if _, err := parseSHA256(text); err == nil {
				t.Fatalf("parseSHA256(%q) = nil err, want error", text)
			}
		})
	}
}

func TestUnit_VerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("hello modeld"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello modeld"))
	want := hex.EncodeToString(sum[:])

	if err := verifyChecksum(p, want); err != nil {
		t.Fatalf("verifyChecksum (match): %v", err)
	}
	if err := verifyChecksum(p, strings.Repeat("0", 64)); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("verifyChecksum (mismatch) = %v, want ErrChecksumMismatch", err)
	}
}

func TestUnit_GetSmallText_StatusMapping(t *testing.T) {
	cases := []struct {
		status  int
		wantErr error // nil => expect success
	}{
		{http.StatusOK, nil},
		{http.StatusNotFound, ErrArtifactUnavailable},
		{http.StatusForbidden, ErrArtifactUnavailable},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte("body"))
			}))
			defer srv.Close()
			c0 := &client{clientVersion: "v1", http: srv.Client()}
			body, err := c0.getSmallText(context.Background(), srv.URL)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if body != "body" {
					t.Fatalf("body = %q", body)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}

	t.Run("500 is not a sentinel", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c0 := &client{clientVersion: "v1", http: srv.Client()}
		_, err := c0.getSmallText(context.Background(), srv.URL)
		if err == nil || errors.Is(err, ErrArtifactUnavailable) {
			t.Fatalf("500 err = %v, want a non-sentinel error", err)
		}
	})
}

func TestUnit_SafeExtraction_RejectsUnsafeTar(t *testing.T) {
	for _, bad := range []string{"/etc/evil", "../escape", "sub/../../escape"} {
		t.Run(bad, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			body := []byte("x")
			_ = tw.WriteHeader(&tar.Header{Name: bad, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(body)
			_ = tw.Close()
			_ = gz.Close()

			arc := filepath.Join(t.TempDir(), "a.tar.gz")
			if err := os.WriteFile(arc, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			if err := extractTarGz(arc, dest); err == nil {
				t.Fatalf("extractTarGz(%q) = nil err, want rejection", bad)
			}
		})
	}
}

func TestUnit_SafeExtraction_RejectsUnsafeZip(t *testing.T) {
	for _, bad := range []string{"/etc/evil", "../escape"} {
		t.Run(bad, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, err := zw.Create(bad)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte("x"))
			_ = zw.Close()

			arc := filepath.Join(t.TempDir(), "a.zip")
			if err := os.WriteFile(arc, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			if err := extractZip(arc, dest); err == nil {
				t.Fatalf("extractZip(%q) = nil err, want rejection", bad)
			}
		})
	}
}

func TestUnit_CheckCapability(t *testing.T) {
	both := versionInfo{Version: "v1", Protocol: transport.ProtocolVersion, Backends: []string{"llama", "openvino"}}
	if err := checkCapability(both, "llama"); err != nil {
		t.Fatalf("llama from both: %v", err)
	}
	if err := checkCapability(both, "openvino"); err != nil {
		t.Fatalf("openvino from both: %v", err)
	}

	llamaOnly := versionInfo{Version: "v1", Protocol: transport.ProtocolVersion, Backends: []string{"llama"}}
	if err := checkCapability(llamaOnly, "openvino"); !errors.Is(err, ErrBackendMissing) {
		t.Fatalf("openvino from llama-only = %v, want ErrBackendMissing", err)
	}

	newerProtocol := versionInfo{Version: "v2", Protocol: transport.ProtocolVersion + 1, Backends: []string{"llama", "openvino"}}
	if err := checkCapability(newerProtocol, "llama"); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("protocol mismatch = %v, want ErrProtocolMismatch", err)
	}
}

func TestUnit_ProbeBinary_LegacyMissingProtocolUsesMinProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is a POSIX shell script")
	}
	launcher := filepath.Join(t.TempDir(), "modeld")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"v0.32.5\",\"backends\":[\"llama\",\"openvino\"]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ProbeBinary(context.Background(), launcher)
	if err != nil {
		t.Fatalf("ProbeBinary: %v", err)
	}
	if got.Protocol != transport.MinProtocol {
		t.Fatalf("Protocol = %d, want legacy min protocol %d", got.Protocol, transport.MinProtocol)
	}
	if err := checkCapability(got, "llama"); err != nil {
		t.Fatalf("legacy checkCapability: %v", err)
	}
}

func TestUnit_FindCompatibleInstall_CurrentPointer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is a POSIX shell script")
	}
	dataRoot := t.TempDir()
	installDir := ManagedInstallDir(dataRoot, "v1.2.3", runtime.GOOS, runtime.GOARCH)
	launcher := filepath.Join(installDir, LauncherName(runtime.GOOS))
	writeFakeModeldLauncher(t, launcher, "v1.2.3", transport.ProtocolVersion, []string{"llama", "openvino"})
	if err := WriteCurrentPointer(dataRoot, installDir); err != nil {
		t.Fatalf("WriteCurrentPointer: %v", err)
	}

	got, err := FindCompatibleInstall(context.Background(), dataRoot, runtime.GOOS, runtime.GOARCH, "openvino")
	if err != nil {
		t.Fatalf("FindCompatibleInstall: %v", err)
	}
	if got.LauncherPath != launcher {
		t.Fatalf("LauncherPath = %q, want %q", got.LauncherPath, launcher)
	}
	if got.Version != "v1.2.3" || got.Protocol != transport.ProtocolVersion {
		t.Fatalf("install metadata = version %q protocol %d", got.Version, got.Protocol)
	}
}

func TestUnit_FindCompatibleInstall_ScanChoosesNewestCompatible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is a POSIX shell script")
	}
	dataRoot := t.TempDir()
	oldLauncher := ManagedLauncherPath(dataRoot, "v1.0.0", runtime.GOOS, runtime.GOARCH)
	newLauncher := ManagedLauncherPath(dataRoot, "v1.0.10", runtime.GOOS, runtime.GOARCH)
	incompatLauncher := ManagedLauncherPath(dataRoot, "v9.0.0", runtime.GOOS, runtime.GOARCH)
	writeFakeModeldLauncher(t, oldLauncher, "v1.0.0", transport.ProtocolVersion, []string{"llama", "openvino"})
	writeFakeModeldLauncher(t, newLauncher, "v1.0.10", transport.ProtocolVersion, []string{"llama", "openvino"})
	writeFakeModeldLauncher(t, incompatLauncher, "v9.0.0", transport.ProtocolVersion+1, []string{"llama", "openvino"})

	got, err := FindCompatibleInstall(context.Background(), dataRoot, runtime.GOOS, runtime.GOARCH, "llama")
	if err != nil {
		t.Fatalf("FindCompatibleInstall: %v", err)
	}
	if got.LauncherPath != newLauncher {
		t.Fatalf("LauncherPath = %q, want newest compatible %q", got.LauncherPath, newLauncher)
	}
}

// --- end-to-end install against a fake release server ---

func TestUnit_EnsureInstalled_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is a POSIX shell script")
	}
	const version = "v9.9.9"
	platform := Platform(runtime.GOOS, runtime.GOARCH)
	archive, sum := buildFakeTarGz(t, version, platform)
	index := buildFakeIndex(version, platform, "modeld-"+version+"-"+platform+".tar.gz")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(index))
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%s  modeld-%s-%s.tar.gz\n", sum, version, platform)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataRoot := t.TempDir()
	opts := Options{BaseURL: srv.URL, DataRoot: dataRoot}

	res, err := EnsureInstalled(context.Background(), "openvino", opts)
	if err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	wantLauncher := ManagedLauncherPath(dataRoot, version, runtime.GOOS, runtime.GOARCH)
	if res.LauncherPath != wantLauncher {
		t.Fatalf("launcher = %q, want %q", res.LauncherPath, wantLauncher)
	}
	if !fileExists(res.LauncherPath) {
		t.Fatalf("launcher not installed at %q", res.LauncherPath)
	}
	if res.Version != version {
		t.Fatalf("version = %q, want %q", res.Version, version)
	}
	if res.Protocol != transport.ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", res.Protocol, transport.ProtocolVersion)
	}
	if res.AlreadyInstalled {
		t.Fatalf("first install reported AlreadyInstalled")
	}
	if launcher, err := CurrentLauncherPath(dataRoot, runtime.GOOS); err != nil || launcher != wantLauncher {
		t.Fatalf("current launcher = %q, %v; want %q", launcher, err, wantLauncher)
	}
	// staging must be cleaned up.
	if entries, _ := os.ReadDir(filepath.Join(dataRoot, "modeld", ".staging")); len(entries) != 0 {
		t.Fatalf("staging left %d entries", len(entries))
	}

	// Second call reuses the existing install without re-downloading.
	res2, err := EnsureInstalled(context.Background(), "llama", opts)
	if err != nil {
		t.Fatalf("second EnsureInstalled: %v", err)
	}
	if !res2.AlreadyInstalled {
		t.Fatalf("second install did not report AlreadyInstalled")
	}
}

func TestUnit_EnsureInstalled_ChecksumMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is a POSIX shell script")
	}
	const version = "v9.9.9"
	platform := Platform(runtime.GOOS, runtime.GOARCH)
	archive, _ := buildFakeTarGz(t, version, platform)
	index := buildFakeIndex(version, platform, "modeld-"+version+"-"+platform+".tar.gz")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(index))
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%s  modeld-%s-%s.tar.gz\n", strings.Repeat("0", 64), version, platform)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataRoot := t.TempDir()
	_, err := EnsureInstalled(context.Background(), "llama", Options{BaseURL: srv.URL, DataRoot: dataRoot})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if dirExists(ManagedInstallDir(dataRoot, version, runtime.GOOS, runtime.GOARCH)) {
		t.Fatalf("install dir created despite checksum mismatch")
	}
}

func TestUnit_EnsureInstalled_NoIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := EnsureInstalled(context.Background(), "llama", Options{
		BaseURL: srv.URL, DataRoot: t.TempDir(),
	})
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
}

func TestUnit_EnsureInstalled_NoCompatibleArtifact(t *testing.T) {
	platform := Platform(runtime.GOOS, runtime.GOARCH)
	idx := indexDocument{Schema: 1, Builds: []indexBuild{{
		Version: "v9.9.9", Platform: platform, Protocol: transport.ProtocolVersion + 1,
		Backends: []string{"llama"}, Channel: "stable",
		Archive: "v9.9.9/modeld-v9.9.9-" + platform + ".tar.gz", SHA256: "v9.9.9/modeld-v9.9.9-" + platform + ".tar.gz.sha256",
	}}}
	body, _ := json.Marshal(idx)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := EnsureInstalled(context.Background(), "llama", Options{BaseURL: srv.URL, DataRoot: t.TempDir()})
	if !errors.Is(err, ErrNoCompatibleArtifact) {
		t.Fatalf("err = %v, want ErrNoCompatibleArtifact", err)
	}
}

func TestUnit_EnsureInstalled_ArtifactUnavailable(t *testing.T) {
	const version = "v9.9.9"
	platform := Platform(runtime.GOOS, runtime.GOARCH)
	index := buildFakeIndex(version, platform, "modeld-"+version+"-"+platform+".tar.gz")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			_, _ = w.Write([]byte(index))
			return
		}
		http.Error(w, "missing", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := EnsureInstalled(context.Background(), "llama", Options{BaseURL: srv.URL, DataRoot: t.TempDir()})
	if !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("err = %v, want ErrArtifactUnavailable", err)
	}
}

// buildFakeTarGz produces a release-shaped archive: a single
// modeld-<version>-<platform>/ directory containing a runnable `modeld` launcher
// that prints the expected `version --json`. Returns the gzip bytes and their
// lowercase-hex SHA-256.
func buildFakeTarGz(t *testing.T, version, platform string) ([]byte, string) {
	t.Helper()
	top := fmt.Sprintf("modeld-%s-%s", version, platform)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"version\":\"%s\",\"protocol\":%d,\"backends\":[\"llama\",\"openvino\"]}'\n", version, transport.ProtocolVersion)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	body := []byte(script)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/modeld", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func writeFakeModeldLauncher(t *testing.T, path, version string, protocol int, backends []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	backendsJSON := `"` + strings.Join(backends, `","`) + `"`
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '{\"version\":\"%s\",\"protocol\":%d,\"backends\":[%s]}'\n", version, protocol, backendsJSON)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func buildFakeIndex(version, platform, archiveName string) string {
	idx := indexDocument{Schema: 1, Builds: []indexBuild{{
		Version:  version,
		Platform: platform,
		Protocol: transport.ProtocolVersion,
		Backends: []string{"llama", "openvino"},
		Channel:  "stable",
		Archive:  version + "/" + archiveName,
		SHA256:   version + "/" + archiveName + ".sha256",
		Size:     123,
	}}}
	b, _ := json.Marshal(idx)
	return string(b)
}
