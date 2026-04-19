package setupcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNeedsNonDefaultOllamaBackendURL(t *testing.T) {
	t.Parallel()
	if needsNonDefaultOllamaBackendURL("http://127.0.0.1:11434") {
		t.Fatal("expected default loopback:11434 to use inferred URL")
	}
	if needsNonDefaultOllamaBackendURL("http://localhost:11434") {
		t.Fatal("expected localhost:11434 to use inferred URL")
	}
	if !needsNonDefaultOllamaBackendURL("http://127.0.0.1:11435") {
		t.Fatal("expected non-default port to require explicit --url")
	}
	if !needsNonDefaultOllamaBackendURL("http://gpu-host:11434") {
		t.Fatal("expected remote host to require explicit --url")
	}
}

func TestEnrichResultWithOllamaAt_noBackends(t *testing.T) {
	t.Parallel()
	r := Evaluate(Input{
		DefaultModel:    "m",
		DefaultProvider: "ollama",
		States:          nil,
	})
	r = enrichResultWithOllamaAt(r, "http://127.0.0.1:11434")
	var saw bool
	for _, iss := range r.Issues {
		if iss.Code == "no_backends" {
			saw = true
			if !strings.Contains(iss.Message, "Local Ollama responded at") {
				t.Fatalf("expected probe hint in message, got %q", iss.Message)
			}
			if !strings.Contains(iss.CLICommand, "ollama pull qwen2.5:7b") {
				t.Fatalf("expected ollama pull in command, got %q", iss.CLICommand)
			}
			if !strings.Contains(iss.CLICommand, "contenox backend add local --type ollama") {
				t.Fatalf("expected registration command, got %q", iss.CLICommand)
			}
			if !strings.Contains(iss.CLICommand, "contenox config set default-model qwen2.5:7b") {
				t.Fatalf("expected default-model in command, got %q", iss.CLICommand)
			}
		}
	}
	if !saw {
		t.Fatal("expected no_backends issue")
	}
}

func TestEnrichResultWithOllamaAt_skipsWhenOllamaReady(t *testing.T) {
	t.Parallel()
	r := Result{
		BackendChecks: []BackendCheck{
			{Name: "local", Type: "ollama", BaseURL: "http://127.0.0.1:11434", Reachable: true},
		},
		Issues: []Issue{{Code: "no_backends", Message: "x"}},
	}
	out := enrichResultWithOllamaAt(r, "http://127.0.0.1:11434")
	if out.Issues[0].Message != "x" {
		t.Fatalf("expected no mutation when ollama already reachable, got %q", out.Issues[0].Message)
	}
}

func TestProbeLocalOllamaAPI_httptest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	base, ok := ProbeLocalOllamaAPI(context.Background())
	if !ok {
		t.Fatal("expected probe success against httptest Ollama")
	}
	if base != strings.TrimRight(srv.URL, "/") {
		t.Fatalf("base URL: got %q want %q", base, srv.URL)
	}
}

func TestResolveOllamaProbeBaseURL_default(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	if got := resolveOllamaProbeBaseURL(); got != "http://127.0.0.1:11434" {
		t.Fatalf("got %q", got)
	}
}

func TestOllamaRegistrationCommand_customPort(t *testing.T) {
	cmd := ollamaRegistrationCommand("http://127.0.0.1:11435")
	if !strings.Contains(cmd, "--url") || !strings.Contains(cmd, "11435") {
		t.Fatalf("expected --url for custom port, got %q", cmd)
	}
}
