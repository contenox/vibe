// index_cmd.go holds the two operator verbs over the workspace semantic
// index — `contenox index` (build/refresh) and `contenox search` (read) —
// plus the wiring that hands the same read to the agent as the
// `workspace_search` tool. Every decision that costs anything or could be
// wrong belongs to internal/services/workspaceindex; this file only resolves
// the directory, shows the price before it is paid, asks, and renders.
package contenoxcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/services/workspaceindex"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

// searchSnippetLines caps how many lines of a hit's chunk `contenox search` prints.
const searchSnippetLines = 6

var indexCmd = &cobra.Command{
	Use:   "index [dir]",
	Args:  cobra.MaximumNArgs(1),
	Short: "Build or refresh this workspace's semantic index.",
	Long: `Index the workspace so 'contenox search' — and the agent's workspace_search
tool — can answer questions about its content: docs, markdown, configuration,
and source in any language.

Indexing costs one embedding call per chunk, which is real money on a hosted
provider. So it prints the count FIRST and asks before spending it:

  1,270 files → 7,065 chunks → 7,065 embed calls against nomic-embed-text · ollama

Refreshes are incremental: only files whose content changed are re-embedded,
and files that disappeared drop their chunks. --force rebuilds everything.

Which files are indexed is the same set '@' completion and find_files see —
gitignored paths, binaries and oversized files are skipped, and the counts
above say how many.

Needs an embedding model. Most chat models cannot embed:

  contenox config set default-embed-model nomic-embed-text

Examples:
  contenox index
  contenox index ~/src/project
  contenox index --force
  contenox index --yes          # no confirmation (scripts, CI)`,
	RunE: runIndexCmd,
}

var searchCmd = &cobra.Command{
	Use:   "search <question>",
	Args:  cobra.ExactArgs(1),
	Short: "Ask the workspace index a question; get file:line-range citations back.",
	Long: `Search this workspace's semantic index and print ranked citations.

Each hit is a file:line-range and a snippet — a location you can open, not a
floating blob. A hit whose file changed since it was indexed is marked STALE
rather than served as if it were current.

This reads the index, never the filesystem: content added since the last
'contenox index' is not here.

Examples:
  contenox search "where is retry backoff configured"
  contenox search "how does the approval flow work" --top 3
  contenox search "session storage" --json | jq -r '.[].path'`,
	RunE: runSearchCmd,
}

func init() {
	indexCmd.Flags().Bool("force", false, "Re-embed every file instead of only the changed ones")
	indexCmd.Flags().Bool("yes", false, "Skip the cost confirmation (required when stdin is not a terminal)")

	searchCmd.Flags().Int("top", 0, "Maximum hits to return (default 8, ceiling 50)")
	searchCmd.Flags().Bool("json", false, "Emit the hits as JSON for scripting")
	searchCmd.Flags().String("dir", "", "Workspace directory to search (default: current directory)")

	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(searchCmd)
}

// ─── wiring ─────────────────────────────────────────────────────────────────

// indexDeps is everything either verb needs after the runtime is up, so the
// verbs are testable against a service with no engine, model or network.
type indexDeps struct {
	Svc         workspaceindex.Service
	WorkspaceID string
	Model       string
	Provider    string
	// EmbedFallback: no embedding model was configured, so the chat model
	// stands in.
	EmbedFallback bool
}

// openWorkspaceIndex opens the shared SQLite, builds the engine, and composes
// the index service over its resolved model route.
func openWorkspaceIndex(ctx context.Context, cmd *cobra.Command, dir string) (io.Closer, *indexDeps, error) {
	contenoxDir, err := contenoxDirForWorkspace(cmd, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database %q: %w", dbPath, err)
	}

	opts, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	opts.EffectiveDB = dbPath

	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("failed to build engine: %w", err)
	}

	model := strings.TrimSpace(engine.EmbeddingModel.Name)
	if model == "" {
		engine.Stop()
		db.Close()
		return nil, nil, errors.New("no embedding model resolved, so nothing can be indexed or searched.\n" +
			"Set one (most chat models cannot embed):\n" +
			"  contenox config set default-embed-model nomic-embed-text\n" +
			"  contenox config set default-embed-provider ollama   # only if it differs from default-provider")
	}

	store := runtimetypes.New(db.WithoutTransaction())
	deps := &indexDeps{
		WorkspaceID:   ResolveWorkspaceID(contenoxDir),
		Model:         model,
		Provider:      engine.EmbeddingModel.Provider,
		EmbedFallback: strings.TrimSpace(getConfigValue(ctx, store, "default-embed-model")) == "",
		Svc: workspaceindex.New(
			store,
			workspaceindex.NewLLMRepoEmbedder(engine.Models, model, engine.EmbeddingModel.Provider),
			ollamatokenizer.NewEstimateTokenizer(),
			workspaceindex.Config{EmbedModel: model, EmbedProvider: engine.EmbeddingModel.Provider},
		),
	}
	return closerFunc(func() error {
		engine.Stop()
		return db.Close()
	}), deps, nil
}

