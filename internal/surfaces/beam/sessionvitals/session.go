package sessionvitals

import (
	"strings"

	libacp "github.com/contenox/contenox/libacp"
)

// idTailCells is how much of a session uuid ShortName keeps.
const idTailCells = 8

// Label is what a session is called wherever it is named: the server-published
// title when there is one, the shortened session name otherwise, and the
// shortened id as the last resort. The precedence matches acpsvc's own
// (sessionListTitle) — a surface must never derive a label of its own, or it
// puts a name on screen that no other client shows.
func Label(title, name, id string) string {
	if title != "" {
		return title
	}
	if name != "" {
		return ShortName(name)
	}
	return ShortName(id)
}

// ShortName is the id fallback's display form: the prefix plus the first
// idTailCells of the tail, so `beam-20a88ab8-4f2e-4b0d-9c31-6f1a2b3c4d5e`
// reads as `beam-20a88ab8`. A name whose tail is not long enough to be a uuid
// is returned untouched.
func ShortName(name string) string {
	i := strings.IndexByte(name, '-')
	if i < 0 {
		return name
	}
	tail := name[i+1:]
	if len(tail) <= idTailCells {
		return name
	}
	return name[:i+1] + tail[:idTailCells]
}

// RosterEntry is one row of a session switcher: the identity and the label a
// surface shows, plus whether it is the session already on screen. It carries
// no decoration — an "(active)" suffix or a highlight is the surface's own.
type RosterEntry struct {
	ID     libacp.SessionID
	Label  string
	Active bool
}

// Roster projects an ACP session roster into switcher rows: at most limit
// entries, id-less entries dropped, each labelled by Label's precedence, and
// the active session moved to the front so accepting an untouched switcher is
// a no-op. The active session is kept in the list rather than filtered out,
// which is what makes that no-op possible. A limit of zero or less yields no
// entries.
func Roster(infos []libacp.SessionInfo, active libacp.SessionID, limit int) []RosterEntry {
	if limit <= 0 {
		return nil
	}
	entries := make([]RosterEntry, 0, len(infos))
	for _, s := range infos {
		if len(entries) >= limit {
			break
		}
		if s.SessionID == "" {
			continue
		}
		// The full id, not ShortName's: a roster row is how one session is
		// told apart from its neighbours, and a shortened uuid can collide.
		label := strings.TrimSpace(s.Title)
		if label == "" {
			label = string(s.SessionID)
		}
		entries = append(entries, RosterEntry{
			ID:     s.SessionID,
			Label:  label,
			Active: s.SessionID == active,
		})
	}
	for i, e := range entries {
		if !e.Active {
			continue
		}
		moved := append([]RosterEntry{entries[i]}, entries[:i]...)
		entries = append(moved, entries[i+1:]...)
		break
	}
	return entries
}
