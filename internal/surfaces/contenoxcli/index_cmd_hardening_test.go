package contenoxcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/services/workspaceindex"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// `contenox index` / `contenox search` — the paths index_cmd_test.go leaves open.
//
// That file pins the happy flow, the rendering and the confirmation. What is
// still unpinned is every way the flow can be WRONG about money or truth:
// --force silently not re-embedding, an edit costing the whole tree, a model
// that cannot embed being discovered on call seven thousand, a build report
// claiming it dropped nothing when it dropped plenty, and progress filling an
// operator's scrollback.
//
// Same rig (fakeEmbedder / newIndexTestDeps / newIndexTestTree / indexTestCmd),
// so these are the same production flow with different pressure on it.
// ---------------------------------------------------------------------------

// TestUnit_IndexCmd_ForceReEmbedsTheWholeTreeInPlace pins --force. The danger is
// symmetrical and both halves are silent: a --force that reuses chunks has not
// rebuilt anything (the operator ran it precisely because they believed the
// index was wrong), and one that empties the table without refilling it leaves
// the workspace worse than unindexed.
func TestUnit_IndexCmd_ForceReEmbedsTheWholeTreeInPlace(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	tree := newIndexTestTree(t)

	cmd, _, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))
	// The first build paid one extra call for the dimension probe a NEW
	// generation costs, so the tree itself is one fewer than that.
	chunks := emb.calls() - 1

	before, err := deps.Svc.Query(ctx, deps.WorkspaceID, "retry backoff", 5)
	require.NoError(t, err)
	require.NotEmpty(t, before)

	// Taken AFTER the query above, because a query embeds its question and that
	// call lands on the same counter.
	baseline := emb.calls()
	cmd2, out2, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd2, deps, tree, true, true))
	forced := emb.calls() - baseline

	// A forced rebuild extends the SAME generation, so it pays for the chunks and
	// nothing else — no second probe.
	require.Equalf(t, chunks, forced,
		"--force made %d embed call(s) where the tree is %d chunk(s): a forced rebuild must re-embed everything, exactly once", forced, chunks)
	require.NotContains(t, out2.String(), "Reusing ", "--force reported reuse; it must not reuse anything")

	// It refilled what it emptied, and the report says both numbers.
	after, err := deps.Svc.Query(ctx, deps.WorkspaceID, "retry backoff", 5)
	require.NoError(t, err)
	require.Len(t, after, len(before), "--force changed how many chunks the same query finds")
	require.Contains(t, out2.String(), "chunk(s) written")
	require.Regexpf(t, `[1-9][0-9,]* dropped`, out2.String(),
		"a forced rebuild dropped the whole generation but reported dropping none: %q", out2.String())
}

// TestUnit_IndexCmd_EditingOneFileCostsOnlyThatFile is the incremental promise
// where an operator actually feels it. A one-file edit that re-embeds the tree
// is the difference between a refresh nobody thinks about and one nobody runs.
func TestUnit_IndexCmd_EditingOneFileCostsOnlyThatFile(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	tree := newIndexTestTree(t)

	cmd, _, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))
	baseline := emb.calls()

	require.NoError(t, os.WriteFile(filepath.Join(tree, "docs", "retry.md"),
		[]byte("# Retry backoff\n\nRewritten: the ceiling is now ninety seconds.\n"), 0o644))

	cmd2, out2, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd2, deps, tree, false, true))
	spent := emb.calls() - baseline

	require.Positivef(t, spent, "the edited file was not re-embedded — a stale index that reports itself current is the worst outcome")
	require.LessOrEqualf(t, spent, 2, "editing one small file cost %d embed call(s); the incremental diff is not narrowing", spent)
	require.Contains(t, out2.String(), "Reusing ", "the plan does not say what the refresh reused")

	// REGRESSION: the incremental path dropped the edited file's old chunks and
	// reported "0 dropped", because only the --force branch recorded the count.
	// A refresh that understates what it CHANGED is the same class of dishonesty
	// as one that understates what it spent.
	require.Regexpf(t, `[1-9][0-9,]* dropped`, out2.String(),
		"the refresh replaced the edited file's chunks but reported dropping none: %q", out2.String())
	// The new content is what the index now answers with.
	hits, err := deps.Svc.Query(ctx, deps.WorkspaceID, "ceiling ninety seconds", 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	var sawNew bool
	for _, h := range hits {
		if strings.Contains(h.Text, "ninety") {
			sawNew = true
			require.Falsef(t, h.Stale, "a chunk re-embedded from the file on disk was still marked stale")
		}
	}
	require.True(t, sawNew, "the refreshed content is not in the index")
}

