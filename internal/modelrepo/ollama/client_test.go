package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama/ollama/api"
)

func TestOllamaHTTPClient_ListUsesBearerAuthAndNormalizesAPIPath(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer srv.Close()

	client, err := newOllamaHTTPClient(srv.URL+"/api", "test-key", srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	if gotPath != "/api/tags" {
		t.Fatalf("path = %q, want /api/tags", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q, want Bearer test-key", gotAuth)
	}
}

func TestOllamaHTTPClient_GenerateStreamsNDJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"response":"hel","done":false}`)
		fmt.Fprintln(w, `{"response":"lo","done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	client, err := newOllamaHTTPClient(srv.URL, "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	var chunks []string
	err = client.Generate(context.Background(), &api.GenerateRequest{Model: "test"}, func(resp api.GenerateResponse) error {
		chunks = append(chunks, resp.Response)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0] != "hel" || chunks[1] != "lo" {
		t.Fatalf("unexpected streamed chunks: %#v", chunks)
	}
}
