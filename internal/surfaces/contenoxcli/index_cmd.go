// index_cmd.go holds the two operator verbs over the workspace semantic index —
// `contenox index` (build/refresh) and `contenox search` (read) — plus the
// composition-root wiring that hands the same read to the AGENT as the
// `workspace_search` tool.
//
// Self-registering (own init(), like beam_cmd.go and inbox_cmd.go), so this file
// is the whole wiring for both commands: flags, rendering, confirmation, and the
// rootCmd.AddCommand calls.
//
// The verbs are THIN, per the build-on-services rule. Every decision that costs
// anything or could be wrong — what to walk, what to chunk, what to re-embed,
// how to rank, what counts as stale — belongs to internal/services/workspaceindex.
// What lives here is what a surface owns: resolving the directory, showing the
// price before it is paid, asking, drawing progress, and rendering citations.
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

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/services/searchtool"
	"github.com/contenox/beam/internal/services/workspaceindex"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

// searchSnippetLines caps how many lines of a hit's chunk `contenox search`
// prints. A chunk is a paragraph-sized passage; the point of the terminal
// rendering is to tell you whether the CITATION is the one you want, and the
// citation is a file:line-range you can open. Six lines does that.
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

// indexDeps is everything either verb needs after the runtime is up. It exists
// so the verbs' BEHAVIOUR (plan, confirm, build, render) is testable against a
// service built over a temp SQLite file and a fake embedder, with no engine, no
// model and no network.
type indexDeps struct {
	Svc         workspaceindex.Service
	WorkspaceID string
	Model       string
	Provider    string
	// EmbedFallback reports that no embedding model was configured and the chat
	// model is standing in — resolveEmbeddingModel's documented, loud
	// degradation. The verbs say so once rather than letting the operator
	// discover it from a provider error N calls later.
	EmbedFallback bool
}

// openWorkspaceIndex is the composition step both verbs share: open the same
// SQLite everything else uses, build the engine the way chat/doctor build it,
// and compose the index service over the engine's ONE resolved model route.
//
// The engine is what supplies the embedding seam. It is not built for a chain
// here — nothing prompts — but it is the only thing that knows how to reach a
// provider, and standing up a second model manager beside it is exactly what
// enginesvc.Engine.Models exists to prevent.
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
		WorkspaceID: ResolveWorkspaceID(contenoxDir),
		Model:       model,
		Provider:    engine.EmbeddingModel.Provider,
		// The fallback is detected by asking the same question
		// resolveEmbeddingModel asked: was default-embed-model actually set?
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

// closerFunc adapts a teardown closure to io.Closer so the verbs can `defer
// closer.Close()` over a two-step teardown (engine, then database) in the order
// beam_cmd.go establishes: the engine goes down before the database it reads.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// getConfigValue reads one `contenox config` key, tolerating a missing one.
// Thin wrapper so this file states its intent at the call site instead of
// repeating clikv's signature.
func getConfigValue(ctx context.Context, store runtimetypes.Store, key string) string {
	v, _ := getConfigKV(ctx, store, key)
	return v
}

// contenoxDirForWorkspace resolves the .contenox directory that IDENTIFIES the
// workspace rooted at dir. It is ResolveContenoxDir's walk with an explicit
// starting point instead of the process's cwd, which is what lets `contenox
// index ~/src/other` and `contenox search q --dir ~/src/other` agree on which
// workspace they are talking about. --data-dir still wins, as everywhere else.
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
			// A .contenox/ without workspace.id is not a workspace (a backup, a
			// pre-init directory): keep walking up, same rule as
			// ResolveContenoxDir.
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
// directory, mirroring `beam [dir]` exactly — including its refusal, so a typo
// that names a FILE is answered the same way by both verbs.
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

// runIndexWith is the verb's whole behaviour, over an injected service: plan,
// price, ask, build, report. Separated from runIndexCmd so the flow — above all
// the "never spend without asking" part — is tested rather than asserted.
func runIndexWith(ctx context.Context, cmd *cobra.Command, deps *indexDeps, dir string, force, yes bool) error {
	out := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// Plan first, ALWAYS. It walks and chunks the tree without making a single
	// embedding call, which is the only way the number below can be honest.
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
		// Deleted files still have chunks to drop. That costs nothing, so it is
		// not a spend and nobody is asked.
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

// renderIndexPlan prints the cost BEFORE it is spent — the blueprint's
// cost-honesty rule. Files/chunks describe the whole selected tree; embed calls
// describe the WORK, and on an incremental refresh those are different numbers,
// so both are shown rather than one being passed off as the other.
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
		// Said out loud so "N files indexed" is never read as "the whole tree".
		fmt.Fprintf(w, "Skipped %s binary, %s oversized and %s generated file(s).\n",
			commafy(plan.SkippedBinary), commafy(plan.SkippedOversize), commafy(plan.SkippedGenerated))
	}
	if plan.CutOver && plan.ConfigID == "" {
		fmt.Fprintln(w, "This creates a new index generation (an index is never mutated across a model or chunking change).")
	}
}

