package contenoxcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

const autocompleteContextCap = 16 * 1024

const autocompleteDefaultMaxTokens = 128

const autocompleteRequestTimeout = 20 * time.Second

type acRequest struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Language  string `json:"language,omitempty"`
	Prefix    string `json:"prefix"`
	Suffix    string `json:"suffix"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

type acCompletionResponse struct {
	ID         string `json:"id"`
	Completion string `json:"completion"`
}

type acErrorResponse struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

type acCompleteFunc func(ctx context.Context, req acRequest) (string, error)

var autocompleteCmd = &cobra.Command{
	Use:   "autocomplete --stdio",
	Short: "Serve editor code completions over stdin/stdout (JSON lines) using the autocomplete model role.",
	Long: `Serve fill-in-the-middle code completions over a JSON-lines stdio protocol.

Reads one JSON request per line from stdin and writes one JSON response per
line to stdout (possibly out of order; match by id):

  request:  {"id":"1","path":"main.go","language":"go","prefix":"func main() {\n\t","suffix":"\n}","max_tokens":64}
  response: {"id":"1","completion":"fmt.Println(\"hello\")"}
        or  {"id":"1","error":"..."}

Uses the default-autocomplete-model / default-autocomplete-provider config
role (the same keys the ACP editor surface reads). With no autocomplete model
configured, every request is answered with an error naming the fix, so editor
clients see the condition on the protocol instead of a silent exit.
prefix/suffix are accepted up to 16 KiB each; longer sides are truncated
toward the cursor position.

Examples:
  contenox config set default-autocomplete-model qwen2.5-coder:7b
  contenox config set default-autocomplete-provider ollama
  contenox autocomplete --stdio`,
	RunE: runAutocompleteCmd,
}

func init() {
	autocompleteCmd.Flags().Bool("stdio", false, "Serve the JSON-lines protocol on stdin/stdout (required; the only mode)")
	rootCmd.AddCommand(autocompleteCmd)
}

func runAutocompleteCmd(cmd *cobra.Command, args []string) error {
	stdio, _ := cmd.Flags().GetBool("stdio")
	if !stdio {
		return fmt.Errorf("autocomplete serves a stdio protocol only; run: contenox autocomplete --stdio")
	}

	ctx := libtracker.WithNewRequestID(cmd.Context())

	contenoxDir, err := contenoxDirForWorkspace(cmd, "")
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database %q: %w", dbPath, err)
	}
	defer db.Close()

	store := runtimetypes.New(db.WithoutTransaction())
	model, provider, err := resolveAutocompleteRole(ctx, store)
	if err != nil {
		// The refusal must ride the stdout protocol: clients never read stderr.
		roleErr := err
		fmt.Fprintf(cmd.ErrOrStderr(), "autocomplete: %v\n", roleErr)
		return runAutocompleteStdio(ctx, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
			func(context.Context, acRequest) (string, error) { return "", roleErr })
	}

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	opts.EffectiveDB = dbPath

	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	fmt.Fprintf(cmd.ErrOrStderr(), "autocomplete: serving stdio protocol (model %s)\n", model)
	return runAutocompleteStdio(ctx, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
		newAutocompleteCompleter(engine.Models, model, provider))
}

func resolveAutocompleteRole(ctx context.Context, store runtimetypes.Store) (model, provider string, err error) {
	model = strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-model"))
	if model == "" {
		return "", "", errors.New("no autocomplete model is configured; set one with: contenox config set default-autocomplete-model <name>  (and optionally default-autocomplete-provider)")
	}
	provider = strings.TrimSpace(clikv.Read(ctx, store, "default-autocomplete-provider"))
	return model, provider, nil
}

const autocompleteSystemInstruction = "You are a code completion engine. The user message uses FIM sentinel tokens (<fim_prefix>, <fim_suffix>, <fim_middle>). Output ONLY the code that fills the middle section. No commentary, no markdown fences, no explanation."

func buildFIMPrompt(prefix, suffix string) string {
	return "<fim_prefix>" + prefix + "<fim_suffix>" + suffix + "<fim_middle>"
}

func newAutocompleteCompleter(models llmrepo.ModelRepo, model, provider string) acCompleteFunc {
	return func(ctx context.Context, req acRequest) (string, error) {
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = autocompleteDefaultMaxTokens
		}
		execCtx, cancel := context.WithTimeout(libtracker.WithNewRequestID(ctx), autocompleteRequestTimeout)
		defer cancel()

		lreq := llmrepo.Request{ModelNames: []string{model}}
		if provider != "" {
			lreq.ProviderTypes = []string{provider}
		}
		messages := []libmodelprovider.Message{
			{Role: "system", Content: autocompleteSystemInstruction},
			{Role: "user", Content: buildFIMPrompt(req.Prefix, req.Suffix)},
		}
		res, _, err := models.Chat(execCtx, lreq, messages, libmodelprovider.WithMaxTokens(maxTokens))
		if err != nil {
			return "", err
		}
		return strings.TrimRightFunc(res.Message.Content, unicode.IsSpace), nil
	}
}

func truncateAutocompleteContext(req *acRequest) (truncated bool) {
	if len(req.Prefix) > autocompleteContextCap {
		req.Prefix = req.Prefix[len(req.Prefix)-autocompleteContextCap:]
		truncated = true
	}
	if len(req.Suffix) > autocompleteContextCap {
		req.Suffix = req.Suffix[:autocompleteContextCap]
		truncated = true
	}
	return truncated
}

func runAutocompleteStdio(ctx context.Context, in io.Reader, out, errW io.Writer, complete acCompleteFunc) error {
	var (
		writeMu sync.Mutex
		wg      sync.WaitGroup
	)
	respond := func(v any) {
		payload, err := json.Marshal(v)
		if err != nil {
			// Drop the line rather than emit a half-written object.
			fmt.Fprintf(errW, "autocomplete: dropping unencodable response: %v\n", err)
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(out, "%s\n", payload)
	}

	reader := bufio.NewReader(in)
	for {
		line, readErr := reader.ReadBytes('\n')
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			var req acRequest
			switch {
			case json.Unmarshal([]byte(trimmed), &req) != nil:
				respond(acErrorResponse{ID: "", Error: "invalid request: not a JSON object of the autocomplete protocol"})
			case strings.TrimSpace(req.ID) == "":
				respond(acErrorResponse{ID: "", Error: "invalid request: id is required"})
			case req.Prefix == "" && req.Suffix == "":
				respond(acErrorResponse{ID: req.ID, Error: "invalid request: prefix or suffix is required"})
			default:
				if truncateAutocompleteContext(&req) {
					fmt.Fprintf(errW, "autocomplete: request %s exceeded %d bytes per side; context truncated toward the cursor\n", req.ID, autocompleteContextCap)
				}
				wg.Add(1)
				go func(req acRequest) {
					defer wg.Done()
					completion, err := complete(ctx, req)
					if err != nil {
						respond(acErrorResponse{ID: req.ID, Error: err.Error()})
						return
					}
					respond(acCompletionResponse{ID: req.ID, Completion: completion})
				}(req)
			}
		}
		if readErr != nil {
			wg.Wait()
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("autocomplete: reading stdin: %w", readErr)
		}
	}
}