// TestUnit_IndexCmd_DeletedFileDropsItsChunksWithoutSpending covers the cleanup
// half: removing a file must drop its chunks, must cost NOTHING (there is
// nothing to embed), and must say so instead of printing a build report that
// implies work happened.
func TestUnit_IndexCmd_DeletedFileDropsItsChunksWithoutSpending(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	tree := newIndexTestTree(t)

	cmd, _, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))
	baseline := emb.calls()

	require.NoError(t, os.Remove(filepath.Join(tree, "docs", "retry.md")))

	cmd2, out2, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd2, deps, tree, false, true))

	require.Equalf(t, baseline, emb.calls(), "dropping a deleted file's chunks made %d embed call(s)", emb.calls()-baseline)
	require.Contains(t, out2.String(), "Dropping the chunks of 1 file(s)")
	// The plan promised a drop; the REPORT must account for it. (This path
	// returns before embedding anything, so the report is the only place the
	// number can appear at all.)
	require.Regexpf(t, `[1-9][0-9,]* dropped|Nothing to embed; dropping the chunks of 1 removed file`, out2.String(),
		"a deletion-only refresh did not account for what it dropped: %q", out2.String())

	// The deleted file is gone from the index — not returned as a stale hit
	// forever, which would be a citation to a file that does not exist.
	hits, err := deps.Svc.Query(ctx, deps.WorkspaceID, "retry backoff doubles", 10)
	require.NoError(t, err)
	for _, h := range hits {
		require.NotEqual(t, "docs/retry.md", h.Path, "a deleted file still has chunks in the index")
	}
}

// TestUnit_IndexCmd_EmptyWorkspaceSpendsNothingAndCreatesNothing covers a
// workspace with nothing indexable in it. The rule that matters is the second
// one: nothing is CREATED, so a later search still degrades to the runnable
// instruction rather than answering "no matches" from an index that does not
// exist — two very different things for the operator reading the output.
func TestUnit_IndexCmd_EmptyWorkspaceSpendsNothingAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)

	cmd, out, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, t.TempDir(), false, true))

	require.Contains(t, out.String(), "0 files → 0 chunks → 0 embed calls")
	require.Contains(t, out.String(), "Already current")
	require.Zerof(t, emb.calls(), "an empty workspace made %d embedding call(s)", emb.calls())

	err := runSearchWith(ctx, cmd, deps, "anything", 0, false)
	require.ErrorContains(t, err, "no index for this workspace",
		"an empty build created an index generation; a workspace with nothing in it has no index")
}

// TestUnit_IndexCmd_UnusableEmbeddingModelIsCaughtByTheProbe is the dimension
// probe's actual justification. A chat model asked to embed — the exact failure
// the default-embed-model keys exist to prevent — must cost ONE call, not one
// per chunk, and the refusal must name the model and the provider so the
// operator knows which of the two settings is wrong.
func TestUnit_IndexCmd_UnusableEmbeddingModelIsCaughtByTheProbe(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)
	emb.fail = errors.New("this model does not support embeddings")
	tree := newIndexTestTree(t)

	cmd, _, _ := indexTestCmd(t, "")
	err := runIndexWith(ctx, cmd, deps, tree, false, true)
	require.Error(t, err, "a model that cannot embed produced a successful index")

	msg := err.Error()
	require.Contains(t, msg, "unusable", "the refusal does not say the model is unusable: %q", msg)
	require.Contains(t, msg, "fake-embed", "the refusal does not name the model: %q", msg)
	require.Contains(t, msg, "testprovider", "the refusal does not name the provider: %q", msg)

	require.Equalf(t, 1, emb.calls(),
		"an unusable model was called %d times — the probe exists so the whole tree is never spent against it", emb.calls())

	// Nothing was half-created: the failed build left no index generation behind
	// for a later search to find and report as empty.
	require.ErrorContains(t, runSearchWith(ctx, cmd, deps, "anything", 0, false), "no index for this workspace")
}

// TestUnit_IndexCmd_MidBuildEmbedFailureLeavesNoHalfIndexedFile is the
// resumability claim, which is the whole reason flush() is called only on a file
// boundary. A build that dies part-way must leave every file either wholly
// indexed or wholly absent — never partially indexed under a sha that MATCHES
// disk, because the next incremental build would read that as "already done" and
// skip the rest of the file forever.
func TestUnit_IndexCmd_MidBuildEmbedFailureLeavesNoHalfIndexedFile(t *testing.T) {
	ctx := context.Background()
	deps, emb := newIndexTestDeps(t)

	// A tree big enough that the failure lands mid-build rather than on file one.
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("doc%02d.md", i)] = strings.Repeat(
			fmt.Sprintf("# Document %d\n\nThis paragraph is about subject %d and retry backoff.\n", i, i), 12)
	}
	tree := writeTree(t, files)

	cmd, _, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd, deps, tree, false, true))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "doc00.md"), []byte("# changed\n\nnew body about retry backoff.\n"), 0o644))

	// Fail every embed from here on: the refresh cannot complete.
	emb.fail = errors.New("provider went away mid-build")
	cmd2, _, _ := indexTestCmd(t, "")
	require.Error(t, runIndexWith(ctx, cmd2, deps, tree, false, true), "a dead provider produced a successful refresh")

	// Recover, and the refresh must still see doc00.md as work to do. If the
	// failed run had written a partial doc00.md under the NEW sha, this second
	// run would report "already current" and the file would stay half-indexed.
	emb.fail = nil
	cmd3, out3, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd3, deps, tree, false, true))
	require.NotContainsf(t, out3.String(), "Already current",
		"the interrupted build left doc00.md recorded as done; a resumed build skipped it: %q", out3.String())

	// "changed" appears in no other document, so the lexical prefilter narrows to
	// doc00.md alone and the hit is unambiguous.
	hits, err := deps.Svc.Query(ctx, deps.WorkspaceID, "changed", 10)
	require.NoError(t, err)
	var recovered bool
	for _, h := range hits {
		if h.Path == "doc00.md" {
			recovered = true
			require.Falsef(t, h.Stale, "the recovered chunk does not match the file on disk")
		}
	}
	require.Truef(t, recovered, "the file whose embedding failed was never recovered by a later build (found %d hit(s))", len(hits))

	// And the index converges: a third run has nothing left to do.
	cmd4, out4, _ := indexTestCmd(t, "")
	require.NoError(t, runIndexWith(ctx, cmd4, deps, tree, false, true))
	require.Contains(t, out4.String(), "Already current", "the index never converged after the failure")
}