// embedTarget names the model the calls go to. The provider is often unset (one
// backend serving everything), and "qwen2.5:7b · " with a dangling separator
// reads as a bug in the line an operator is about to approve a spend on.
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

// indexProgress draws build progress as ONE rewritten line on stderr, and only
// when stderr is a terminal.
//
// Both halves are deliberate. A line per file would put thousands of lines into
// the operator's scrollback for a command whose useful output is three lines;
// and a carriage return written into a pipe or a CI log is noise nobody can
// read, so a non-terminal gets nothing during the build and the report at the
// end — which is the whole answer anyway.
type indexProgress struct {
	w        io.Writer
	enabled  bool
	mu       sync.Mutex
	lastWide int
}

func newIndexProgress(w io.Writer) *indexProgress {
	// The check is on the real stderr rather than on w: w may be a test buffer
	// or a redirect, and neither is a terminal an operator is watching.
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

// renderProgressLine is the single status line's text. Pure, so what an
// operator sees mid-build is testable without a terminal.
func renderProgressLine(ev workspaceindex.Progress) string {
	switch ev.Phase {
	case workspaceindex.PhaseEmbedding:
		return fmt.Sprintf("embedding %s/%s files · %s/%s chunks · %s",
			commafy(ev.Files), commafy(ev.FilesTotal),
			commafy(ev.Chunks), commafy(ev.ChunksTotal),
			truncateMiddle(ev.Path, 48))
	default:
		// Planning is already on stdout as the cost line, and done is followed
		// immediately by the report: neither needs a status line of its own.
		return ""
	}
}

// truncateMiddle shortens a path from the MIDDLE, keeping the leading directory
// and the filename — the two halves that identify a file. Truncating the end
// would show a column of identical directory prefixes.
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

// confirmSpend asks before money is spent. It reads from the command's own
// stdin so a test can answer it, and it refuses to assume "yes" from a closed
// stdin: an accidental pipe must never start thousands of billed calls.
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

// commafy groups an integer with thousands separators. The counts this command
// prints are the ones an operator decides on, and 7065 reads as a different
// order of magnitude than 7,065 at a glance.
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
		// The Hit slice verbatim, so `| jq` sees the same fields the tool and
		// the service do. An empty result is `[]`, never `null`: a script that
		// iterates must not have to special-case the empty case.
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

// renderSearchHits prints one citation per hit — `path:start-end  score` — with
// an indented, capped snippet under it. Stale hits are labelled where they are
// read, not in a footnote: a hit whose file moved underneath is a lie unless it
// says so on the same line.
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

// writeSnippet prints a chunk indented under its citation, capped in lines and
// saying how many it withheld. Never silently truncate — the whole feature's
// credibility is that what it shows is what is there.
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

// workspaceSearchRepo is the toolset registered in localToolset, carrying the
// deferred querier so the composition root can bind it later.
//
// It exists because of an ORDERING fact, not a design preference: the local
// toolset is an INPUT to enginesvc.Build, while the embedding seam the index
// needs (engine.Models) is an OUTPUT of it. The tool must therefore be
// constructed before the thing it depends on exists. Rather than stand up a
// second model manager beside the engine's one — the exact thing
// enginesvc.Engine.Models was exposed to prevent — the querier is a hole that
// BuildEngine fills in the same function, four lines later.
type workspaceSearchRepo struct {
	taskengine.ToolsRepo
	querier *deferredQuerier
}

// newWorkspaceSearchTools builds the unbound toolset. Until bindWorkspaceSearch
// runs, a call reports plainly that retrieval is not wired up rather than
// panicking or, worse, answering from nothing.
func newWorkspaceSearchTools(workspaceID string) *workspaceSearchRepo {
	q := &deferredQuerier{}
	return &workspaceSearchRepo{ToolsRepo: searchtool.NewTools(q, workspaceID), querier: q}
}

// bindWorkspaceSearch hands the registered workspace_search toolset the index
// service, once the engine that supplies its embedding seam exists. A toolset
// map without the provider (a surface that did not register it) is not an
// error: nothing is bound and the tool is simply absent.
func bindWorkspaceSearch(tools map[string]taskengine.ToolsRepo, db libdb.DBManager, engine *Engine) {
	repo, ok := tools[searchtool.ToolsProviderName].(*workspaceSearchRepo)
	if !ok || engine == nil {
		return
	}
	model := strings.TrimSpace(engine.EmbeddingModel.Name)
	if model == "" {
		// No embedding model resolved at all. Leaving the querier unbound is the
		// honest outcome: the tool then tells the model retrieval is
		// unavailable, instead of failing inside a provider call every turn.
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
// construction. The mutex is not decoration: binding happens on the goroutine
// building the engine, and calls arrive on task-engine goroutines.
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
		// Reported as ErrNoIndex on purpose: from the model's side "there is
		// nothing to search here, ask the human to run contenox index" is
		// exactly true, and it is the one degradation the tool already renders
		// as a runnable instruction rather than a fault.
		return nil, fmt.Errorf("%w: no embedding model is configured (contenox config set default-embed-model <name>)", workspaceindex.ErrNoIndex)
	}
	return svc.Query(ctx, workspaceID, question, topK)
}
