package composer

import "testing"

var sampleHistory = []string{"first prompt", "second prompt", "third prompt"}

// TestUnit_HistoryRecallSequence walks the standard shell semantics: Up
// stashes the draft and walks back, Down walks forward, and stepping past
// the newest entry restores the draft with its caret.
func TestUnit_HistoryRecallSequence(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.InsertString("in progress")
	c.Home()
	c.CursorRight() // caret somewhere the restore must reproduce

	steps := []struct {
		name string
		up   bool
		want string
	}{
		{"first up recalls the newest", true, "third prompt"},
		{"second up", true, "second prompt"},
		{"third up", true, "first prompt"},
		{"down walks back toward the draft", false, "second prompt"},
		{"down again", false, "third prompt"},
		{"down past the newest restores the draft", false, "in progress"},
	}
	for _, s := range steps {
		if s.up {
			c.CursorUp()
		} else {
			c.CursorDown()
		}
		if got := c.Draft(); got != s.want {
			t.Fatalf("%s: draft = %q, want %q", s.name, got, s.want)
		}
	}
	if c.line != 0 || c.off != 1 {
		t.Fatalf("restored caret = (%d, %d), want the stashed (0, 1)", c.line, c.off)
	}
}

// TestUnit_HistoryEdges pins the two ends and the empty case, where the
// shell convention is to stay put rather than wrap around.
func TestUnit_HistoryEdges(t *testing.T) {
	c := New()
	if c.ShouldRecallUp() {
		t.Fatal("ShouldRecallUp() with no history")
	}
	if c.HistoryUp() {
		t.Fatal("HistoryUp() with no history changed the buffer")
	}
	if c.HistoryDown() {
		t.Fatal("HistoryDown() with no recall in progress changed the buffer")
	}

	c.SetHistory(sampleHistory)
	if !c.ShouldRecallUp() {
		t.Fatal("ShouldRecallUp() = false on the first line with history")
	}
	if c.ShouldRecallDown() {
		t.Fatal("ShouldRecallDown() = true with no recall in progress")
	}

	for i := 0; i < len(sampleHistory); i++ {
		if !c.HistoryUp() {
			t.Fatalf("HistoryUp() step %d = false", i)
		}
	}
	if c.HistoryUp() {
		t.Fatal("HistoryUp() at the oldest entry reported a change")
	}
	if got := c.Draft(); got != sampleHistory[0] {
		t.Fatalf("draft = %q, want to stay at the oldest entry", got)
	}
	if !c.ShouldRecallDown() {
		t.Fatal("ShouldRecallDown() = false during recall")
	}
}

// TestUnit_HistoryEditDetaches is standard readline behavior: once a
// recalled entry is edited it is the user's own draft again, so Down no
// longer walks history and the next Up starts a fresh walk that stashes the
// edited text.
func TestUnit_HistoryEditDetaches(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)

	c.CursorUp()
	c.InsertString(" edited")
	if got := c.Draft(); got != "third prompt edited" {
		t.Fatalf("draft = %q, want the edited recall", got)
	}
	if c.ShouldRecallDown() {
		t.Fatal("ShouldRecallDown() = true after an edit detached the recall")
	}

	c.CursorDown()
	if got := c.Draft(); got != "third prompt edited" {
		t.Fatalf("Down after an edit changed the draft to %q", got)
	}

	// A fresh walk stashes the edited text and can restore it.
	c.CursorUp()
	if got := c.Draft(); got != "third prompt" {
		t.Fatalf("draft = %q, want a fresh recall of the newest entry", got)
	}
	c.CursorDown()
	if got := c.Draft(); got != "third prompt edited" {
		t.Fatalf("draft = %q, want the edited text restored from the stash", got)
	}
}

