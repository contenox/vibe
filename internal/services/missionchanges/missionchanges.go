// Package missionchanges is the attention layer's first two stages made
// concrete: it reads a mission's already-journaled work and answers two
// questions an operator's oversight cockpit asks — "what did this unit change?"
// and "where did its attention go, and did it wander?" It is a pure CONSUMER of
// recordings the runtime already keeps (the kernel's per-session replay
// journal), never a new recording duty; that is the layer's blind-spot
// doctrine (see
// docs/development/blueprints/beam/attention-layer.md): every artifact of
// orientation must be an automatic by-product of work, computed, never asked
// for.
//
// # What it computes
//
// Given a mission id, the service resolves the mission's one session/instance
// (missionservice binds exactly one of each), folds that session's kernel
// journal, and returns:
//
//   - Changes: the changed-files list — per path, the FIRST OldText and LAST
//     NewText seen across the mission's diff events, with a git-shaped status
//     derived from them. The list and the per-path diff form a deliberate
//     two-endpoint contract
//     (ide-workflows.md Arc 1); the raw material is acpsvc's
//     diffContentFromResult, which already turns every file-write tool result
//     into a libacp.ToolCallContent{Type: Diff, Path, OldText, NewText} flowing
//     through the session stream.
//
//   - Attention (Stage 1 — Degree-of-Interest): a per-path interest score from
//     ALL of the unit's tool interactions touching that path — edits weighted
//     above reads weighted above the rest — so the changed-files list is ordered
//     by where the unit's attention CONCENTRATED, not alphabetically. Review
//     starts at the hot spot.
//
//   - Scope (Stage 2 — scope-anomaly detection): the touched-path set summarized
//     as distinct files and distinct top-level directories, plus a scopeAnomaly
//     flag raised when any touched path falls OUTSIDE the mission's workspace
//     root (its cwd). This converts a core design premise — that SCOPE,
//     not step count, is the real efficiency signal, and that derailment is a
//     scope anomaly before it is anything else (the $HOME-wanderer detectable
//     from its first two tool calls) — into the fleet's cheapest alarm: set
//     arithmetic over the same fold.
//
// # The advice-not-gate law (binding)
//
// Nothing here gates anything. The scores RANK and the anomaly FLAGS; envelopes
// gate. Rank is advice; the operator's eyes stay the judge — the attention layer
// is an exoskeleton, not an autopilot. A high score never blocks a file, a
// scopeAnomaly never stops a unit, and both are computed after the fact from a
// record that was written whether or not anyone was watching. This law is why
// the service has no verb that mutates a mission and no dependency that could.
package missionchanges

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/libacp"
)

// The DOI weight table (Stage 1). These weights
// are TUNABLE HYPOTHESES, not constants of nature.
// The ordering edit > read > other is the load-bearing claim ("a mutation is a
// stronger attention signal than an inspection"); the exact integers are a first
// guess meant to be measured against rank-vs-flat review, never trusted as
// truth.
//
// The wire granularity is coarser than the ideal (edit > read > stat/list):
// acpsvc's toolKindFor folds read_file, stat_file, list_dir and grep all into
// libacp.ToolKindRead before the event is journaled, so this layer cannot
// separate a read from a stat at the point it consumes them. That collapse is a
// known limit of the current recording, documented here rather than papered
// over; if it ever matters, the finer signal is added upstream at the recording,
// not guessed at here.
//
// Two further mechanics — DECAY per round (recent attention outweighs
// stale) and MASKING (one anchor per neighborhood) — are deliberately NOT yet
// applied: additive accumulation is the honest Stage-1 stub the blueprint calls
// for ("order-by-interest is one fold away from order-by-path"), and layering
// decay/masking on top is a later, separately-measurable refinement. Naming them
// here is the register the blueprint requires: they are known, deferred, and
// tunable.
const (
	weightEdit    = 4 // a write/sed mutation — the strongest attention signal
	weightDelete  = 4 // a deletion is a mutation too
	weightMove    = 3
	weightRead    = 2 // read/stat/list/grep, folded to one kind at the wire
	weightFetch   = 2
	weightExecute = 2
	weightOther   = 1
)

// maxChangedFiles caps the changed-files list the way review UIs cap a diff view: a
// review surface must stay legible, and a mission that touched thousands of
// files is exactly when the cap matters most. The cap is applied AFTER scoring
// and sorting, so the files that survive it are the highest-attention ones — the
// cap never hides the hot spot, only the long cold tail. When it bites, Changes
// sets Incomplete so the frontend can say "showing the top N of more".
const maxChangedFiles = 100

