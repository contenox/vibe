package contenoxcli

import (
	"archive/zip"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/stretchr/testify/require"
)

// TestUnit_RedactSecrets asserts the bundle's redaction: credential shapes and
// named assignments are replaced, the surrounding context survives so the line
// stays readable, and the reported count matches what was removed.
func TestUnit_RedactSecrets(t *testing.T) {
	t.Run("named assignments keep their field name", func(t *testing.T) {
		got, n := redactSecrets(`level=INFO api_key=sk-abcdefghijklmnopqrstuvwx backend=ollama`)
		require.Equal(t, 1, n)
		require.Contains(t, got, "api_key=[REDACTED]")
		require.Contains(t, got, "backend=ollama")
		require.NotContains(t, got, "abcdefghijklmnop")
	})

	t.Run("json values are redacted in place", func(t *testing.T) {
		got, n := redactSecrets(`{"apiKey": "AIzaSyA1234567890123456789012345", "type": "gemini"}`)
		require.Equal(t, 1, n)
		require.Contains(t, got, `"type": "gemini"`)
		require.NotContains(t, got, "AIzaSyA")
	})

	t.Run("bare provider key shapes are caught without a field name", func(t *testing.T) {
		got, n := redactSecrets("call failed for sk-ant-api03-ZZZZZZZZZZZZ")
		require.Equal(t, 1, n)
		require.NotContains(t, got, "sk-ant-api03")
	})

	t.Run("url userinfo loses only the password", func(t *testing.T) {
		got, n := redactSecrets("dialing https://admin:hunter2hunter2@vllm.internal/v1")
		require.Equal(t, 1, n)
		require.Contains(t, got, "https://admin:[REDACTED]@vllm.internal/v1")
	})

	t.Run("a lone userinfo is a credential too", func(t *testing.T) {
		got, n := redactSecrets("Failed to connect to NATS at nats://s3cr3ttok3n@bus.internal:4222")
		require.Equal(t, 1, n)
		require.Contains(t, got, "nats://[REDACTED]@bus.internal:4222")
	})

	t.Run("clean text is untouched and counts zero", func(t *testing.T) {
		in := "level=INFO msg=\"backend synced\" models=3 duration=1.2s"
		got, n := redactSecrets(in)
		require.Equal(t, 0, n)
		require.Equal(t, in, got)
	})
}

// TestUnit_WriteDoctorBundle asserts the archive carries the report, build
// info, and the log tail — all redacted — and that the count is reported.
func TestUnit_WriteDoctorBundle(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry.log"),
		[]byte("level=INFO msg=start\nlevel=ERROR api_key=sk-abcdefghijklmnopqrstuvwx\n"), 0o600))

	res := setupcheck.Result{
		DefaultModel:    "qwen3:8b",
		DefaultProvider: "ollama",
		BackendCount:    1,
		BackendChecks: []setupcheck.BackendCheck{
			{Name: "vllm", Type: "vllm", BaseURL: "https://u:hunter2hunter2@vllm.internal/v1"},
		},
	}

	path := filepath.Join(dir, "bundle.zip")
	redacted, err := writeDoctorBundle(path, res, dir, filepath.Join(dir, "local.db"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, redacted, 2, "the log key and the backend URL password must both be counted")

	members := readZip(t, path)
	require.Contains(t, members, "doctor.json")
	require.Contains(t, members, "build.txt")
	require.Contains(t, members, "logs/workspace/telemetry.log",
		"a log's member name must name its source directory: the three sources can hold same-named logs")

	require.Contains(t, members["doctor.json"], `"defaultModel": "qwen3:8b"`)
	require.NotContains(t, members["doctor.json"], "hunter2hunter2")
	require.Contains(t, members["logs/workspace/telemetry.log"], "msg=start")
	require.NotContains(t, members["logs/workspace/telemetry.log"], "abcdefghijklmnop")
	require.Contains(t, members["build.txt"], "platform:")
}

// TestUnit_ReadLogTail asserts an oversized log is archived as its tail with a
// truncation note rather than whole.
func TestUnit_ReadLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", doctorBundleLogTail+2048)+"TAILMARK"), 0o600))
	got, err := readLogTail(path)
	require.NoError(t, err)
	require.Contains(t, got, "[truncated:")
	require.Contains(t, got, "TAILMARK")
	require.Less(t, len(got), doctorBundleLogTail+256)
}

// TestUnit_DoctorIssueLink asserts the pre-filled issue carries the environment
// facts and names the bundle, and never inlines log content.
func TestUnit_DoctorIssueLink(t *testing.T) {
	res := setupcheck.Result{
		DefaultProvider: "ollama",
		Issues:          []setupcheck.Issue{{Code: "no_backends", Severity: "error", Message: "no backends registered"}},
	}
	link := doctorIssueLink(res, "/tmp/b.zip")
	require.True(t, strings.HasPrefix(link, doctorIssueURL+"?"))

	u, err := url.Parse(link)
	require.NoError(t, err)
	body := u.Query().Get("body")
	require.Contains(t, body, "- contenox: ")
	require.Contains(t, body, "ready: no — no backends registered")
	require.Contains(t, body, "(unset)", "an unset default must read as unset, not as an empty line")
	require.Contains(t, body, "/tmp/b.zip")
	require.NotEmpty(t, u.Query().Get("title"))
}

// TestUnit_DoctorBundlePath asserts --bundle-out wins and the default name is
// timestamped.
func TestUnit_DoctorBundlePath(t *testing.T) {
	require.Equal(t, "/tmp/x.zip", doctorBundlePath("/tmp/x.zip", time.Now()))
	require.Equal(t, "contenox-doctor-20260805-101500.zip",
		doctorBundlePath("", time.Date(2026, 8, 5, 10, 15, 0, 0, time.UTC)))
}

func readZip(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	require.NoError(t, err)
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		out[f.Name] = string(data)
	}
	return out
}