// closerFunc adapts a teardown closure to io.Closer: engine down, then database.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func getConfigValue(ctx context.Context, store runtimetypes.Store, key string) string {
	v, _ := getConfigKV(ctx, store, key)
	return v
}

// contenoxDirForWorkspace resolves the .contenox dir identifying the
// workspace rooted at dir, so `index <dir>` and `search --dir <dir>` agree.
func contenoxDirForWorkspace(cmd *cobra.Command, dir string) (string, error) {
	if cmd != nil {
		if dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir"); dataDir != "" {
			return filepath.Abs(dataDir)
		}
	}
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	start := cur
	for {
		candidate := filepath.Join(cur, ".contenox")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			// A .contenox/ without workspace.id isn't a workspace; keep walking.
			if _, werr := os.Stat(filepath.Join(candidate, "workspace.id")); werr == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Join(start, ".contenox"), nil
		}
		cur = parent
	}
}

// resolveWorkspaceDir turns an optional directory argument into an absolute
// directory, mirroring `beam [dir]`'s refusal of a non-directory path.
func resolveWorkspaceDir(arg string) (string, error) {
	if strings.TrimSpace(arg) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return cwd, nil
	}
	dir, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve workspace directory %q: %w", arg, err)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", arg)
	}
	return dir, nil
}

// ─── contenox index ─────────────────────────────────────────────────────────

func runIndexCmd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = libtracker.WithNewRequestID(ctx)

	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	dir, err := resolveWorkspaceDir(arg)
	if err != nil {
		return err
	}

	closer, deps, err := openWorkspaceIndex(ctx, cmd, dir)
	if err != nil {
		return err
	}
	defer closer.Close()

	force, _ := cmd.Flags().GetBool("force")
	yes, _ := cmd.Flags().GetBool("yes")
	return runIndexWith(ctx, cmd, deps, dir, force, yes)
}

// runIndexWith is the verb's whole behavior over an injected service: plan,
// price, ask, build, report.
func runIndexWith(ctx context.Context, cmd *cobra.Command, deps *indexDeps, dir string, force, yes bool) error {
	out := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// Plans without a single embedding call, so the number below is honest.
	plan, err := deps.Svc.Plan(ctx, dir, workspaceindex.BuildOptions{
		WorkspaceID: deps.WorkspaceID,
		Force:       force,
	})
	if err != nil {
		return fmt.Errorf("failed to plan the index: %w", err)
	}
	if deps.EmbedFallback {
		fmt.Fprintf(errW, "warning: no default-embed-model is set, so %q (the chat model) will be asked to embed — most chat models cannot.\n"+
			"         contenox config set default-embed-model nomic-embed-text\n\n", deps.Model)
	}
	renderIndexPlan(out, plan, deps.Model, deps.Provider)

	switch {
	case plan.EmbedCalls == 0 && plan.FilesDeleted == 0:
		fmt.Fprintln(out, "\nAlready current: nothing changed since the last index.")
		return nil
	case plan.EmbedCalls == 0:
		fmt.Fprintf(out, "\nNothing to embed; dropping the chunks of %d removed file(s).\n", plan.FilesDeleted)
	default:
		if !yes {
			ok, err := confirmSpend(cmd, fmt.Sprintf("Make %s embedding call(s) against %s?", commafy(plan.EmbedCalls), embedTarget(deps.Model, deps.Provider)))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(out, "Cancelled; nothing was embedded.")
				return nil
			}
		}
	}

	progress := newIndexProgress(errW)
	report, err := deps.Svc.Build(ctx, dir, workspaceindex.BuildOptions{
		WorkspaceID: deps.WorkspaceID,
		Force:       force,
		Progress:    progress.emit,
	})
	progress.done()
	if err != nil {
		return fmt.Errorf("index failed: %w", err)
	}
	renderBuildReport(out, report)
	return nil
}

