//go:build !llamanode

package llama

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/transport"
)

// fakeSession is a no-op transport.Session used to check that Service wires a
// request to a backend session without compiling the CGO llama.cpp backend.
type fakeSession struct{ closed bool }

func (f *fakeSession) EnsurePrefix(context.Context, transport.PrefixInput) (transport.PrefixStatus, error) {
	return transport.PrefixStatus{}, nil
}

func (f *fakeSession) PrefillSuffix(context.Context, transport.SuffixInput) (transport.SuffixStatus, error) {
	return transport.SuffixStatus{}, nil
}

func (f *fakeSession) Decode(context.Context, transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	ch := make(chan transport.StreamChunk)
	close(ch)
	return ch, nil
}

func (f *fakeSession) ExplainContext() transport.ContextReport { return transport.ContextReport{} }

func (f *fakeSession) Snapshot(context.Context) (transport.SessionSnapshot, error) {
	return transport.SessionSnapshot{}, nil
}

func (f *fakeSession) Restore(context.Context, transport.SessionSnapshot) error { return nil }

func (f *fakeSession) Close() error { f.closed = true; return nil }

// The reshaped modeld/llama exposes exactly one boundary: transport.Service.
var _ transport.Service = (*Service)(nil)

func TestOpenSessionWithoutBackendIsUnavailable(t *testing.T) {
	SetSessionFactory(nil)
	if SessionAvailable() {
		t.Fatal("no backend should be available after clearing the factory")
	}
	path := writeTestGGUF(t, 32768)
	_, err := (&Service{}).OpenSession(context.Background(), transport.OpenSessionRequest{Path: path, Type: "llama"})
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("want ErrSessionUnavailable, got %v", err)
	}
}

func TestOpenSessionRejectsForeignBackend(t *testing.T) {
	SetSessionFactory(func(string, transport.Config, []AdapterSpec) (transport.Session, error) {
		t.Fatal("backend must not be reached for a foreign model type")
		return nil, nil
	})
	t.Cleanup(func() { SetSessionFactory(nil) })
	_, err := (&Service{}).OpenSession(context.Background(), transport.OpenSessionRequest{
		Path: "/ir/coder", Type: "openvino",
	})
	if !errors.Is(err, transport.ErrBackendMismatch) {
		t.Fatalf("want ErrBackendMismatch, got %v", err)
	}
}

func TestOpenSessionRoutesModelAndConfigToBackend(t *testing.T) {
	var gotModel string
	var gotCfg transport.Config
	fake := &fakeSession{}
	SetSessionFactory(func(modelPath string, cfg transport.Config, _ []AdapterSpec) (transport.Session, error) {
		gotModel, gotCfg = modelPath, cfg
		return fake, nil
	})
	t.Cleanup(func() { SetSessionFactory(nil) })
	path := writeTestGGUF(t, 32768)

	// NumGpuLayers is intentionally omitted: GPU layers are subject to capacity
	// resolution (zeroed without an accelerator), which is covered by the capacity
	// tests. This test checks request routing to the backend, so it uses
	// fields that pass through unchanged.
	cfg := transport.Config{NumCtx: 4096, PromptFormat: "chatml"}
	sess, err := (&Service{}).OpenSession(context.Background(), transport.OpenSessionRequest{
		ModelName: "foo",
		Type:      "llama",
		Path:      path,
		Config:    cfg,
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if gotModel != path {
		t.Errorf("model id not routed to backend: got %q", gotModel)
	}
	if gotCfg.NumCtx != 4096 || gotCfg.PromptFormat != "chatml" {
		t.Errorf("config not routed to backend: got %+v", gotCfg)
	}
	if sess != transport.Session(fake) {
		t.Fatal("OpenSession did not return the backend session")
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not reach the backend session")
	}
}

// LoRA adapters on the request must reach the session backend so the variant is
// actually served (not silently dropped, which would serve the base under a
// variant identity). This proves Service.OpenSession → toAdapterSpecs → the
// registered factory, without the CGo backend.
func TestOpenSessionRoutesAdaptersToBackend(t *testing.T) {
	var gotAdapters []AdapterSpec
	fake := &fakeSession{}
	SetSessionFactory(func(_ string, _ transport.Config, adapters []AdapterSpec) (transport.Session, error) {
		gotAdapters = adapters
		return fake, nil
	})
	t.Cleanup(func() { SetSessionFactory(nil) })
	path := writeTestGGUF(t, 32768)

	sess, err := (&Service{}).OpenSession(context.Background(), transport.OpenSessionRequest{
		ModelName: "foo",
		Type:      "llama",
		Path:      path,
		Config:    transport.Config{NumCtx: 4096, PromptFormat: "chatml"},
		Adapters: []transport.AdapterSpec{
			{Name: "style", Path: "/adapters/style.gguf", Digest: "adapter-1", Scale: 1.5},
		},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if len(gotAdapters) != 1 {
		t.Fatalf("adapters not routed to backend: got %+v", gotAdapters)
	}
	if a := gotAdapters[0]; a.Path != "/adapters/style.gguf" || a.Digest != "adapter-1" || a.Scale != 1.5 {
		t.Fatalf("adapter fields not routed faithfully: got %+v", a)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