// diffDisplayCap bounds the bytes of original/modified text the diff endpoint
// returns for one file. A single pathological
// generated file must not turn a diff fetch into a multi-megabyte response; past
// this size the text is truncated and Diff.Truncated is set so the frontend
// renders a "diff too large" affordance instead of choking Monaco. The kernel
// journal already caps each diff field upstream (taskengine's journalTextFieldCap
// is 16KiB), so this is a second, generous backstop, not the primary bound.
const diffDisplayCap = 128 * 1024

// maxOutsidePaths bounds the sample of out-of-root paths reported on a scope
// anomaly. The anomaly is a boolean alarm; the sample is a courtesy for the
// operator ("it wandered HERE"), not an exhaustive audit, so a handful is enough
// and an unbounded list would just be noise from a badly-derailed unit.
const maxOutsidePaths = 20

// ChangedFile is one file the mission's unit wrote, as it appears in the ordered
// changed-files list. Score is its Stage-1 Degree-of-Interest (advice for review
// order, never a gate); Status is the git-shaped verdict derived from the first
// and last diff seen for the path.
type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "added" | "modified" | "deleted"
	Score  int    `json:"score"`
}

// The three status strings ChangedFile.Status draws from. Contracted values —
// the Beam diff viewer keys its badges on them.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
)

// ScopeStats is the Stage-2 scope summary: how broadly the unit ranged and
// whether it left its lane. Files and Dirs are the breadth signal — the
// REAL efficiency metric (a landed unit works in few files);
// Anomaly plus OutsidePaths are the derailment early-warning. All advisory.
type ScopeStats struct {
	Files        int      `json:"files"`
	Dirs         int      `json:"dirs"`
	Anomaly      bool     `json:"anomaly"`
	OutsidePaths []string `json:"outsidePaths,omitempty"`
}

// Changes is the GET /missions/{id}/changes response: the ordered changed-files
// list, the cap flag, and the scope summary. Files is always non-nil so an
// empty result renders as [] rather than null. Incomplete is true when the
// changed-files list was capped (see maxChangedFiles) — the frontend must treat
// a true Incomplete as "there are more changed files than shown".
type Changes struct {
	Files      []ChangedFile `json:"files"`
	Incomplete bool          `json:"incomplete"`
	Scope      ScopeStats    `json:"scope"`
}

// Diff is the GET /missions/{id}/changes/diff?path= response: an {original,
// modified} pair fed straight to Monaco's
// DiffEditor. Truncated is set when either side was clipped to diffDisplayCap —
// the frontend then shows a "diff too large" state instead of a partial diff
// pretending to be whole.
type Diff struct {
	Original  string `json:"original"`
	Modified  string `json:"modified"`
	Truncated bool   `json:"truncated,omitempty"`
}

// missionGetter is the NARROW slice of missionservice this package needs: resolve
// a mission id to its record (for the bound session/instance). Declared here,
// not imported wholesale, so the dependency is one verb and a unit test can
// satisfy it with a stub. missionservice.Service satisfies it.
type missionGetter interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
}

// SessionJournalReader is the OPTIONAL kernel capability this layer reads through
// — the exact register of fleetservice's sessionTextReader / the kernel's
// SessionAgentText precedent. The concrete agentinstance.Manager provides
// SessionJournal (the raw per-session replay journal plus the session's cwd,
// recoverable WITHOUT attaching a viewer); it is reached by type assertion and
// is deliberately NOT on the Manager lifecycle interface, so a Manager double
// need not grow a method and sibling mocks stay untouched. The kernel returns
// the journal UNINTERPRETED — all of the attention/scope judgement lives here,
// keeping the kernel policy-free.
//
// ok is false for an unknown instance or a session the instance does not own (a
// unit that already stopped, or a mission never dispatched); the service then
// returns an EMPTY Changes, not an error — "nothing recorded" is an honest,
// non-exceptional answer for a mission whose unit is gone.
type SessionJournalReader interface {
	SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool)
}