// TestUnit_HistoryOnlyAtBufferEdges: with a multiline draft, Up and Down are
// caret movement until the caret reaches the first or last line.
func TestUnit_HistoryOnlyAtBufferEdges(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.SetDraft("alpha\nbeta\ngamma")

	if c.ShouldRecallUp() {
		t.Fatal("ShouldRecallUp() = true with the caret on the last line")
	}
	c.CursorUp()
	if c.line != 1 || c.Draft() != "alpha\nbeta\ngamma" {
		t.Fatalf("Up moved to line %d / draft %q, want caret movement", c.line, c.Draft())
	}
	c.CursorUp()
	if c.line != 0 || c.Draft() != "alpha\nbeta\ngamma" {
		t.Fatalf("Up moved to line %d / draft %q, want caret movement", c.line, c.Draft())
	}

	// Now at the top edge: the next Up is a recall.
	if !c.ShouldRecallUp() {
		t.Fatal("ShouldRecallUp() = false with the caret on the first line")
	}
	c.CursorUp()
	if got := c.Draft(); got != "third prompt" {
		t.Fatalf("draft = %q, want the newest history entry", got)
	}

	// And stepping back past the newest returns the whole multiline draft.
	c.CursorDown()
	if got := c.Draft(); got != "alpha\nbeta\ngamma" {
		t.Fatalf("draft = %q, want the stashed multiline draft", got)
	}
	if c.line != 0 || c.off != 4 {
		t.Fatalf("restored caret = (%d, %d), want the stashed (0, 4)", c.line, c.off)
	}
}

// TestUnit_HistoryResetOnSubmitAndReseed: the composer keeps no store of its
// own. Submit ends any recall, and the app re-seeds the list from the
// session's persisted user turns.
func TestUnit_HistoryResetOnSubmitAndReseed(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.CursorUp()
	if _, ok := c.Submit(); !ok {
		t.Fatal("Submit() refused a recalled entry")
	}
	if c.ShouldRecallDown() {
		t.Fatal("a recall survived Submit()")
	}
	if !c.Empty() {
		t.Fatalf("buffer = %q after Submit, want cleared", c.Draft())
	}

	// SetHistory during a recall ends the WALK and leaves the buffer alone:
	// the app re-seeds after every turn, and that must never yank text away.
	c.CursorUp()
	if got := c.Draft(); got != "third prompt" {
		t.Fatalf("draft = %q, want the newest entry", got)
	}
	c.SetHistory([]string{"second prompt", "third prompt", "the submitted one"})
	if got := c.Draft(); got != "third prompt" {
		t.Fatalf("SetHistory changed the buffer to %q", got)
	}
	c.CursorUp()
	if got := c.Draft(); got != "the submitted one" {
		t.Fatalf("draft = %q, want the newest entry of the re-seeded list", got)
	}
}

// TestUnit_SetHistoryKeepsTheStashRecoverable is the data-loss case the
// re-seed used to cause.
//
// The app re-seeds the recall list on its own schedule, after every turn. It
// can land while the user is mid-recall: they have pressed Up a few times and
// are reading an old prompt, and the only copy of the draft they were typing
// is the stash. Dropping it there deletes typed text on a timer the user
// cannot see. So SetHistory ends the walk — the next Up starts fresh from the
// newest entry — but Down still steps back to the draft.
func TestUnit_SetHistoryKeepsTheStashRecoverable(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.InsertString("half-written thought")
	c.CursorUp() // stashes the draft, recalls "third prompt"

	c.SetHistory([]string{"third prompt", "a newer turn"})

	// The buffer is left exactly as it was.
	if got := c.Draft(); got != "third prompt" {
		t.Fatalf("SetHistory changed the buffer to %q", got)
	}
	// And the draft is still reachable, which is the whole point.
	if !c.ShouldRecallDown() {
		t.Fatal("ShouldRecallDown() = false: the stashed draft is unreachable")
	}
	c.CursorDown()
	if got := c.Draft(); got != "half-written thought" {
		t.Fatalf("draft = %q, want the stash restored", got)
	}
	// Restoring consumes it; there is nothing further forward.
	if c.ShouldRecallDown() {
		t.Fatal("ShouldRecallDown() = true after the stash was restored")
	}

	// A fresh Up after the re-seed walks the NEW list from its newest entry
	// and stashes what is on screen now.
	c.CursorUp()
	if got := c.Draft(); got != "a newer turn" {
		t.Fatalf("draft = %q, want the newest entry of the re-seeded list", got)
	}
	c.CursorDown()
	if got := c.Draft(); got != "half-written thought" {
		t.Fatalf("draft = %q, want the typed text back", got)
	}
}

