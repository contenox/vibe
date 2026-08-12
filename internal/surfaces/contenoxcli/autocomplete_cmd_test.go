package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeACLines(t *testing.T, out string) (okByID map[string]string, errsByID map[string]string) {
	t.Helper()
	okByID = map[string]string{}
	errsByID = map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &obj), "line=%q", line)
		id, _ := obj["id"].(string)
		_, hasCompletion := obj["completion"]
		_, hasError := obj["error"]
		require.False(t, hasCompletion && hasError, "a response carries exactly one of completion/error: %q", line)
		require.True(t, hasCompletion || hasError, "a response carries completion or error: %q", line)
		if hasCompletion {
			okByID[id] = obj["completion"].(string)
		} else {
			errsByID[id] = obj["error"].(string)
		}
	}
	return okByID, errsByID
}

// TestUnit_AutocompleteStdio_RoundTrip asserts requests in produce id-matched completions out, one JSON object per line, all requests answered.
func TestUnit_AutocompleteStdio_RoundTrip(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	got := map[string]acRequest{}
	complete := func(_ context.Context, req acRequest) (string, error) {
		mu.Lock()
		got[req.ID] = req
		mu.Unlock()
		return "-> " + req.ID, nil
	}

	in := strings.NewReader(
		`{"id":"1","path":"main.go","language":"go","prefix":"func main() {","suffix":"}","max_tokens":64}` + "\n" +
			`{"id":"2","path":"lib.py","prefix":"def f(","suffix":""}` + "\n")
	var out, errW bytes.Buffer
	require.NoError(t, runAutocompleteStdio(context.Background(), in, &out, &errW, complete))

	okByID, errsByID := decodeACLines(t, out.String())
	require.Empty(t, errsByID)
	require.Equal(t, map[string]string{"1": "-> 1", "2": "-> 2"}, okByID)

	require.Equal(t, "func main() {", got["1"].Prefix)
	require.Equal(t, "}", got["1"].Suffix)
	require.Equal(t, 64, got["1"].MaxTokens)
	require.Equal(t, "go", got["1"].Language)
}

// TestUnit_AutocompleteStdio_MalformedLineAnsweredNotFatal asserts a non-JSON line, a missing id, and an empty prefix/suffix each answer with an error, and a valid request after them still completes.
func TestUnit_AutocompleteStdio_MalformedLineAnsweredNotFatal(t *testing.T) {
	t.Parallel()

	complete := func(_ context.Context, req acRequest) (string, error) { return "ok", nil }

	in := strings.NewReader(
		"this is not json\n" +
			`{"path":"x.go","prefix":"a"}` + "\n" +
			`{"id":"3","prefix":"","suffix":""}` + "\n" +
			`{"id":"4","prefix":"a","suffix":"b"}` + "\n")
	var out, errW bytes.Buffer
	require.NoError(t, runAutocompleteStdio(context.Background(), in, &out, &errW, complete))

	okByID, errsByID := decodeACLines(t, out.String())
	require.Equal(t, map[string]string{"4": "ok"}, okByID)
	// The two id-less failures both answer on the empty id (last wins in the map); assert presence and count via the raw output instead.
	require.Contains(t, errsByID, "")
	require.Contains(t, errsByID, "3")
	assert.Contains(t, errsByID["3"], "prefix or suffix is required")
	assert.Equal(t, 3, strings.Count(out.String(), `"error"`))
}

// TestUnit_AutocompleteStdio_CompleterErrorRidesTheID asserts a failing completion answers {"id","error"} for its own id without disturbing other requests.
func TestUnit_AutocompleteStdio_CompleterErrorRidesTheID(t *testing.T) {
	t.Parallel()

	complete := func(_ context.Context, req acRequest) (string, error) {
		if req.ID == "boom" {
			return "", errors.New("model exploded")
		}
		return "fine", nil
	}

	in := strings.NewReader(
		`{"id":"boom","prefix":"a"}` + "\n" +
			`{"id":"ok","prefix":"b"}` + "\n")
	var out, errW bytes.Buffer
	require.NoError(t, runAutocompleteStdio(context.Background(), in, &out, &errW, complete))

	okByID, errsByID := decodeACLines(t, out.String())
	require.Equal(t, map[string]string{"ok": "fine"}, okByID)
	require.Equal(t, map[string]string{"boom": "model exploded"}, errsByID)
}

// TestUnit_AutocompleteStdio_OversizedContextTruncatedTowardCursor asserts a prefix beyond the cap keeps its END, a suffix keeps its START, the completion still runs, and truncation is noted on stderr.
func TestUnit_AutocompleteStdio_OversizedContextTruncatedTowardCursor(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got acRequest
	complete := func(_ context.Context, req acRequest) (string, error) {
		mu.Lock()
		got = req
		mu.Unlock()
		return "done", nil
	}

	prefix := strings.Repeat("p", autocompleteContextCap) + "NEAR_CURSOR"
	suffix := "AFTER_CURSOR" + strings.Repeat("s", autocompleteContextCap)
	reqLine, err := json.Marshal(acRequest{ID: "big", Prefix: prefix, Suffix: suffix})
	require.NoError(t, err)

	var out, errW bytes.Buffer
	require.NoError(t, runAutocompleteStdio(context.Background(), strings.NewReader(string(reqLine)+"\n"), &out, &errW, complete))

	okByID, errsByID := decodeACLines(t, out.String())
	require.Empty(t, errsByID)
	require.Equal(t, map[string]string{"big": "done"}, okByID)

	require.Len(t, got.Prefix, autocompleteContextCap)
	require.True(t, strings.HasSuffix(got.Prefix, "NEAR_CURSOR"), "prefix keeps its end (nearest the cursor)")
	require.Len(t, got.Suffix, autocompleteContextCap)
	require.True(t, strings.HasPrefix(got.Suffix, "AFTER_CURSOR"), "suffix keeps its start (nearest the cursor)")
	require.Contains(t, errW.String(), "truncated")
}