// Service answers the two attention-layer endpoints. It is read-only by
// construction (the advice-not-gate law): no method here mutates a mission.
type Service interface {
	// Changes folds mission id's session journal into the ordered changed-files
	// list plus the scope summary. An unknown mission id surfaces as the
	// resolver's not-found error; a known mission whose unit left no recoverable
	// journal yields an empty, non-error Changes.
	Changes(ctx context.Context, missionID string) (*Changes, error)

	// Diff returns the {original, modified} pair for one changed path in mission
	// id — first OldText, last NewText across the mission's diff events for that
	// path — truncating to diffDisplayCap. A path that the mission never wrote
	// yields a not-found error so the frontend can tell "no such changed file"
	// from "an empty diff".
	Diff(ctx context.Context, missionID, filePath string) (*Diff, error)
}

type service struct {
	missions missionGetter
	journal  SessionJournalReader
}

// New builds the service over a mission resolver and the kernel journal reader.
// journal may be nil (a deployment without a live kernel), in which case every
// mission reads as having no recorded work — the endpoints still answer, with
// empty changes, rather than failing.
func New(missions missionGetter, journal SessionJournalReader) Service {
	return &service{missions: missions, journal: journal}
}

func (s *service) Changes(ctx context.Context, missionID string) (*Changes, error) {
	updates, cwd, err := s.load(ctx, missionID)
	if err != nil {
		return nil, err
	}
	folded := fold(updates)
	return folded.changes(cwd), nil
}

func (s *service) Diff(ctx context.Context, missionID, filePath string) (*Diff, error) {
	updates, _, err := s.load(ctx, missionID)
	if err != nil {
		return nil, err
	}
	folded := fold(updates)
	fileDiff, ok := folded.files[filePath]
	if !ok {
		return nil, errdefs.NotFound("no changed file at that path in this mission")
	}
	original, modified, truncated := capDiff(fileDiff.firstOld, fileDiff.lastNew)
	return &Diff{Original: original, Modified: modified, Truncated: truncated}, nil
}

// load resolves the mission and returns its session journal plus workspace cwd.
// A mission with no bound session/instance, or one whose unit is no longer live
// in the kernel, returns (nil, "", nil): an empty-but-valid input the folds
// render as empty results.
func (s *service) load(ctx context.Context, missionID string) ([]libacp.SessionNotification, string, error) {
	m, err := s.missions.Get(ctx, missionID)
	if err != nil {
		return nil, "", err
	}
	if s.journal == nil || m.SessionID == "" || m.InstanceID == "" {
		return nil, "", nil
	}
	updates, cwd, ok := s.journal.SessionJournal(m.InstanceID, libacp.SessionID(m.SessionID))
	if !ok {
		return nil, "", nil
	}
	return updates, cwd, nil
}

// fileFold is the accumulated diff state for one written path: the FIRST OldText
// (the file's content as the mission first found it) and the LAST NewText (its
// content as the mission left it). first-old/last-new is the whole aggregation
// the diff-review arc names — it collapses an arbitrary sequence of edits to one
// before/after pair, which is exactly the {original, modified} Monaco renders.
type fileFold struct {
	firstOld    string
	lastNew     string
	haveFirst   bool
	firstSeenAt int // journal index of the first diff — a stable secondary sort key
}

// folded is the whole-journal fold: per-path diff state, per-path attention
// score, and the full touched-path set (edits AND reads AND every other located
// tool touch) for the scope summary.
type folded struct {
	files      map[string]*fileFold
	scores     map[string]int
	touched    map[string]struct{}
	touchOrder []string // first-touch order, for deterministic output on score ties
}