// renderIndexPlan prints the cost before it is spent.
func renderIndexPlan(w io.Writer, plan *workspaceindex.BuildPlan, model, provider string) {
	fmt.Fprintf(w, "Workspace  %s\n", plan.WorkspaceID)
	fmt.Fprintf(w, "Root       %s\n", plan.Root)
	fmt.Fprintf(w, "\n%s files → %s chunks → %s embed calls against %s\n",
		commafy(plan.Files), commafy(plan.Chunks), commafy(plan.EmbedCalls), embedTarget(model, provider))

	if plan.ChunksReused > 0 {
		fmt.Fprintf(w, "Reusing %s chunk(s) from files that did not change.\n", commafy(plan.ChunksReused))
	}
	if plan.FilesDeleted > 0 {
		fmt.Fprintf(w, "Dropping the chunks of %s file(s) that no longer exist.\n", commafy(plan.FilesDeleted))
	}
	if plan.SkippedBinary > 0 || plan.SkippedOversize > 0 || plan.SkippedGenerated > 0 {
		fmt.Fprintf(w, "Skipped %s binary, %s oversized and %s generated file(s).\n",
			commafy(plan.SkippedBinary), commafy(plan.SkippedOversize), commafy(plan.SkippedGenerated))
	}
	if plan.CutOver && plan.ConfigID == "" {
		fmt.Fprintln(w, "This creates a new index generation (an index is never mutated across a model or chunking change).")
	}
}

// embedTarget names the model the calls go to, omitting a dangling separator
// when provider is unset.
func embedTarget(model, provider string) string {
	if strings.TrimSpace(provider) == "" {
		return model
	}
	return model + " · " + provider
}

func renderBuildReport(w io.Writer, report *workspaceindex.BuildReport) {
	fmt.Fprintf(w, "\nIndexed in %s: %s chunk(s) written, %s dropped, %s embed call(s).\n",
		formatDuration(report.Duration),
		commafy(report.ChunksWritten), commafy(report.ChunksDeleted), commafy(report.EmbedCalls))
	fmt.Fprintln(w, "Ask it something: contenox search \"your question\"")
}

// indexProgress draws build progress as one rewritten line on stderr, only
// when stderr is a terminal.
type indexProgress struct {
	w        io.Writer
	enabled  bool
	mu       sync.Mutex
	lastWide int
}

func newIndexProgress(w io.Writer) *indexProgress {
	return &indexProgress{w: w, enabled: xterm.IsTerminal(int(os.Stderr.Fd()))}
}

func (p *indexProgress) emit(ev workspaceindex.Progress) {
	if !p.enabled {
		return
	}
	line := renderProgressLine(ev)
	if line == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pad := ""
	if n := p.lastWide - len([]rune(line)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	p.lastWide = len([]rune(line))
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
}

func (p *indexProgress) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enabled && p.lastWide > 0 {
		fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastWide))
	}
}

func renderProgressLine(ev workspaceindex.Progress) string {
	switch ev.Phase {
	case workspaceindex.PhaseEmbedding:
		return fmt.Sprintf("embedding %s/%s files · %s/%s chunks · %s",
			commafy(ev.Files), commafy(ev.FilesTotal),
			commafy(ev.Chunks), commafy(ev.ChunksTotal),
			truncateMiddle(ev.Path, 48))
	default:
		return ""
	}
}

