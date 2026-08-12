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

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/libacp"
)

const (
	weightEdit    = 4
	weightDelete  = 4
	weightMove    = 3
	weightRead    = 2
	weightFetch   = 2
	weightExecute = 2
	weightOther   = 1
)

const maxChangedFiles = 100

const diffDisplayCap = 128 * 1024

const maxOutsidePaths = 20

// ChangedFile is one file the mission's unit wrote, with Score its Stage-1 DOI and Status derived from the first and last diff seen.
type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "added" | "modified" | "deleted"
	Score  int    `json:"score"`
}

// The three status strings ChangedFile.Status draws from.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
)

// ScopeStats is the Stage-2 scope summary: how broadly the unit ranged and whether it left its lane; all fields are advisory.
type ScopeStats struct {
	Files        int      `json:"files"`
	Dirs         int      `json:"dirs"`
	Anomaly      bool     `json:"anomaly"`
	OutsidePaths []string `json:"outsidePaths,omitempty"`
}

// Changes is the GET /missions/{id}/changes response, with Files always non-nil and Incomplete set when the list was capped.
type Changes struct {
	Files      []ChangedFile `json:"files"`
	Incomplete bool          `json:"incomplete"`
	Scope      ScopeStats    `json:"scope"`
}

// Diff is the GET /missions/{id}/changes/diff?path= response fed to Monaco's DiffEditor; Truncated is set when either side was clipped.
type Diff struct {
	Original  string `json:"original"`
	Modified  string `json:"modified"`
	Truncated bool   `json:"truncated,omitempty"`
}

type missionGetter interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
}

// SessionJournalReader is the kernel capability, reached by type assertion, that returns a session's raw journal; ok is false for an unknown instance or session.
type SessionJournalReader interface {
	SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool)
}

// Service answers the two attention-layer endpoints and is read-only by construction: no method here mutates a mission.
type Service interface {
	// Changes folds mission id's session journal into the ordered changed-files list plus the scope summary; a mission whose unit left no recoverable journal yields an empty, non-error Changes.
	Changes(ctx context.Context, missionID string) (*Changes, error)

	// Diff returns the {original, modified} pair for one changed path in mission id, truncated to diffDisplayCap; a path the mission never wrote yields a not-found error.
	Diff(ctx context.Context, missionID, filePath string) (*Diff, error)
}

type service struct {
	missions missionGetter
	journal  SessionJournalReader
}

// New builds the service over a mission resolver and the kernel journal reader; journal may be nil, in which case every mission reads as having no recorded work.
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

type fileFold struct {
	firstOld    string
	lastNew     string
	haveFirst   bool
	firstSeenAt int
}

type folded struct {
	files      map[string]*fileFold
	scores     map[string]int
	touched    map[string]struct{}
	touchOrder []string
}

func fold(updates []libacp.SessionNotification) *folded {
	f := &folded{
		files:   make(map[string]*fileFold),
		scores:  make(map[string]int),
		touched: make(map[string]struct{}),
	}
	// callWeights dedupes weight per (tool call, path) across an invocation's notifications.
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

func topLevelDir(root, p string) string {
	cp := filepath.Clean(p)
	if root != "" && root != "." && filepath.IsAbs(cp) {
		if rel, err := filepath.Rel(root, cp); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return firstSegment(filepath.ToSlash(rel))
		}
	}
	if filepath.IsAbs(cp) {
		trimmed := strings.TrimPrefix(filepath.ToSlash(cp), "/")
		return "/" + firstSegment(trimmed)
	}
	return firstSegment(filepath.ToSlash(cp))
}

func firstSegment(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return "."
}

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