// fold walks the session journal ONCE and accumulates everything the two
// endpoints need. It reads every tool-call notification and interprets each the
// way acpsvc emitted it: a diff content is an edit of its path (with old/new
// text), a location is a touch of its path weighted by the tool's kind.
//
// # Why scoring is deduped by tool-call id
//
// One tool INVOCATION can be journaled as more than one notification, and acpsvc
// spreads its evidence across them. The interactive approval flow emits a
// create/pending notification (SessionUpdateToolCall — a location, no diff yet)
// and then a terminal update (SessionUpdateToolCallUpdate — the diff); the
// unattended deterministic flow emits a single notification that acpsvc's
// normalizeToolCallNotification PROMOTES to a create (SessionUpdateToolCall)
// because it is the first for its id — carrying the diff on that create. So
// neither "score only updates" nor "score every notification" is right: the
// first would drop every deterministic write (its diff rides a create), the
// second would double-count every interactive one (create location + update
// diff).
//
// The correct unit is the INVOCATION, keyed by ToolCallID: each invocation
// contributes each path it touched ONCE, at that path's strongest weight across
// all of the invocation's notifications (a diff makes the path an edit, which
// dominates any read/stat location for the same path). Two separate writes of one
// file are two invocations (distinct ids — acpsvc's toolCallWireID gives repeated
// runs distinct ids) and so score twice: genuinely repeated attention. An id-less
// notification (should not occur for a real tool call) is treated as its own
// invocation so it still scores once.
func fold(updates []libacp.SessionNotification) *folded {
	f := &folded{
		files:   make(map[string]*fileFold),
		scores:  make(map[string]int),
		touched: make(map[string]struct{}),
	}
	// callWeights[toolCallID][path] is the weight this invocation adds for the
	// path — deduped across the invocation's create + update notifications.
	callWeights := make(map[string]map[string]int)
	for i, n := range updates {
		u := n.Update
		if u.SessionUpdate != libacp.SessionUpdateToolCall && u.SessionUpdate != libacp.SessionUpdateToolCallUpdate {
			continue
		}
		callID := u.ToolCallID
		if callID == "" {
			callID = fmt.Sprintf("\x00idx%d", i) // id-less: its own invocation
		}
		cw := callWeights[callID]
		if cw == nil {
			cw = make(map[string]int)
			callWeights[callID] = cw
		}

		editPaths := make(map[string]struct{})
		for _, c := range u.ToolContent {
			if c.Type != libacp.ToolCallContentDiff || c.Path == "" {
				continue
			}
			editPaths[c.Path] = struct{}{}
			f.markTouched(c.Path)
			ff := f.files[c.Path]
			if ff == nil {
				ff = &fileFold{firstSeenAt: i}
				f.files[c.Path] = ff
			}
			if !ff.haveFirst {
				ff.firstOld = c.OldText
				ff.haveFirst = true
			}
			ff.lastNew = c.NewText
			cw[c.Path] = weightEdit // an edit dominates any other touch this invocation
		}
		for _, loc := range u.Locations {
			if loc.Path == "" {
				continue
			}
			f.markTouched(loc.Path)
			if _, isEdit := editPaths[loc.Path]; isEdit {
				continue // already the strongest weight for this invocation
			}
			if w := weightForKind(u.Kind); w > cw[loc.Path] {
				cw[loc.Path] = w
			}
		}
	}
	// Flush each invocation's per-path weight into the additive DOI score, one
	// invocation at a time.
	for _, cw := range callWeights {
		for p, w := range cw {
			f.scores[p] += w
		}
	}
	return f
}

func (f *folded) markTouched(p string) {
	if _, seen := f.touched[p]; seen {
		return
	}
	f.touched[p] = struct{}{}
	f.touchOrder = append(f.touchOrder, p)
}

// weightForKind maps a journaled libacp.ToolKind to its DOI weight. The kinds
// that never carry a path in practice (think/switch_mode) fall through to
// weightOther harmlessly, since they only reach here with a non-empty location.
func weightForKind(k libacp.ToolKind) int {
	switch k {
	case libacp.ToolKindEdit:
		return weightEdit
	case libacp.ToolKindDelete:
		return weightDelete
	case libacp.ToolKindMove:
		return weightMove
	case libacp.ToolKindRead, libacp.ToolKindSearch:
		return weightRead
	case libacp.ToolKindFetch:
		return weightFetch
	case libacp.ToolKindExecute:
		return weightExecute
	default:
		return weightOther
	}
}

// changes renders the fold into the endpoint response, ordering the changed
// files by DOI (Stage 1) and summarizing the touched set (Stage 2) against the
// workspace root.
func (f *folded) changes(cwd string) *Changes {
	files := make([]ChangedFile, 0, len(f.files))
	for p, ff := range f.files {
		files = append(files, ChangedFile{
			Path:   p,
			Status: statusFor(ff),
			Score:  f.scores[p],
		})
	}
	// Order-by-interest, not order-by-path: highest DOI first so review starts
	// where the unit's attention concentrated. Ties break by earliest first-diff
	// then by path, purely for a deterministic, stable render.
	sort.Slice(files, func(a, b int) bool {
		if files[a].Score != files[b].Score {
			return files[a].Score > files[b].Score
		}
		fa, fb := f.files[files[a].Path], f.files[files[b].Path]
		if fa.firstSeenAt != fb.firstSeenAt {
			return fa.firstSeenAt < fb.firstSeenAt
		}
		return files[a].Path < files[b].Path
	})

	incomplete := false
	if len(files) > maxChangedFiles {
		files = files[:maxChangedFiles]
		incomplete = true
	}

	return &Changes{
		Files:      files,
		Incomplete: incomplete,
		Scope:      f.scope(cwd),
	}
}