// truncateMiddle shortens a path from the middle, keeping the leading
// directory and filename that identify it.
func truncateMiddle(s string, width int) string {
	r := []rune(s)
	if len(r) <= width || width < 5 {
		return s
	}
	keep := width - 1
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// confirmSpend asks before money is spent; a closed stdin is never "yes".
func confirmSpend(cmd *cobra.Command, question string) (bool, error) {
	fmt.Fprintf(cmd.OutOrStdout(), "\n%s [y/N] ", question)
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		fmt.Fprintln(cmd.OutOrStdout())
		return false, errors.New("no answer received — 'contenox index' asks before spending; pass --yes to run it non-interactively")
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes", nil
}

// commafy groups an integer with thousands separators.
func commafy(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ─── contenox search ────────────────────────────────────────────────────────

func runSearchCmd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = libtracker.WithNewRequestID(ctx)

	dirFlag, _ := cmd.Flags().GetString("dir")
	dir, err := resolveWorkspaceDir(dirFlag)
	if err != nil {
		return err
	}

	closer, deps, err := openWorkspaceIndex(ctx, cmd, dir)
	if err != nil {
		return err
	}
	defer closer.Close()

	topK, _ := cmd.Flags().GetInt("top")
	asJSON, _ := cmd.Flags().GetBool("json")
	return runSearchWith(ctx, cmd, deps, args[0], topK, asJSON)
}

func runSearchWith(ctx context.Context, cmd *cobra.Command, deps *indexDeps, question string, topK int, asJSON bool) error {
	hits, err := deps.Svc.Query(ctx, deps.WorkspaceID, question, topK)
	if err != nil {
		switch {
		case errors.Is(err, workspaceindex.ErrNoIndex):
			return errors.New("no index for this workspace — run: contenox index")
		case errors.Is(err, workspaceindex.ErrEmptyQuestion):
			return errors.New("search needs a question — try: contenox search \"where is retry backoff configured\"")
		default:
			return fmt.Errorf("search failed: %w", err)
		}
	}
	if asJSON {
		// Empty is `[]`, never `null`, so a script never special-cases it.
		if hits == nil {
			hits = []workspaceindex.Hit{}
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}
	renderSearchHits(cmd.OutOrStdout(), question, hits)
	return nil
}

// renderSearchHits prints one citation per hit with an indented, capped
// snippet; stale hits are labelled inline, not in a footnote.
func renderSearchHits(w io.Writer, question string, hits []workspaceindex.Hit) {
	if len(hits) == 0 {
		fmt.Fprintf(w, "No match for %q.\n", question)
		fmt.Fprintln(w, "The index covers only what 'contenox index' walked (gitignored, binary and oversized files are skipped), and it is a snapshot — run 'contenox index' if the content is new.")
		return
	}
	stale := 0
	for i, hit := range hits {
		if i > 0 {
			fmt.Fprintln(w)
		}
		line := fmt.Sprintf("%s:%d-%d  %.3f", hit.Path, hit.StartLine, hit.EndLine, hit.Score)
		if hit.Stale {
			stale++
			line += "  [STALE: file changed since indexing]"
		}
		fmt.Fprintln(w, line)
		writeSnippet(w, hit.Text)
	}
	if stale > 0 {
		fmt.Fprintf(w, "\n%d of %d hit(s) are stale: the file changed after it was indexed, so the text above may not be what is on disk. Refresh with 'contenox index'.\n", stale, len(hits))
	}
}

// writeSnippet prints a chunk indented under its citation, capped in lines
// and saying how many it withheld rather than truncating silently.
func writeSnippet(w io.Writer, text string) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	shown := lines
	if len(shown) > searchSnippetLines {
		shown = shown[:searchSnippetLines]
	}
	for _, l := range shown {
		fmt.Fprintf(w, "    %s\n", l)
	}
	if len(lines) > len(shown) {
		fmt.Fprintf(w, "    … (+%d line(s) not shown; open the range above)\n", len(lines)-len(shown))
	}
}

// ─── the agent's half: workspace_search ─────────────────────────────────────

// workspaceSearchRepo carries a deferred querier: the local toolset is an
// input to enginesvc.Build, but its embedding seam is an output of it, so
// BuildEngine fills this hole once the engine exists.
type workspaceSearchRepo struct {
	taskengine.ToolsRepo
	querier *deferredQuerier
}

// newWorkspaceSearchTools builds the unbound toolset; until bindWorkspaceSearch
// runs, a call reports that retrieval isn't wired up rather than panicking.
func newWorkspaceSearchTools(workspaceID string) *workspaceSearchRepo {
	q := &deferredQuerier{}
	return &workspaceSearchRepo{ToolsRepo: searchtool.NewTools(q, workspaceID), querier: q}
}

// bindWorkspaceSearch hands the registered workspace_search toolset the index
// service once the engine's embedding seam exists. A toolset map without the
// provider is not an error: nothing is bound and the tool is simply absent.
func bindWorkspaceSearch(tools map[string]taskengine.ToolsRepo, db libdb.DBManager, engine *Engine) {
	repo, ok := tools[searchtool.ToolsProviderName].(*workspaceSearchRepo)
	if !ok || engine == nil {
		return
	}
	model := strings.TrimSpace(engine.EmbeddingModel.Name)
	if model == "" {
		// Leaving the querier unbound: the tool then tells the model retrieval
		// is unavailable instead of failing inside a provider call.
		return
	}
	repo.querier.bind(workspaceindex.New(
		runtimetypes.New(db.WithoutTransaction()),
		workspaceindex.NewLLMRepoEmbedder(engine.Models, model, engine.EmbeddingModel.Provider),
		ollamatokenizer.NewEstimateTokenizer(),
		workspaceindex.Config{EmbedModel: model, EmbedProvider: engine.EmbeddingModel.Provider},
	))
}

// deferredQuerier is a searchtool.Querier whose backing service arrives after
// construction; the mutex guards bind (engine goroutine) against concurrent
// Query calls (task-engine goroutines).
type deferredQuerier struct {
	mu  sync.RWMutex
	svc workspaceindex.Service
}

func (d *deferredQuerier) bind(svc workspaceindex.Service) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.svc = svc
}

func (d *deferredQuerier) Query(ctx context.Context, workspaceID, question string, topK int) ([]workspaceindex.Hit, error) {
	d.mu.RLock()
	svc := d.svc
	d.mu.RUnlock()
	if svc == nil {
		// ErrNoIndex: from the model's side, "nothing to search, run contenox
		// index" is exactly true, and it's rendered as a runnable instruction.
		return nil, fmt.Errorf("%w: no embedding model is configured (contenox config set default-embed-model <name>)", workspaceindex.ErrNoIndex)
	}
	return svc.Query(ctx, workspaceID, question, topK)
}
