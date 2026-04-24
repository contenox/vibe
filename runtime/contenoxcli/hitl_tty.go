package contenoxcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/contenox/contenox/runtime/hitlservice"
	"github.com/contenox/contenox/runtime/localtools"
)

// NewCLIAskApproval returns an AskApproval callback suitable for interactive
// CLI use. It opens /dev/tty directly so it works even when stdin is piped
// (e.g. --auto runs or shell-piped input). Falls back to os.Stdin if /dev/tty
// is unavailable.
//
// The callback prints the tool name, args, and diff (if any) to w (stderr),
// prompts "Approve? [y/N]:", and blocks until the user responds or ctx is
// cancelled. Only "y" or "yes" (case-insensitive) approves; everything else
// (including blank input and EOF) denies.
func NewCLIAskApproval(w io.Writer) localtools.AskApproval {
	return func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		// Try to open the controlling terminal directly so we can prompt even
		// when stdin is a pipe.
		tty, err := os.Open("/dev/tty")
		if err != nil {
			tty = os.Stdin
		} else {
			defer tty.Close()
		}

		fmt.Fprintln(w, "\n────────────────────────────────────────────────────")
		fmt.Fprintf(w, "  HITL approval required\n")
		fmt.Fprintf(w, "  Tools : %s\n", req.ToolsName)
		fmt.Fprintf(w, "  Tool : %s\n", req.ToolName)
		if len(req.Args) > 0 {
			fmt.Fprintln(w, "  Args :")
			for k, v := range req.Args {
				fmt.Fprintf(w, "    %s = %v\n", k, v)
			}
		}
		if req.Diff != "" {
			fmt.Fprintln(w, "  Diff :")
			for _, line := range strings.Split(req.Diff, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
		fmt.Fprintln(w, "────────────────────────────────────────────────────")
		fmt.Fprint(w, "  Approve? [y/N]: ")

		// Read the response in a goroutine so we can respect ctx cancellation.
		type result struct {
			line string
			ok   bool
		}
		ch := make(chan result, 1)
		go func() {
			scanner := bufio.NewScanner(tty)
			if scanner.Scan() {
				ch <- result{line: scanner.Text(), ok: true}
			} else {
				ch <- result{ok: false} // EOF or error
			}
		}()

		select {
		case r := <-ch:
			fmt.Fprintln(w) // newline after inline prompt
			if !r.ok {
				return false, nil
			}
			trimmed := strings.TrimSpace(strings.ToLower(r.line))
			return trimmed == "y" || trimmed == "yes", nil
		case <-ctx.Done():
			fmt.Fprintln(w, "\n  (cancelled)")
			return false, ctx.Err()
		}
	}
}