// TestUnit_IndexProgress_NonTerminalGetsNoScrollbackAtAll is the anti-spam rule.
// A line per file would put thousands of lines into the scrollback of a command
// whose useful output is three lines, and a carriage return written into a pipe
// or a CI log is noise nobody can read — so a non-terminal (which is what `go
// test` gives us) must get nothing during the build.
func TestUnit_IndexProgress_NonTerminalGetsNoScrollbackAtAll(t *testing.T) {
	var buf bytes.Buffer
	p := newIndexProgress(&buf)
	for i := 1; i <= 5000; i++ {
		p.emit(workspaceindex.Progress{
			Phase: workspaceindex.PhaseEmbedding,
			Path:  fmt.Sprintf("internal/services/pkg%d/file%d.go", i, i),
			Files: i, FilesTotal: 5000, Chunks: i * 3, ChunksTotal: 15000,
		})
	}
	p.done()
	require.Emptyf(t, buf.String(),
		"progress wrote %d byte(s) into a non-terminal; a redirect or CI log gets the report only", buf.Len())
}

// TestUnit_IndexProgress_TerminalRewritesOneLineAndClearsIt exercises the
// enabled renderer directly, since the terminal check is on the real stderr and
// a test never has one. What is pinned is the discipline: carriage returns, no
// newlines, and a shorter line fully erasing the longer one it replaced (or the
// tail of the previous path is left dangling on screen).
func TestUnit_IndexProgress_TerminalRewritesOneLineAndClearsIt(t *testing.T) {
	var buf bytes.Buffer
	p := &indexProgress{w: &buf, enabled: true}

	p.emit(workspaceindex.Progress{Phase: workspaceindex.PhaseEmbedding, Path: "a/very/long/path/to/some/file.go", Files: 1, FilesTotal: 9, Chunks: 1, ChunksTotal: 9})
	long := buf.Len()
	p.emit(workspaceindex.Progress{Phase: workspaceindex.PhaseEmbedding, Path: "b.go", Files: 2, FilesTotal: 9, Chunks: 2, ChunksTotal: 9})
	p.done()

	s := buf.String()
	require.NotContains(t, s, "\n", "the status line was appended instead of rewritten")
	// Two updates (one \r each) plus done()'s erase-and-return (two).
	require.Equal(t, 4, strings.Count(s, "\r"), "the line is not being rewritten in place")
	require.Greater(t, buf.Len(), long)
	require.Contains(t, s, "b.go")
	// The shorter second line is padded to cover the first.
	require.Regexpf(t, `b\.go\s{2,}`, s, "a shorter line did not erase the longer one it replaced: %q", s)
}

// TestUnit_DeferredQuerier_UnboundIsAnInstructionNotAPanic covers the ordering
// hole bindWorkspaceSearch fills: the toolset is registered BEFORE the engine
// that supplies its embedding seam exists. Until it is bound — or when no
// embedding model resolved at all — a call must degrade to the one thing the
// tool already renders as a runnable instruction.
func TestUnit_DeferredQuerier_UnboundIsAnInstructionNotAPanic(t *testing.T) {
	var d deferredQuerier

	hits, err := d.Query(context.Background(), "ws", "anything", 5)
	require.Nil(t, hits)
	require.ErrorIsf(t, err, workspaceindex.ErrNoIndex,
		"an unbound querier must report ErrNoIndex, which the tool renders as `run contenox index`; got %v", err)
	require.Contains(t, err.Error(), "default-embed-model", "the cause does not name the setting that fixes it")

	// Binding happens on the goroutine building the engine while calls arrive on
	// task-engine goroutines; under -race this is where an unguarded field shows.
	deps, _ := newIndexTestDeps(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = d.Query(context.Background(), "ws", "anything", 5) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.bind(deps.Svc) }()
	wg.Wait()

	_, err = d.Query(context.Background(), deps.WorkspaceID, "anything", 5)
	require.ErrorIs(t, err, workspaceindex.ErrNoIndex, "a bound querier over an empty store still reports no index")
}

// writeTree materialises a workspace from a path→body map.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return root
}
