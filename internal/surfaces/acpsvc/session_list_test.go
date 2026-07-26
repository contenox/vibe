package acpsvc

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_ParseDBTime_SQLiteDriverFormat pins the layout the sqlite driver
// actually stores for a bound time.Time (Go's time.Time.String() output).
// Before this layout was handled, every session/list row lost its updatedAt
// and the sidebar sort silently collapsed to random-UUID order.
func TestUnit_ParseDBTime_SQLiteDriverFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"2026-07-15 21:43:19.742065508 +0000 UTC", time.Date(2026, 7, 15, 21, 43, 19, 742065508, time.UTC)},
		{"2026-07-16 05:34:46.365453246 +0000 UTC", time.Date(2026, 7, 16, 5, 34, 46, 365453246, time.UTC)},
		{"2026-07-16 07:00:00 +0200 CEST", time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)},
		{"2026-07-16T05:34:46Z", time.Date(2026, 7, 16, 5, 34, 46, 0, time.UTC)},
		{"2026-07-16 05:34:46", time.Date(2026, 7, 16, 5, 34, 46, 0, time.UTC)},
	} {
		ts, ok := parseDBTimeString(tc.in)
		require.True(t, ok, "parseDBTimeString(%q) must parse", tc.in)
		assert.True(t, ts.UTC().Equal(tc.want), "parseDBTimeString(%q) = %v, want %v", tc.in, ts.UTC(), tc.want)
	}
	_, ok := parseDBTimeString("not a time")
	assert.False(t, ok)
}

func listRow(id string, at string) sessionListRow {
	r := sessionListRow{internalID: id, name: "acp-" + id}
	if at != "" {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			panic(err)
		}
		r.updatedAt = ts
		r.hasTime = true
	}
	return r
}

// TestUnit_SessionListOrder_FreshestFirst pins the roster order: most recent
// activity first, never-messaged sessions after all sessions with activity,
// ties broken deterministically.
func TestUnit_SessionListOrder_FreshestFirst(t *testing.T) {
	rows := []sessionListRow{
		listRow("aaa", ""),
		listRow("bbb", "2026-07-16T05:00:00Z"),
		listRow("ccc", "2026-07-16T12:00:00Z"),
		listRow("ddd", "2026-07-15T22:00:00Z"),
		listRow("eee", "2026-07-16T12:00:00Z"), // tie with ccc
		listRow("fff", ""),
	}
	sort.Slice(rows, func(i, j int) bool { return sessionListRowLess(rows[i], rows[j]) })

	var order []string
	for _, r := range rows {
		order = append(order, r.internalID)
	}
	// eee before ccc on the id tie-break (descending); no-time rows last,
	// also id-descending.
	assert.Equal(t, []string{"eee", "ccc", "bbb", "ddd", "fff", "aaa"}, order)
}

// TestUnit_SessionListCursor_ResumesWithoutSkipOrDup walks a sorted roster
// page by page through the cursor codec and requires every row to appear
// exactly once, including when the boundary row vanished between pages.
func TestUnit_SessionListCursor_ResumesWithoutSkipOrDup(t *testing.T) {
	rows := []sessionListRow{
		listRow("eee", "2026-07-16T12:00:00Z"),
		listRow("ccc", "2026-07-16T12:00:00Z"),
		listRow("bbb", "2026-07-16T05:00:00Z"),
		listRow("ddd", "2026-07-15T22:00:00Z"),
		listRow("fff", ""),
		listRow("aaa", ""),
	}

	const pageSize = 2
	var seen []string
	cursor := ""
	for range 10 {
		start := 0
		if cursor != "" {
			start = listSessionsResume(rows, cursor)
		}
		end := min(start+pageSize, len(rows))
		if start >= end {
			break
		}
		for _, r := range rows[start:end] {
			seen = append(seen, r.internalID)
		}
		if end >= len(rows) {
			break
		}
		cursor = listSessionsCursor(rows[end-1])
	}
	assert.Equal(t, []string{"eee", "ccc", "bbb", "ddd", "fff", "aaa"}, seen)

	// A boundary row deleted between pages must not skip its successors.
	cursor = listSessionsCursor(rows[1]) // ccc
	remaining := append(append([]sessionListRow{}, rows[:1]...), rows[2:]...)
	resume := listSessionsResume(remaining, cursor)
	require.Less(t, resume, len(remaining))
	assert.Equal(t, "bbb", remaining[resume].internalID)
}