// statusFor derives the git-shaped status from a path's first/last diff, in the
// precedence the contract fixes: added when the first OldText was empty (the
// file did not exist when the mission first touched it), else deleted when the
// last NewText is empty (it was emptied/removed), else modified. Checking added
// FIRST means a file created and later emptied still reads as "added" — it is
// new to the mission regardless of where it ended up.
func statusFor(ff *fileFold) string {
	switch {
	case ff.firstOld == "":
		return StatusAdded
	case ff.lastNew == "":
		return StatusDeleted
	default:
		return StatusModified
	}
}

// scope summarizes the touched-path set against the workspace root: distinct
// files, distinct top-level directories, and the anomaly flag. An empty root
// (the kernel did not record a cwd for the session) disables the anomaly check —
// there is no lane to have left — rather than guessing one, since a false
// derailment alarm is worse than a missing one for an advisory signal.
func (f *folded) scope(root string) ScopeStats {
	root = filepath.Clean(root)
	dirs := make(map[string]struct{})
	var outside []string
	for _, p := range f.touchOrder {
		if root != "" && root != "." && isOutside(root, p) {
			if len(outside) < maxOutsidePaths {
				outside = append(outside, p)
			}
		}
		dirs[topLevelDir(root, p)] = struct{}{}
	}
	sort.Strings(outside)
	return ScopeStats{
		Files:        len(f.touched),
		Dirs:         len(dirs),
		Anomaly:      len(outside) > 0,
		OutsidePaths: outside,
	}
}

// isOutside reports whether p falls outside the workspace root. A RELATIVE p is
// always inside by construction (it is named relative to the cwd the tool ran
// in, so it cannot escape it without a ".." the tool layer already refuses);
// only an ABSOLUTE path that does not sit under root trips the alarm — which is
// exactly the shape of the derailment this detects (the $HOME-wanderer naming an
// absolute path in another tree). This is set arithmetic, and it is ADVICE: it
// raises an eyebrow, it never stops the unit.
func isOutside(root, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// topLevelDir names the "top-level directory" a path counts toward for the
// breadth signal. Under the root, it is the FIRST path segment of the
// root-relative path — "." for a file directly in the root, "src" for
// src/foo/bar.go — so breadth measures how many top-level areas of the workspace
// the unit ranged across, not how deep it dug. Outside the root (or with no
// root), it falls back to the path's own leading segment, so a wanderer's
// out-of-tree touches still register as distinct breadth rather than collapsing
// into one bucket.
func topLevelDir(root, p string) string {
	cp := filepath.Clean(p)
	if root != "" && root != "." && filepath.IsAbs(cp) {
		if rel, err := filepath.Rel(root, cp); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return firstSegment(filepath.ToSlash(rel))
		}
	}
	if filepath.IsAbs(cp) {
		// e.g. /etc/hostname -> "/etc"; /home/x/y -> "/home"
		trimmed := strings.TrimPrefix(filepath.ToSlash(cp), "/")
		return "/" + firstSegment(trimmed)
	}
	return firstSegment(filepath.ToSlash(cp))
}

// firstSegment returns the first slash-separated element of a relative path, or
// "." when the path is a bare filename (no directory part) — the root-itself
// bucket. A bare filename lives directly in its base directory, so it counts
// toward "." rather than toward a directory named after the file.
func firstSegment(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return "."
}

// capDiff clips original/modified to diffDisplayCap, reporting whether either
// side was clipped. Clipping is on bytes, not runes, and may split a multi-byte
// rune at the boundary; that is acceptable for a "diff too large" fallback the
// frontend renders as a warning rather than trusting as exact.
func capDiff(original, modified string) (string, string, bool) {
	truncated := false
	if len(original) > diffDisplayCap {
		original = original[:diffDisplayCap]
		truncated = true
	}
	if len(modified) > diffDisplayCap {
		modified = modified[:diffDisplayCap]
		truncated = true
	}
	return original, modified, truncated
}
