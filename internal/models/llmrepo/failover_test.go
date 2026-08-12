package llmrepo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type recordingTracker struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingTracker) Start(_ context.Context, operation, subject string, _ ...any) (func(error), func(string, any), func()) {
	r.mu.Lock()
	r.events = append(r.events, operation+"/"+subject)
	r.mu.Unlock()
	return func(error) {}, func(string, any) {}, func() {}
}

func (r *recordingTracker) has(event string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == event {
			return true
		}
	}
	return false
}

type fakeChatClient struct {
	calls  int
	result libmodelprovider.ChatResult
	err    error
}

func (c *fakeChatClient) Chat(context.Context, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, error) {
	c.calls++
	return c.result, c.err
}

type fakeStreamClient struct {
	calls   int
	parcels []*libmodelprovider.StreamParcel
}

func (c *fakeStreamClient) Stream(context.Context, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, error) {
	c.calls++
	ch := make(chan *libmodelprovider.StreamParcel, len(c.parcels))
	for _, p := range c.parcels {
		ch <- p
	}
	close(ch)
	return ch, nil
}

type fakePromptClient struct {
	calls  int
	result string
	err    error
}

func (c *fakePromptClient) Prompt(context.Context, string, float32, string) (string, *libmodelprovider.TokenUsage, error) {
	c.calls++
	return c.result, nil, c.err
}

type failoverProvider struct {
	*libmodelprovider.MockProvider
	chat   *fakeChatClient
	stream *fakeStreamClient
	prompt *fakePromptClient
}

func (p *failoverProvider) GetChatConnection(context.Context, string) (libmodelprovider.LLMChatClient, error) {
	return p.chat, nil
}

func (p *failoverProvider) GetStreamConnection(context.Context, string) (libmodelprovider.LLMStreamClient, error) {
	return p.stream, nil
}

func (p *failoverProvider) GetPromptConnection(context.Context, string) (libmodelprovider.LLMPromptExecClient, error) {
	return p.prompt, nil
}

func newFailoverProvider(backend string) *failoverProvider {
	return &failoverProvider{
		MockProvider: &libmodelprovider.MockProvider{
			ID:            "vertex-google:gemini-test",
			Name:          "gemini-test",
			CanChatFlag:   true,
			CanStreamFlag: true,
			CanPromptFlag: true,
			Backends:      []string{backend},
		},
		chat:   &fakeChatClient{},
		stream: &fakeStreamClient{},
		prompt: &fakePromptClient{},
	}
}

func notFound404(backend string) error {
	err := fmt.Errorf("vertex API returned non-200 status for stream: 404, backend %s", backend)
	return libmodelprovider.ClassifyProviderError(err, http.StatusNotFound, "", "")
}

func selectionsFor(providers ...*failoverProvider) []llmresolver.Selection {
	sels := make([]llmresolver.Selection, 0, len(providers))
	for _, p := range providers {
		sels = append(sels, llmresolver.Selection{Provider: p, Backend: p.Backends[0]})
	}
	return sels
}

func testMessages() []libmodelprovider.Message {
	return []libmodelprovider.Message{{Role: "user", Content: "hello"}}
}

// A model-not-found refusal from the first backend must fail over to the
// second, calling the refused backend exactly once.
func TestUnit_ChatFailover_SecondBackendServes(t *testing.T) {
	refusing := newFailoverProvider("https://us-central1.example/v1")
	refusing.chat.err = notFound404(refusing.Backends[0])
	serving := newFailoverProvider("https://global.example/v1")
	serving.chat.result = libmodelprovider.ChatResult{Message: libmodelprovider.Message{Role: "assistant", Content: "served"}}

	tracker := &recordingTracker{}
	mm := &modelManager{tracker: tracker}

	resp, meta, err := mm.runChatSelections(context.Background(), Request{}, selectionsFor(refusing, serving), "", testMessages(), nil)
	require.NoError(t, err)
	require.Equal(t, "served", resp.Message.Content)
	require.Equal(t, serving.Backends[0], meta.BackendID)
	require.Equal(t, 1, refusing.chat.calls, "a 404 is terminal for that backend and must not be retried against it")
	require.Equal(t, 1, serving.chat.calls)
	require.True(t, tracker.has("failover/chat"), "the failover must be recorded")
}

// When every backend refuses, the error aggregates each backend's refusal
// and keeps the typed class reachable.
func TestUnit_ChatFailover_AllBackendsExhausted(t *testing.T) {
	first := newFailoverProvider("https://us-central1.example/v1")
	first.chat.err = notFound404(first.Backends[0])
	second := newFailoverProvider("https://global.example/v1")
	second.chat.err = notFound404(second.Backends[0])

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	_, _, err := mm.runChatSelections(context.Background(), Request{}, selectionsFor(first, second), "", testMessages(), nil)
	require.Error(t, err)
	require.ErrorIs(t, err, libmodelprovider.ErrModelNotFoundOnBackend)
	require.Contains(t, err.Error(), first.Backends[0])
	require.Contains(t, err.Error(), second.Backends[0])
	require.Contains(t, err.Error(), "every capable backend")
	require.Equal(t, 1, first.chat.calls)
	require.Equal(t, 1, second.chat.calls)
}

// A failure that is not terminal-for-the-backend keeps its original
// semantics: the request fails immediately and no other backend is tried.
func TestUnit_ChatFailover_NonTerminalErrorFailsImmediately(t *testing.T) {
	failing := newFailoverProvider("https://us-central1.example/v1")
	failing.chat.err = errors.New("vertex API error: 500 - internal")
	standby := newFailoverProvider("https://global.example/v1")

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	_, _, err := mm.runChatSelections(context.Background(), Request{}, selectionsFor(failing, standby), "", testMessages(), nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, libmodelprovider.ErrModelNotFoundOnBackend)
	require.Equal(t, 1, failing.chat.calls)
	require.Zero(t, standby.chat.calls, "a non-terminal failure must not trigger failover")
}

