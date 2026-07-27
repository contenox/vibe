package composer

// History recall, with the standard shell semantics the blueprint asks for
// (MVP item 7, D12): Up walks back through earlier entries, Down walks
// forward, and stepping past the newest entry restores the draft that recall
// displaced. The list itself is not the composer's to own — the app seeds it
// from the session's persisted user turns, so slash and shell lines
// participate exactly as far as the session persisted them, and there is no
// bespoke store here to drift out of sync.

// SetHistory replaces the recall list, oldest entry first. Any recall in
// progress ends and the buffer is left untouched: re-seeding after a submit
// must never yank the text the user is typing.
//
// The STASH survives. The app re-seeds this list after every turn, on its own
// schedule, and that can land mid-recall — the user has pressed Up a few
// times and is reading an old prompt, holding an unsent draft that only the
// stash still remembers. Dropping it there would destroy typed text on a
// timer the user cannot see, which is the one thing a composer must never do.
// So the recall ENDS (the walk restarts from the newest entry next time) but
// Down still steps back to the draft, and any edit clears it the way an edit
// always has.
func (c *Composer) SetHistory(entries []string) {
	c.history = append([]string(nil), entries...)
	c.histIdx = -1
}

// ShouldRecallUp reports whether an Up key at the current caret means
// "recall" rather than "move the caret": the caret is on the first buffer
// line and there is history to walk. CursorUp already applies this
// internally; the predicate is exported so the app-shell can label or route
// the key without duplicating the rule.
func (c *Composer) ShouldRecallUp() bool {
	return c.line == 0 && len(c.history) > 0
}

// ShouldRecallDown reports whether a Down key means "step forward through
// recall": the caret is on the last buffer line and there is somewhere
// forward to go — either a recall in progress, or a stashed draft still
// waiting to be stepped back to after SetHistory ended the walk. Down with
// neither is not a history action, because there is nothing newer than what
// is already on screen.
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
// stashed draft and ends the recall. It reports whether the buffer changed.
//
// With no recall in progress it still restores a surviving stash — that is
// the case a SetHistory mid-recall leaves behind, and the draft it holds is
// the user's unsent text. With neither there is nothing to step to and it is
// false.
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
//
// An existing stash is never overwritten. The only way to reach a second
// stashDraft without an intervening edit is a SetHistory that ended a recall
// and left the stash standing; the buffer at that point is a RECALLED entry,
// which is already in the history list, and stashing it over the user's draft
// would lose the one copy of the draft that exists.
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
// detaches it from history (standard readline behavior): the text is now the
// user's own draft, a later Up starts a fresh walk from the newest entry,
// and the stash it would have restored is gone with the draft it belonged
// to.
func (c *Composer) touch() { c.detach() }

// detach ends any recall in progress and drops the stash.
func (c *Composer) detach() {
	c.histIdx = -1
	c.stash, c.hasStash = nil, false
}
