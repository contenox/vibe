package contenoxcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
)

// ErrApprovalAborted is returned when the operator aborts the whole run from an
// approval prompt. Callers should treat it as a user-initiated cancellation,
// not a tool failure.
var ErrApprovalAborted = errors.New("run aborted at approval prompt")

const (
	// maxArgValueDisplay bounds how much of a single argument value is echoed
	// above the prompt.
	//
	// Printing a 100-line `replacement` argument in full pushes the diff — the
	// thing the decision should actually be based on — off the top of the
	// screen. The diff is rendered last, closest to the prompt, and the raw
	// argument is summarised rather than dumped.
	maxArgValueDisplay = 240

	// maxDiffLinesDisplay bounds the rendered diff. When it truncates, the
	// notice appears immediately above the prompt where it cannot be scrolled
	// past unnoticed.
	maxDiffLinesDisplay = 120

	// inputFlushWindow is how long to spend draining stray keystrokes typed
	// before the prompt was drawn.
	inputFlushWindow = 15 * time.Millisecond
)

// CLIApprovalOptions configures the interactive approval prompt.
//
// Deliberately absent: any form of "always allow this for the rest of the
// session". A cached grant would let this callback answer approve-actions
// without a human, which is precisely what validatePolicy already forbids
// policies themselves from doing (see the on_timeout=allow rejection in
// hitlservice/policy.go). The only legitimate way to stop being asked about a
// tool is an allow RULE in the policy document, where it is versioned,
// validated, and visible to every transport rather than cached in one client.
type CLIApprovalOptions struct {
	// AuditLog, when non-nil, receives one line per decision. Approvals are
	// security-relevant events and should be recoverable after the fact,
	// independent of whatever scrolled off the terminal.
	AuditLog func(req hitlservice.ApprovalRequest, decision string)
}

// NewCLIAskApproval returns an AskApproval callback suitable for interactive
// CLI use. It opens /dev/tty directly so it works even when stdin is piped.
//
// Unlike a naive prompt, this one:
//   - refuses to read approvals from a non-terminal stdin (a piped "y" must
//     never approve a mutation),
//   - drains buffered input before prompting, so a keystroke typed during the
//     previous prompt cannot silently answer this one,
//   - renders arguments in a stable order and bounded length, with the diff
//     last so it is adjacent to the decision,
//   - offers an explicit abort alongside approve/deny.
//
// Every call reaches a human. This callback has no memory between prompts and
// no way to answer one on its own.
func NewCLIAskApproval(w io.Writer) localtools.AskApproval {
	return NewCLIAskApprovalWithOptions(w, CLIApprovalOptions{})
}

func NewCLIAskApprovalWithOptions(w io.Writer, opts CLIApprovalOptions) localtools.AskApproval {
	var (
		mu      sync.Mutex // serialises prompts; two concurrent reads of one tty steal each other's input
		aborted bool
	)

	return func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		mu.Lock()
		defer mu.Unlock()

		if aborted {
			return false, ErrApprovalAborted
		}

		key := req.ToolsName + "." + req.ToolName

		// Let the task engine's asynchronous event bus flush 'step_started' to
		// stderr before drawing the prompt.
		//
		// TODO: this is a race workaround, not a fix. The engine should signal
		// that its log for this step has been emitted; sleeping means a slow
		// bus still interleaves and a fast one wastes a quarter second on every
		// approval.
		time.Sleep(250 * time.Millisecond)

		tty, isTerm, err := openControllingTerminal()
		if err != nil || !isTerm {
			// No terminal to ask on. Deny rather than reading a decision out of
			// a pipe: a "y" sitting in piped stdin is not consent.
			fmt.Fprintf(w, "\n  [denied: %s requires approval but no terminal is attached]\n", key)
			if opts.AuditLog != nil {
				opts.AuditLog(req, "denied-no-tty")
			}
			return false, nil
		}
		defer tty.Close()

		// Drop anything typed before the prompt was drawn. Without this, typing
		// "yy" at one prompt approves the next one before the operator has seen
		// it — the exact failure mode a per-call gate exists to prevent.
		flushInput(tty)

		renderApprovalRequest(w, req)

		fmt.Fprintln(w, "────────────────────────────────────────────────────")
		fmt.Fprint(w, "  Approve? [y]es / [N]o / [q]uit: ")

		line, ok, err := readLine(ctx, tty, w)
		if err != nil {
			return false, err
		}

		decision := "denied"
		approved := false
		switch {
		case !ok:
			// EOF or read error: deny.
		default:
			switch strings.TrimSpace(strings.ToLower(line)) {
			case "y", "yes":
				approved, decision = true, "approved"
			case "q", "quit", "abort":
				aborted = true
				decision = "aborted"
			}
		}

		if opts.AuditLog != nil {
			opts.AuditLog(req, decision)
		}
		if aborted {
			fmt.Fprintln(w, "  (run aborted)")
			return false, ErrApprovalAborted
		}
		return approved, nil
	}
}