// TestUnit_ResolveAutocompleteRole asserts the verb refuses without default-autocomplete-model set, naming the config key, and resolves once the sibling keys are set.
func TestUnit_ResolveAutocompleteRole(t *testing.T) {
	ctx, _, store := openTestDB(t)

	_, _, err := resolveAutocompleteRole(ctx, store)
	require.Error(t, err)
	require.Contains(t, err.Error(), "default-autocomplete-model")

	data, _ := json.Marshal("qwen2.5-coder:7b")
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-autocomplete-model", data))
	data, _ = json.Marshal("ollama")
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-autocomplete-provider", data))

	model, provider, err := resolveAutocompleteRole(ctx, store)
	require.NoError(t, err)
	require.Equal(t, "qwen2.5-coder:7b", model)
	require.Equal(t, "ollama", provider)
}

func TestUnit_AutocompleteStdio_NoModelRefusalRidesTheProtocol(t *testing.T) {
	ctx, _, store := openTestDB(t)
	_, _, roleErr := resolveAutocompleteRole(ctx, store)
	require.Error(t, roleErr)

	in := strings.NewReader(`{"id":"1","prefix":"func main() {"}` + "\n")
	var out, errW bytes.Buffer
	require.NoError(t, runAutocompleteStdio(ctx, in, &out, &errW,
		func(context.Context, acRequest) (string, error) { return "", roleErr }))

	_, errsByID := decodeACLines(t, out.String())
	require.Contains(t, errsByID, "1")
	disablePattern := regexp.MustCompile(`(?i)no[-_ ]?(autocomplete[-_ ])?model`)
	require.True(t, disablePattern.MatchString(errsByID["1"]),
		"packages/vscode ghost/protocol.ts isNoModelConfiguredError must match this error, got: %q", errsByID["1"])
}

type acStubRepo struct {
	gotReq      llmrepo.Request
	gotMessages []libmodelprovider.Message
	gotArgs     []libmodelprovider.ChatArgument
	reply       string
}

func (s *acStubRepo) Chat(_ context.Context, req llmrepo.Request, messages []libmodelprovider.Message, args ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	s.gotReq = req
	s.gotMessages = messages
	s.gotArgs = args
	return libmodelprovider.ChatResult{Message: libmodelprovider.Message{Role: "assistant", Content: s.reply}}, llmrepo.Meta{}, nil
}

func (s *acStubRepo) Tokenize(context.Context, string, string) ([]int, error) {
	return nil, errors.New("stub: not implemented")
}

func (s *acStubRepo) CountTokens(context.Context, string, string) (int, error) {
	return 0, errors.New("stub: not implemented")
}

func (s *acStubRepo) PromptExecute(context.Context, llmrepo.Request, string, float32, string) (string, llmrepo.Meta, error) {
	return "", llmrepo.Meta{}, errors.New("stub: not implemented")
}

func (s *acStubRepo) Embed(context.Context, llmrepo.EmbedRequest, string) ([]float64, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: not implemented")
}

func (s *acStubRepo) Stream(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: not implemented")
}

// TestUnit_AutocompleteCompleter_PromptShapeAndRolePin asserts the instruct-style FIM prompt shape, the role pin on the resolved model/provider, and trailing-whitespace trim on the completion.
func TestUnit_AutocompleteCompleter_PromptShapeAndRolePin(t *testing.T) {
	t.Parallel()

	repo := &acStubRepo{reply: "\tfmt.Println(\"hi\")\n\n"}
	complete := newAutocompleteCompleter(repo, "qwen2.5-coder:7b", "ollama")

	got, err := complete(context.Background(), acRequest{ID: "1", Prefix: "func main() {\n", Suffix: "\n}"})
	require.NoError(t, err)
	require.Equal(t, "\tfmt.Println(\"hi\")", got, "trailing whitespace is trimmed")

	require.Equal(t, []string{"qwen2.5-coder:7b"}, repo.gotReq.ModelNames)
	require.Equal(t, []string{"ollama"}, repo.gotReq.ProviderTypes)

	require.Len(t, repo.gotMessages, 2)
	require.Equal(t, "system", repo.gotMessages[0].Role)
	require.Equal(t, autocompleteSystemInstruction, repo.gotMessages[0].Content)
	require.Equal(t, "user", repo.gotMessages[1].Role)
	require.Equal(t, "<fim_prefix>func main() {\n<fim_suffix>\n}<fim_middle>", repo.gotMessages[1].Content)
	require.Len(t, repo.gotArgs, 1, "exactly the max-tokens argument rides the call")

	// An empty provider leaves ProviderTypes unset so llmrepo falls back to the default provider.
	repo2 := &acStubRepo{reply: "x"}
	_, err = newAutocompleteCompleter(repo2, "m", "")(context.Background(), acRequest{ID: "2", Prefix: "a"})
	require.NoError(t, err)
	require.Nil(t, repo2.gotReq.ProviderTypes)
}