// TestUnit_SetHistoryMidRecallDoesNotStashARecalledEntryOverTheDraft: after a
// re-seed the buffer holds a history entry, and a subsequent Up must not
// stash THAT over the user's real draft — the entry is already in the list,
// the draft exists nowhere else.
func TestUnit_SetHistoryMidRecallPreservesTheOriginalStash(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.InsertString("the only copy")
	c.CursorUp()
	c.SetHistory(sampleHistory)

	c.CursorUp() // a fresh walk over a buffer that is itself a history entry
	c.CursorDown()
	c.CursorDown() // past the newest, back to the stash

	if got := c.Draft(); got != "the only copy" {
		t.Fatalf("draft = %q, want the original typed text", got)
	}
}

// TestUnit_EditStillDropsTheStash: SetHistory is the only thing that spares
// it. An edit means the text on screen is the user's own now, so the draft it
// displaced is gone with the recall it belonged to — standard readline.
func TestUnit_EditStillDropsTheStash(t *testing.T) {
	c := New()
	c.SetHistory(sampleHistory)
	c.InsertString("draft")
	c.CursorUp()
	c.SetHistory(sampleHistory)

	c.InsertString(" edited")
	if c.ShouldRecallDown() {
		t.Fatal("the stash survived an edit")
	}
	c.CursorDown()
	if got := c.Draft(); got != "third prompt edited" {
		t.Fatalf("Down after an edit changed the draft to %q", got)
	}
}

// TestUnit_MentionSpan covers the trigger rule file-addressing depends on:
// an `@` token at the buffer start or after whitespace, never inside a word.
func TestUnit_MentionSpan(t *testing.T) {
	cases := []struct {
		name      string
		draft     string
		left      int // CursorLeft presses from the end of the draft
		wantOK    bool
		wantStart int
		wantLen   int
		wantQuery string
	}{
		{name: "bare at the buffer start", draft: "@", wantOK: true, wantStart: 0, wantLen: 1},
		{name: "token at the buffer start", draft: "@main.go", wantOK: true, wantStart: 0, wantLen: 8, wantQuery: "main.go"},
		{name: "after a space", draft: "look at @cmd/ma", wantOK: true, wantStart: 8, wantLen: 7, wantQuery: "cmd/ma"},
		{name: "caret before the at-sign", draft: "look at @cmd", left: 4, wantOK: true, wantStart: 8, wantLen: 4, wantQuery: "cmd"},
		{name: "caret inside the token", draft: "look at @cmd", left: 1, wantOK: true, wantStart: 8, wantLen: 4, wantQuery: "cmd"},
		{name: "mid-word is not a mention", draft: "user@host", wantOK: false},
		{name: "mid-word with the caret on the at-sign", draft: "user@host", left: 4, wantOK: false},
		{name: "caret in whitespace", draft: "@main.go ", wantOK: false},
		{name: "empty buffer", draft: "", wantOK: false},
		{name: "no mention at all", draft: "just prose", wantOK: false},
		{name: "on a later line", draft: "first line\n@sec", wantOK: true, wantStart: 11, wantLen: 4, wantQuery: "sec"},
		{name: "second mention in a line", draft: "@a and @b", wantOK: true, wantStart: 7, wantLen: 2, wantQuery: "b"},
		{name: "email-shaped token", draft: "mail me at a@b.com", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.SetDraft(tc.draft)
			for i := 0; i < tc.left; i++ {
				c.CursorLeft()
			}

			start, length, query, ok := c.MentionSpan()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (span %d+%d %q)", ok, tc.wantOK, start, length, query)
			}
			if !ok {
				return
			}
			if start != tc.wantStart || length != tc.wantLen || query != tc.wantQuery {
				t.Fatalf("span = (%d, %d, %q), want (%d, %d, %q)",
					start, length, query, tc.wantStart, tc.wantLen, tc.wantQuery)
			}
			// The span must address exactly the token in Draft().
			runes := []rune(c.Draft())
			if got, want := string(runes[start:start+length]), "@"+query; got != want {
				t.Fatalf("Draft()[span] = %q, want %q", got, want)
			}
		})
	}
}