// renderApprovalRequest prints the request with the diff last, so the most
// decision-relevant content sits directly above the prompt.
func renderApprovalRequest(w io.Writer, req hitlservice.ApprovalRequest) {
	fmt.Fprintln(w, "\n────────────────────────────────────────────────────")
	fmt.Fprintf(w, "  HITL approval required\n")
	fmt.Fprintf(w, "  Tools : %s\n", req.ToolsName)
	fmt.Fprintf(w, "  Tool  : %s\n", req.ToolName)

	if len(req.Args) > 0 {
		fmt.Fprintln(w, "  Args  :")
		// Map iteration order is randomised, so the same request printed twice
		// lists its arguments in different orders. For a gate a human is
		// pattern-matching on under fatigue, unstable layout is a real cost.
		keys := make([]string, 0, len(req.Args))
		for k := range req.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "    %s = %s\n", k, summariseArg(req.Args[k]))
		}
	}

	if req.Diff != "" {
		fmt.Fprintln(w, "  Diff  :")
		lines := strings.Split(req.Diff, "\n")
		shown := lines
		if len(lines) > maxDiffLinesDisplay {
			shown = lines[:maxDiffLinesDisplay]
		}
		for _, line := range shown {
			fmt.Fprintf(w, "    %s\n", line)
		}
		if len(lines) > maxDiffLinesDisplay {
			fmt.Fprintf(w, "\n  ⚠ diff truncated: showing %d of %d lines — %d lines NOT shown below this point.\n",
				maxDiffLinesDisplay, len(lines), len(lines)-maxDiffLinesDisplay)
			fmt.Fprintf(w, "    Approving accepts changes you have not seen.\n")
		}
	}
}

// summariseArg renders an argument value for review: whitespace made visible,
// long values elided with their true size, so a 4 KB replacement reads as one
// line rather than scrolling the diff away.
func summariseArg(v any) string {
	s := fmt.Sprintf("%v", v)
	total := len(s)
	lines := strings.Count(s, "\n") + 1

	if total <= maxArgValueDisplay && lines == 1 {
		return s
	}
	head := s
	if len(head) > maxArgValueDisplay {
		head = head[:maxArgValueDisplay]
	}
	head = strings.ReplaceAll(head, "\n", "\\n")
	return fmt.Sprintf("%s… [%d bytes, %d lines — see diff]", head, total, lines)
}

// readLine reads one line, respecting ctx cancellation.
//
// The read runs inline rather than in a goroutine that outlives the call: a
// leaked reader stays blocked on the same tty and consumes the *next* prompt's
// keystroke. Cancellation closes the file to unblock the read.
func readLine(ctx context.Context, tty *os.File, w io.Writer) (string, bool, error) {
	type result struct {
		line string
		ok   bool
	}
	ch := make(chan result, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		sc := bufio.NewScanner(tty)
		if sc.Scan() {
			ch <- result{line: sc.Text(), ok: true}
			return
		}
		ch <- result{ok: false}
	}()

	select {
	case r := <-ch:
		fmt.Fprintln(w)
		return r.line, r.ok, nil
	case <-ctx.Done():
		fmt.Fprintln(w, "\n  (cancelled)")
		// Unblock the reader so it cannot survive into the next prompt.
		_ = tty.Close()
		<-done
		return "", false, ctx.Err()
	}
}
