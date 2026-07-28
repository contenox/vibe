// Package missionchanges answers two oversight questions from a mission's
// already-journaled work: what did the unit change, and did its attention
// wander outside its workspace. It is a pure consumer of the kernel's replay
// journal — it never records anything and no method mutates a mission
// (advice-not-gate: scores rank and anomalies flag, they never gate).
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

// DOI (Degree-of-Interest) weights for Stage 1 scoring. These are tunable
// hypotheses, not fixed constants; only the ordering edit > read > other is
// load-bearing. The wire granularity is coarser than ideal: acpsvc folds
// read_file/stat_file/list_dir/grep into one ToolKindRead before journaling,
// so this layer cannot separate them. Decay and masking are deliberately not
// applied yet — this is the additive Stage-1 stub.
const (
	weightEdit    = 4 // a write/sed mutation — the strongest attention signal
	weightDelete  = 4 // a deletion is a mutation too
	weightMove    = 3
	weightRead    = 2 // read/stat/list/grep, folded to one kind at the wire
	weightFetch   = 2
	weightExecute = 2
	weightOther   = 1
)

// maxChangedFiles caps the changed-files list, applied after scoring and
// sorting so the surviving entries are the highest-attention ones. When it
// bites, Changes.Incomplete is set.
const maxChangedFiles = 100

// diffDisplayCap bounds the bytes of original/modified text one Diff
// response returns; past this, Diff.Truncated is set. The kernel journal
// already caps each diff field upstream (16KiB); this is a generous backstop.
const diffDisplayCap = 128 * 1024

// maxOutsidePaths bounds the sample of out-of-root paths reported on a scope
// anomaly — a courtesy sample, not an exhaustive audit.
const maxOutsidePaths = 20

// ChangedFile is one file the mission's unit wrote. Score is its Stage-1
// DOI (advice for review order, never a gate); Status is derived from the
// first and last diff seen for the path.
type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "added" | "modified" | "deleted"
	Score  int    `json:"score"`
}

// The three status strings ChangedFile.Status draws from. Contracted values
// — the Beam diff viewer keys its badges on them.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
)

// ScopeStats is the Stage-2 scope summary: how broadly the unit ranged and
// whether it left its lane. All fields are advisory.
type ScopeStats struct {
	Files        int      `json:"files"`
	Dirs         int      `json:"dirs"`
	Anomaly      bool     `json:"anomaly"`
	OutsidePaths []string `json:"outsidePaths,omitempty"`
}

// Changes is the GET /missions/{id}/changes response. Files is always
// non-nil. Incomplete is true when the list was capped (maxChangedFiles).
type Changes struct {
	Files      []ChangedFile `json:"files"`
	Incomplete bool          `json:"incomplete"`
	Scope      ScopeStats    `json:"scope"`
}

// Diff is the GET /missions/{id}/changes/diff?path= response, fed straight
// to Monaco's DiffEditor. Truncated is set when either side was clipped to
// diffDisplayCap.
type Diff struct {
	Original  string `json:"original"`
	Modified  string `json:"modified"`
	Truncated bool   `json:"truncated,omitempty"`
}

// missionGetter is the narrow slice of missionservice this package needs, so
// a unit test can satisfy it with a stub. missionservice.Service implements it.
type missionGetter interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
}

// SessionJournalReader is the optional kernel capability this layer reads
// through (reached by type assertion, since it's not on the Manager
// lifecycle interface). The kernel returns the journal uninterpreted; all
// attention/scope judgement lives here.
//
// ok is false for an unknown instance or a session the instance does not
// own; the service then returns an empty Changes, not an error.
type SessionJournalReader interface {
	SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool)
}

// Service answers the two attention-layer endpoints. Read-only by
// construction: no method here mutates a mission.
type Service interface {
	// Changes folds mission id's session journal into the ordered
	// changed-files list plus the scope summary. A known mission whose unit
	// left no recoverable journal yields an empty, non-error Changes.
	Changes(ctx context.Context, missionID string) (*Changes, error)

	// Diff returns the {original, modified} pair for one changed path in
	// mission id, truncated to diffDisplayCap. A path the mission never
	// wrote yields a not-found error.
	Diff(ctx context.Context, missionID, filePath string) (*Diff, error)
}

type service struct {
	missions missionGetter
	journal  SessionJournalReader
}

// New builds the service over a mission resolver and the kernel journal
// reader. journal may be nil, in which case every mission reads as having no
// recorded work rather than failing.
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

// load resolves the mission and returns its session journal plus workspace
// cwd. A mission with no bound session/instance, or a unit no longer live in
// the kernel, returns (nil, "", nil) — an empty-but-valid input.
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

// fileFold is the accumulated diff state for one written path: the first
// OldText and the last NewText, collapsing an arbitrary edit sequence to one
// before/after pair.
type fileFold struct {
	firstOld    string
	lastNew     string
	haveFirst   bool
	firstSeenAt int // journal index of the first diff — a stable secondary sort key
}

// folded is the whole-journal fold: per-path diff state, per-path attention
// score, and the full touched-path set for the scope summary.
type folded struct {
	files      map[string]*fileFold
	scores     map[string]int
	touched    map[string]struct{}
	touchOrder []string // first-touch order, for deterministic output on score ties
}

// fold walks the session journal once. Scoring is deduped by tool-call id:
// one invocation can be journaled as more than one notification (an
// interactive approval flow emits a create-location then an update-diff; a
// deterministic flow emits a single notification promoted to a create
// carrying the diff), so each invocation contributes each path it touched
// once, at that path's strongest weight across its own notifications. Two
// separate writes of one file are two invocations (distinct ToolCallIDs) and
// score twice.
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

// weightForKind maps a journaled libacp.ToolKind to its DOI weight. Kinds
// that never carry a path in practice fall through to weightOther harmlessly.
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

// changes renders the fold into the endpoint response: files ordered by DOI
// (Stage 1), ties broken by earliest first-diff then path for a stable
// render; touched set summarized against the workspace root (Stage 2).
func (f *folded) changes(cwd string) *Changes {
	files := make([]ChangedFile, 0, len(f.files))
	for p, ff := range f.files {
		files = append(files, ChangedFile{
			Path:   p,
			Status: statusFor(ff),
			Score:  f.scores[p],
		})
	}
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

// statusFor derives the git-shaped status from a path's first/last diff:
// added if the first OldText was empty, else deleted if the last NewText is
// empty, else modified. Checking added first means a file created and later
// emptied still reads as added.
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

// scope summarizes the touched-path set against the workspace root. An empty
// root disables the anomaly check rather than guessing one, since a false
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

// isOutside reports whether p falls outside the workspace root. A relative p
// is always inside by construction; only an absolute path outside root trips
// the alarm.
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

// topLevelDir names the top-level directory a path counts toward for the
// breadth signal: under root, the first segment of the root-relative path
// ("." for a file directly in root); outside root (or with no root), the
// path's own leading segment.
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

// firstSegment returns the first slash-separated element of a relative path,
// or "." for a bare filename (the root-itself bucket).
func firstSegment(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return "."
}

// capDiff clips original/modified to diffDisplayCap, reporting whether
// either side was clipped. Clipping is on bytes and may split a multi-byte
// rune at the boundary, acceptable for a "diff too large" fallback.
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