// TestUnit_SpliceMention: the replacement lands over the span with one
// trailing space and the caret after it, ready for the next word.
func TestUnit_SpliceMention(t *testing.T) {
	cases := []struct {
		name    string
		draft   string
		left    int
		repl    string
		want    string
		wantLn  int
		wantOff int
	}{
		{
			name: "completes a partial token", draft: "look at @cmd/ma",
			repl: "@cmd/main.go", want: "look at @cmd/main.go ", wantLn: 0, wantOff: 21,
		},
		{
			name: "completes a bare trigger", draft: "@",
			repl: "@README.md", want: "@README.md ", wantLn: 0, wantOff: 11,
		},
		{
			name: "keeps the text after the span", draft: "@doc and more", left: 9,
			repl: "@docs/x.md", want: "@docs/x.md  and more", wantLn: 0, wantOff: 11,
		},
		{
			name: "splices on a later line", draft: "first\n@sec",
			repl: "@second/file.go", want: "first\n@second/file.go ", wantLn: 1, wantOff: 16,
		},
		{
			name: "a multibyte path keeps the caret in cells", draft: "@日本",
			repl: "@日本語/ファイル.go", want: "@日本語/ファイル.go ", wantLn: 0, wantOff: 13,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			c.SetDraft(tc.draft)
			for i := 0; i < tc.left; i++ {
				c.CursorLeft()
			}
			start, length, _, ok := c.MentionSpan()
			if !ok {
				t.Fatalf("MentionSpan() = false for %q", tc.draft)
			}

			c.SpliceMention(start, length, tc.repl)
			if got := c.Draft(); got != tc.want {
				t.Fatalf("Draft() = %q, want %q", got, tc.want)
			}
			if c.line != tc.wantLn || c.off != tc.wantOff {
				t.Fatalf("caret = (%d, %d), want (%d, %d)", c.line, c.off, tc.wantLn, tc.wantOff)
			}
		})
	}
}

// TestUnit_SpliceMentionOutOfRange: the completion that produced a span is
// asynchronous, so a stale span must be ignored rather than panic.
func TestUnit_SpliceMentionOutOfRange(t *testing.T) {
	for _, span := range [][2]int{{-1, 2}, {0, -1}, {4, 20}, {99, 1}} {
		c := New()
		c.SetDraft("@abc")
		c.SpliceMention(span[0], span[1], "@x.go")
		if got := c.Draft(); got != "@abc" {
			t.Fatalf("span %v changed the draft to %q", span, got)
		}
	}
}

// TestUnit_SpliceMentionDetachesHistory: a splice is an edit like any other.
func TestUnit_SpliceMentionDetachesHistory(t *testing.T) {
	c := New()
	c.SetHistory([]string{"@old"})
	c.CursorUp()
	start, length, _, ok := c.MentionSpan()
	if !ok {
		t.Fatal("MentionSpan() = false on the recalled entry")
	}
	c.SpliceMention(start, length, "@new.go")
	if c.ShouldRecallDown() {
		t.Fatal("a recall survived SpliceMention()")
	}
}
