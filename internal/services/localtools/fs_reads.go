package localtools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// SQLDialect selects the placeholder style used for the local_fs_reads table.
type SQLDialect int

const (
	// DialectSQLite uses '?' positional placeholders.
	DialectSQLite SQLDialect = iota
	// DialectPostgres uses $1..$N placeholders.
	DialectPostgres
)

func (d SQLDialect) rebind(query string) string {
	if d != DialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			b.WriteByte(query[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func escapeSQLiteLike(s string) string { return likeEscape(s) }

const readMarkerTTL = 24 * time.Hour

var prunedSessions sync.Map

func sessionIDFromContext(ctx context.Context) string {
	v := ctx.Value(runtimetypes.SessionIDContextKey)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func rangeReadMarkerPath(absPath string) string {
	// Distinct key: sharing the full-read key would let a range read unlock a full-file write.
	return "range:" + absPath
}

func fullHashMarkerPath(absPath, hash string) string  { return "fullhash:" + absPath + ":" + hash }
func rangeHashMarkerPath(absPath, hash string) string { return "rangehash:" + absPath + ":" + hash }

func (h *LocalFSTools) recordFullRead(ctx context.Context, absPath string, content []byte) {
	hash := contentHash(content)
	h.recordReadMarker(ctx, absPath)
	h.recordReadMarker(ctx, fullHashMarkerPath(absPath, hash))
}

func (h *LocalFSTools) recordRangeRead(ctx context.Context, absPath string, content []byte) {
	hash := contentHash(content)
	h.recordReadMarker(ctx, rangeReadMarkerPath(absPath))
	h.recordReadMarker(ctx, rangeHashMarkerPath(absPath, hash))
}

func (h *LocalFSTools) recordReadMarker(ctx context.Context, markerPath string) {
	if h.db == nil {
		return
	}
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}
	exec := h.db.WithoutTransaction()
	_, _ = exec.ExecContext(ctx, h.dialect.rebind(
		`INSERT INTO local_fs_reads (session_id, path, last_read_at) VALUES (?, ?, ?)
		 ON CONFLICT (session_id, path) DO UPDATE SET last_read_at = excluded.last_read_at`),
		sessionID, markerPath, time.Now().UTC(),
	)
	h.pruneStaleMarkers(ctx, sessionID)
}

func (h *LocalFSTools) pruneStaleMarkers(ctx context.Context, sessionID string) {
	if _, done := prunedSessions.LoadOrStore(sessionID, struct{}{}); done {
		return
	}
	exec := h.db.WithoutTransaction()
	_, _ = exec.ExecContext(ctx, h.dialect.rebind(
		`DELETE FROM local_fs_reads WHERE session_id = ? AND last_read_at < ?`),
		sessionID, time.Now().UTC().Add(-readMarkerTTL),
	)
}

// PruneLocalFSReads removes read markers older than the retention window across all sessions, intended for a periodic maintenance job; the per-session prune above only covers sessions this process has seen.
func PruneLocalFSReads(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, dialect SQLDialect, olderThan time.Duration) error {
	if olderThan <= 0 {
		olderThan = readMarkerTTL
	}
	_, err := exec.ExecContext(ctx, dialect.rebind(
		`DELETE FROM local_fs_reads WHERE last_read_at < ?`),
		time.Now().UTC().Add(-olderThan),
	)
	return err
}

func (h *LocalFSTools) readTrackingDisabled(ctx context.Context) bool {
	return h.db == nil || sessionIDFromContext(ctx) == ""
}

func (h *LocalFSTools) hasPriorRead(ctx context.Context, absPath string) bool {
	return h.hasReadMarker(ctx, absPath)
}

func (h *LocalFSTools) hasPriorRangeRead(ctx context.Context, absPath string) bool {
	return h.hasReadMarker(ctx, rangeReadMarkerPath(absPath))
}

func (h *LocalFSTools) hasCurrentFullRead(ctx context.Context, absPath, currentHash string) bool {
	return h.hasReadMarker(ctx, fullHashMarkerPath(absPath, currentHash))
}

func (h *LocalFSTools) hasCurrentRangeRead(ctx context.Context, absPath, currentHash string) bool {
	return h.hasReadMarker(ctx, rangeHashMarkerPath(absPath, currentHash))
}

func (h *LocalFSTools) hasAnyPriorRead(ctx context.Context, absPath string) bool {
	if h.readTrackingDisabled(ctx) {
		return true
	}
	return h.hasReadMarker(ctx, absPath) || h.hasReadMarker(ctx, rangeReadMarkerPath(absPath))
}

func (h *LocalFSTools) hasReadMarker(ctx context.Context, markerPath string) bool {
	if h.db == nil {
		return true
	}
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return true
	}
	exec := h.db.WithoutTransaction()
	var dummy string
	err := exec.QueryRowContext(ctx, h.dialect.rebind(
		`SELECT path FROM local_fs_reads WHERE session_id = ? AND path = ?`),
		sessionID, markerPath,
	).Scan(&dummy)
	if err == nil {
		return true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	// Any other DB error: fail open, so a tracker outage does not block the model from working.
	return true
}

// InvalidateSessionReads clears every read marker for a session; call this after context compaction, since the read-dedup stub is only true while the earlier read is still in the model's context.
func (h *LocalFSTools) InvalidateSessionReads(ctx context.Context, sessionID string) error {
	if h.db == nil || sessionID == "" {
		return nil
	}
	exec := h.db.WithoutTransaction()
	_, err := exec.ExecContext(ctx, h.dialect.rebind(
		`DELETE FROM local_fs_reads WHERE session_id = ?`), sessionID)
	return err
}

func (h *LocalFSTools) invalidateReads(ctx context.Context, absPath string) {
	if h.db == nil {
		return
	}
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}

	fullPrefix := likeEscape("fullhash:"+absPath+":") + "%"
	rangePrefix := likeEscape("rangehash:"+absPath+":") + "%"

	exec := h.db.WithoutTransaction()
	_, _ = exec.ExecContext(ctx, h.dialect.rebind(
		`DELETE FROM local_fs_reads
		  WHERE session_id = ?
		    AND (
		      path = ?
		      OR path = ?
		      OR path LIKE ? ESCAPE '\'
		      OR path LIKE ? ESCAPE '\'
		    )`),
		sessionID,
		absPath,
		rangeReadMarkerPath(absPath),
		fullPrefix,
		rangePrefix,
	)
}

type mutationGate struct {
	denial   string
	denied   bool
	exists   bool
	content  []byte
	hash     string
	verified bool
}

func (h *LocalFSTools) requireReadBeforeMutation(ctx context.Context, absPath, displayPath string, requirement readRequirement) mutationGate {
	info, err := h.stat(ctx, absPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A brand-new file needs no prior read.
			return mutationGate{exists: false}
		}
		// Permission/IO error: let the actual mutation attempt surface it.
		return mutationGate{exists: true}
	}
	if info.IsDir() {
		return mutationGate{exists: true}
	}

	if h.readTrackingDisabled(ctx) {
		content, readErr := h.fileIO.ReadFile(ctx, absPath)
		if readErr != nil {
			return mutationGate{exists: true}
		}
		return mutationGate{exists: true, content: content, hash: contentHash(content), verified: true}
	}

	content, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		// Let the actual mutation attempt surface the I/O error.
		return mutationGate{exists: true}
	}
	hash := contentHash(content)
	allow := mutationGate{exists: true, content: content, hash: hash, verified: true}

	deny := func(format string) mutationGate {
		return mutationGate{
			denial:  fmt.Sprintf(format, displayPath, displayPath),
			denied:  true,
			exists:  true,
			content: content,
			hash:    hash,
		}
	}

	switch requirement {
	case requireFullFileRead:
		if h.hasCurrentFullRead(ctx, absPath, hash) {
			return allow
		}
		if h.hasCurrentRangeRead(ctx, absPath, hash) {
			return deny(readBeforeWriteFullReadDenial)
		}
		if h.hasAnyPriorRead(ctx, absPath) {
			return deny(readBeforeWriteStaleReadDenial)
		}
		return deny(readBeforeWriteDenial)

	case requireAnyFileRead:
		if h.hasCurrentFullRead(ctx, absPath, hash) || h.hasCurrentRangeRead(ctx, absPath, hash) {
			return allow
		}
		if h.hasAnyPriorRead(ctx, absPath) {
			return deny(readBeforeWriteStaleReadDenial)
		}
		return deny(readBeforeWriteDenial)

	default:
		if h.hasCurrentFullRead(ctx, absPath, hash) {
			return allow
		}
		if h.hasAnyPriorRead(ctx, absPath) {
			return deny(readBeforeWriteStaleReadDenial)
		}
		return deny(readBeforeWriteDenial)
	}
}

func (h *LocalFSTools) confirmUnchanged(ctx context.Context, absPath, expected string) (bool, []byte) {
	current, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return expected == "", nil
		}
		return false, nil
	}
	return contentHash(current) == expected, current
}
