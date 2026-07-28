package composer

// History recall follows standard shell semantics: Up walks back through
// earlier entries, Down walks forward, and stepping past the newest entry
// restores the displaced draft. The app owns the list, seeded from the
// session's persisted user turns; there is no separate store here to drift
// out of sync.

// SetHistory replaces the recall list, oldest entry first, and leaves the
// buffer untouched — re-seeding after a submit must never yank text the user
// is typing. Any recall in progress ends, but a surviving stash is not
// dropped: Down still steps back to it, since it may be the only copy of an
// unsent draft. An edit clears it as usual.
func (c *Composer) SetHistory(entries []string) {
	c.history = append([]string(nil), entries...)
	c.histIdx = -1
}

// ShouldRecallUp reports whether an Up key means recall rather than caret
// movement: the caret is on the first line and there is history to walk.
// Exported so the app-shell can label or route the key without duplicating
// the rule CursorUp applies internally.
func (c *Composer) ShouldRecallUp() bool {
	return c.line == 0 && len(c.history) > 0
}

// ShouldRecallDown reports whether a Down key means stepping forward
// through recall: the caret is on the last line and there is either a
// recall in progress or a stashed draft to return to. With neither, there
// is nothing newer than what is already on screen.
func (c *Composer) ShouldRecallDown() bool {
	return c.line == len(c.lines)-1 && (c.histIdx >= 0 || c.hasStash)
}

// HistoryUp loads the previous entry, stashing the in-progress draft on the
// first step. It reports whether the buffer changed: false with no history,
// and false at the oldest entry, where the shell convention is to stay put.
func (c *Composer) HistoryUp() bool {
	if len(c.history) == 0 {
		return false
	}
	if c.histIdx < 0 {
		c.stashDraft()
		c.histIdx = len(c.history) - 1
		c.setBuffer(c.history[c.histIdx])
		return true
	}
	if c.histIdx == 0 {
		return false
	}
	c.histIdx--
	c.setBuffer(c.history[c.histIdx])
	return true
}

// HistoryDown loads the next entry, and past the newest one restores the
// stashed draft and ends the recall. It reports whether the buffer changed:
// with no recall in progress it still restores a surviving stash (the case
// a SetHistory mid-recall leaves behind), and is false with neither.
func (c *Composer) HistoryDown() bool {
	if c.histIdx < 0 {
		if c.hasStash {
			c.restoreStash()
			return true
		}
		return false
	}
	if c.histIdx < len(c.history)-1 {
		c.histIdx++
		c.setBuffer(c.history[c.histIdx])
		return true
	}
	c.histIdx = -1
	c.restoreStash()
	return true
}

// stashDraft saves the buffer and caret displaced by the first recall step.
// An existing stash is never overwritten: the only way to reach a second
// call without an intervening edit is a SetHistory that left the stash
// standing, and the buffer at that point is already a history entry, not
// the user's draft.
func (c *Composer) stashDraft() {
	if c.hasStash {
		return
	}
	c.stash = make([][]rune, len(c.lines))
	for i, l := range c.lines {
		c.stash[i] = append([]rune(nil), l...)
	}
	c.stashLine, c.stashOff = c.line, c.off
	c.hasStash = true
}

// restoreStash puts the displaced draft back, caret and all.
func (c *Composer) restoreStash() {
	if !c.hasStash {
		c.clear()
		return
	}
	c.lines = c.stash
	c.line, c.off = c.stashLine, c.stashOff
	c.stash, c.hasStash = nil, false
}

// touch records that the buffer was edited. Editing a recalled entry
// detaches it from history: the text is now the user's own draft, a later
// Up starts a fresh walk, and the stash it would have restored is gone.
func (c *Composer) touch() { c.detach() }

// detach ends any recall in progress and drops the stash.
func (c *Composer) detach() {
	c.histIdx = -1
	c.stash, c.hasStash = nil, false
}
