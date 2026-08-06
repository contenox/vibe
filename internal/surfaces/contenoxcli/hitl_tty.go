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

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/surfaces/beamtui/sanitize"
)

// ErrApprovalAborted is returned when the operator aborts the whole run from an
// approval prompt. Callers should treat it as a user-initiated cancellation,
// not a tool failure.
var ErrApprovalAborted = errors.New("run aborted at approval prompt")

const (
	// maxArgValueDisplay bounds how much of a single argument value is echoed
	// above the prompt, so a long argument does not push the diff off screen.
	maxArgValueDisplay = 240

	// maxDiffLinesDisplay bounds the rendered diff; the truncation notice
	// appears directly above the prompt so it cannot be missed.
	maxDiffLinesDisplay = 120

	// maxArgBlockLines bounds a multi-line argument block, matching
	// comp/approval's cap so both surfaces show the same amount.
	maxArgBlockLines = 40

	// argBlockTabStop is the tab width used in argument/diff blocks, matching
	// standard diff tools so code stays aligned as written.
	argBlockTabStop = 8

	// inputFlushWindow is how long to spend draining stray keystrokes typed
	// before the prompt was drawn.
	inputFlushWindow = 15 * time.Millisecond
)

// CLIApprovalOptions configures the interactive approval prompt.
//
// Deliberately has no "always allow for the rest of the session" option: the
// only legitimate way to stop being asked about a tool is an allow rule in
// the policy document, not a grant cached in one client.
type CLIApprovalOptions struct {
	// AuditLog, when non-nil, receives one line per decision, so approvals
	// remain recoverable after the fact.
	AuditLog func(req hitlservice.ApprovalRequest, decision string)
}

// NewCLIAskApproval returns an AskApproval callback suitable for interactive
// CLI use. It opens /dev/tty directly so it works even when stdin is piped,
// refuses to read approvals from a non-terminal stdin, drains buffered input
// before each prompt, and offers an explicit abort alongside approve/deny.
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
		// TODO: race workaround, not a fix — the engine should signal instead.
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

		// Drop anything typed before the prompt was drawn, or "yy" at one
		// prompt would approve the next before the operator has seen it.
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
	// Why THIS call stopped, when the args alone do not say — which command of
	// a compound line tripped a rule, or which binary was not trusted. Without
	// it a withdrawn allow looks like an unexplained card.
	if detail := strings.TrimSpace(req.Detail); detail != "" {
		fmt.Fprintf(w, "  Reason: %s\n", sanitize.Lines(detail))
	}

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
			writeArg(w, k, req.Args[k], req.Diff != "")
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
			// Sanitized so an escape sequence in the repo content can't erase
			// the prompt, but never wrapped or elided.
			fmt.Fprintf(w, "    %s\n", sanitize.ExpandTabs(sanitize.Lines(line), argBlockTabStop))
		}
		if len(lines) > maxDiffLinesDisplay {
			fmt.Fprintf(w, "\n  ⚠ diff truncated: showing %d of %d lines — %d lines NOT shown below this point.\n",
				maxDiffLinesDisplay, len(lines), len(lines)-maxDiffLinesDisplay)
			fmt.Fprintf(w, "    Approving accepts changes you have not seen.\n")
		}
	}
}

// writeArg prints one argument for review.
//
// A value containing newlines (a script body, heredoc, patch) is printed as
// an indented block instead of a truncated one-liner, since that is the
// argument most in need of full reading. hasDiff suppresses the block when
// the diff below already renders the same bytes.
func writeArg(w io.Writer, key string, v any, hasDiff bool) {
	s, isString := v.(string)
	if !isString || hasDiff || !strings.Contains(s, "\n") {
		fmt.Fprintf(w, "    %s = %s\n", key, summariseArg(v))
		return
	}

	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	shown := lines
	if len(shown) > maxArgBlockLines {
		shown = shown[:maxArgBlockLines]
	}

	fmt.Fprintf(w, "    %s =\n", sanitize.Line(key))
	for _, l := range shown {
		fmt.Fprintf(w, "      %s\n", sanitize.ExpandTabs(sanitize.Lines(l), argBlockTabStop))
	}
	if hidden := len(lines) - len(shown); hidden > 0 {
		fmt.Fprintf(w, "      ⚠ +%d more lines — approving accepts content you have not seen\n", hidden)
	}
}

// summariseArg renders an argument value for review: whitespace made visible,
// long values elided with their true size so they read as one line rather
// than scrolling the diff away. The result is sanitized, since an escape
// sequence in the argument could otherwise erase the prompt beneath it.
func summariseArg(v any) string {
	s := fmt.Sprintf("%v", v)
	total := len(s)
	lines := strings.Count(s, "\n") + 1

	if total <= maxArgValueDisplay && lines == 1 {
		return sanitize.Line(s)
	}
	head := s
	if r := []rune(head); len(r) > maxArgValueDisplay {
		// Cut on a rune boundary: a byte cut can split a rune and put a
		// replacement character in the middle of what is being reviewed.
		head = string(r[:maxArgValueDisplay])
	}
	head = sanitize.Line(strings.ReplaceAll(head, "\n", "\\n"))
	return fmt.Sprintf("%s… [%d bytes, %d lines — see diff]", head, total, lines)
}

// readLine reads one line, respecting ctx cancellation. Cancellation closes
// the file to unblock the read rather than leaving a goroutine that would
// consume the next prompt's keystroke.
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