// An access-denied refusal (HTTP 403) carries the same failover semantics as
// model-not-found.
func TestUnit_ChatFailover_AccessDeniedAlsoFailsOver(t *testing.T) {
	denied := newFailoverProvider("https://us-central1.example/v1")
	denied.chat.err = libmodelprovider.ClassifyProviderError(errors.New("vertex API error: 403 PERMISSION_DENIED"), http.StatusForbidden, "PERMISSION_DENIED", "")
	serving := newFailoverProvider("https://global.example/v1")
	serving.chat.result = libmodelprovider.ChatResult{Message: libmodelprovider.Message{Role: "assistant", Content: "served"}}

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	resp, meta, err := mm.runChatSelections(context.Background(), Request{}, selectionsFor(denied, serving), "", testMessages(), nil)
	require.NoError(t, err)
	require.Equal(t, "served", resp.Message.Content)
	require.Equal(t, serving.Backends[0], meta.BackendID)
	require.Equal(t, 1, denied.chat.calls)
}

// A stream whose FIRST parcel is a backend-terminal error fails over before
// the consumer sees anything; the second backend's parcels arrive as the one
// visible result.
func TestUnit_StreamFailover_FirstParcelRefusalFailsOver(t *testing.T) {
	refusing := newFailoverProvider("https://us-central1.example/v1")
	refusing.stream.parcels = []*libmodelprovider.StreamParcel{{Error: notFound404(refusing.Backends[0])}}
	serving := newFailoverProvider("https://global.example/v1")
	serving.stream.parcels = []*libmodelprovider.StreamParcel{
		{Data: "hello "},
		{Data: "world"},
		{Terminal: &libmodelprovider.StreamTerminal{FinishReason: "stop"}},
	}

	tracker := &recordingTracker{}
	mm := &modelManager{tracker: tracker}

	stream, meta, err := mm.runStreamSelections(context.Background(), Request{}, selectionsFor(refusing, serving), "", testMessages(), nil)
	require.NoError(t, err)
	require.Equal(t, serving.Backends[0], meta.BackendID)

	var content string
	var sawTerminal bool
	for parcel := range stream {
		require.NoError(t, parcel.Error, "the refused backend's error must never reach the consumer")
		content += parcel.Data
		if parcel.Terminal != nil {
			sawTerminal = true
		}
	}
	require.Equal(t, "hello world", content)
	require.True(t, sawTerminal)
	require.Equal(t, 1, refusing.stream.calls, "a 404 is terminal for that backend and must not be retried against it")
	require.Equal(t, 1, serving.stream.calls)
	require.True(t, tracker.has("failover/stream"), "the failover must be recorded")
}

// When every backend's stream refuses, the aggregated error lists each
// backend's refusal.
func TestUnit_StreamFailover_AllBackendsExhausted(t *testing.T) {
	first := newFailoverProvider("https://us-central1.example/v1")
	first.stream.parcels = []*libmodelprovider.StreamParcel{{Error: notFound404(first.Backends[0])}}
	second := newFailoverProvider("https://global.example/v1")
	second.stream.parcels = []*libmodelprovider.StreamParcel{{Error: notFound404(second.Backends[0])}}

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	_, _, err := mm.runStreamSelections(context.Background(), Request{}, selectionsFor(first, second), "", testMessages(), nil)
	require.Error(t, err)
	require.ErrorIs(t, err, libmodelprovider.ErrModelNotFoundOnBackend)
	require.Contains(t, err.Error(), first.Backends[0])
	require.Contains(t, err.Error(), second.Backends[0])
	require.Equal(t, 1, first.stream.calls)
	require.Equal(t, 1, second.stream.calls)
}

// Once content has flowed, a terminal-classified error later in the stream is
// final: no failover, the consumer sees the content and then the error.
func TestUnit_StreamFailover_MidStreamErrorStaysFinal(t *testing.T) {
	failing := newFailoverProvider("https://us-central1.example/v1")
	failing.stream.parcels = []*libmodelprovider.StreamParcel{
		{Data: "partial"},
		{Error: notFound404(failing.Backends[0])},
	}
	standby := newFailoverProvider("https://global.example/v1")

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	stream, _, err := mm.runStreamSelections(context.Background(), Request{}, selectionsFor(failing, standby), "", testMessages(), nil)
	require.NoError(t, err)

	var parcels []*libmodelprovider.StreamParcel
	for parcel := range stream {
		parcels = append(parcels, parcel)
	}
	require.Len(t, parcels, 2)
	require.Equal(t, "partial", parcels[0].Data)
	require.Error(t, parcels[1].Error)
	require.Zero(t, standby.stream.calls, "content already flowed; a mid-stream failure must stay final")
}

// The prompt path shares the failover semantics.
func TestUnit_PromptFailover_SecondBackendServes(t *testing.T) {
	refusing := newFailoverProvider("https://us-central1.example/v1")
	refusing.prompt.err = notFound404(refusing.Backends[0])
	serving := newFailoverProvider("https://global.example/v1")
	serving.prompt.result = "served"

	mm := &modelManager{tracker: libtracker.NoopTracker{}}

	result, meta, err := mm.runPromptSelections(context.Background(), Request{}, selectionsFor(refusing, serving), "sys", 0, "prompt")
	require.NoError(t, err)
	require.Equal(t, "served", result)
	require.Equal(t, serving.Backends[0], meta.BackendID)
	require.Equal(t, 1, refusing.prompt.calls)
	require.Equal(t, 1, serving.prompt.calls)
}
