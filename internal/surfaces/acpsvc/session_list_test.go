package acpsvc

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestUnit_SessionListOrder_FreshestFirst pins the roster order: freshest
// activity first, never-messaged sessions last, ties broken by id.
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
	// eee before ccc: same timestamp, tie-break falls to id descending.
	assert.Equal(t, []string{"eee", "ccc", "bbb", "ddd", "fff", "aaa"}, order)
}

// TestUnit_SessionListCursor_ResumesWithoutSkipOrDup pins that paging through
// the cursor codec visits every row exactly once, even across a deletion.
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
