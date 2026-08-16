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
	"github.com/contenox/contenox/internal/surfaces/contenoxcli/sanitize"
)

// ErrApprovalAborted is returned when the operator aborts the whole run from an
// approval prompt.
var ErrApprovalAborted = errors.New("run aborted at approval prompt")

const (
	maxArgValueDisplay = 240

	maxDiffLinesDisplay = 120

	maxArgBlockLines = 40

	argBlockTabStop = 8

	inputFlushWindow = 15 * time.Millisecond
)

// CLIApprovalOptions configures the interactive approval prompt. There is no
// "always allow for the session" option: the only way to stop being asked is an
// allow rule in the policy document.
type CLIApprovalOptions struct {
	// AuditLog, when non-nil, receives one line per decision.
	AuditLog func(req hitlservice.ApprovalRequest, decision string)
}

// NewCLIAskApproval returns an AskApproval callback for interactive CLI use. It
// opens /dev/tty directly so it works when stdin is piped, and refuses to read
// approvals from a non-terminal stdin.
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
			// A "y" sitting in piped stdin is not consent.
			fmt.Fprintf(w, "\n  [denied: %s requires approval but no terminal is attached]\n", key)
			if opts.AuditLog != nil {
				opts.AuditLog(req, "denied-no-tty")
			}
			return false, nil
		}
		defer tty.Close()

		// Otherwise "yy" at one prompt would approve the next unseen.
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

// renderApprovalRequest prints the request with the diff last, directly above
// the prompt.
func renderApprovalRequest(w io.Writer, req hitlservice.ApprovalRequest) {
	fmt.Fprintln(w, "\n────────────────────────────────────────────────────")
	fmt.Fprintf(w, "  HITL approval required\n")
	fmt.Fprintf(w, "  Tools : %s\n", req.ToolsName)
	fmt.Fprintf(w, "  Tool  : %s\n", req.ToolName)
	if detail := strings.TrimSpace(req.Detail); detail != "" {
		fmt.Fprintf(w, "  Reason: %s\n", sanitize.Lines(detail))
	}

	if len(req.Args) > 0 {
		fmt.Fprintln(w, "  Args  :")
		// Map iteration order is randomised; a gate must render stably.
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
			// Sanitized, but never wrapped or elided.
			fmt.Fprintf(w, "    %s\n", sanitize.ExpandTabs(sanitize.Lines(line), argBlockTabStop))
		}
		if len(lines) > maxDiffLinesDisplay {
			fmt.Fprintf(w, "\n  ⚠ diff truncated: showing %d of %d lines — %d lines NOT shown below this point.\n",
				maxDiffLinesDisplay, len(lines), len(lines)-maxDiffLinesDisplay)
			fmt.Fprintf(w, "    Approving accepts changes you have not seen.\n")
		}
	}
}

// writeArg prints one argument for review. A value containing newlines is
// printed as an indented block; hasDiff suppresses that when the diff below
// already renders the same bytes.
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
// long values elided with their true size, and the result sanitized.
func summariseArg(v any) string {
	s := fmt.Sprintf("%v", v)
	total := len(s)
	lines := strings.Count(s, "\n") + 1

	if total <= maxArgValueDisplay && lines == 1 {
		return sanitize.Line(s)
	}
	head := s
	if r := []rune(head); len(r) > maxArgValueDisplay {
		// Cut on a rune boundary.
		head = string(r[:maxArgValueDisplay])
	}
	head = sanitize.Line(strings.ReplaceAll(head, "\n", "\\n"))
	return fmt.Sprintf("%s… [%d bytes, %d lines — see diff]", head, total, lines)
}

// readLine reads one line, respecting ctx cancellation by closing the file to
// unblock the read.
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
