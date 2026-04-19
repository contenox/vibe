package contenoxcli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/llmresolver"
	"github.com/contenox/contenox/internal/setupcheck"
)

// ErrPreflightBlocked is returned when LLM setup is not ready; instructions are already printed to w.
var ErrPreflightBlocked = errors.New("LLM setup is not ready")

// PreflightLLMSetup checks setup status before running chat, run, or plan. If the user must fix
// configuration first, it prints instructions and returns ErrPreflightBlocked. Otherwise it returns nil.
func PreflightLLMSetup(w io.Writer, res setupcheck.Result) error {
	if !llmSetupNeedsAttention(res) {
		return nil
	}
	io.WriteString(w, "\n")
	io.WriteString(w, "Cannot run until LLM setup is ready.\n")
	PrintSetupIssues(w, res)
	return ErrPreflightBlocked
}

func llmSetupNeedsAttention(res setupcheck.Result) bool {
	for _, iss := range res.Issues {
		if iss.Severity == "error" {
			return true
		}
		if iss.Code == "no_backends" {
			// Warning in Evaluate, but chat/run cannot resolve any model without a backend.
			return true
		}
	}
	return false
}

// isModelResolverFailure reports errors where printing setupcheck issues helps the user.
func isModelResolverFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, llmresolver.ErrNoAvailableModels) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "no models found") ||
		strings.Contains(s, "model name cannot be empty") ||
		strings.Contains(s, "client resolution failed")
}

var setupIssuePriority = map[string]int{
	"missing_default_model":            0,
	"missing_default_provider":         1,
	"no_backends":                      2,
	"default_provider_backend_missing": 3,
	"runtime_state_empty":              4,
	"all_backends_unreachable":         5,
	"default_provider_api_key_missing": 6,
	"default_provider_auth_failed":     7,
	"default_provider_not_synced":      8,
	"default_provider_unreachable":     9,
	"no_chat_models":                   10,
	"default_model_not_available":      11,
}

func issueRank(code string) int {
	if n, ok := setupIssuePriority[code]; ok {
		return n
	}
	return 100
}

// PrintSetupIssues prints actionable setup issues after model resolution failures.
// Includes error and warning severities, ordered for readability.
func PrintSetupIssues(w io.Writer, res setupcheck.Result) {
	if len(res.Issues) == 0 {
		return
	}
	issues := append([]setupcheck.Issue(nil), res.Issues...)
	sort.SliceStable(issues, func(i, j int) bool {
		si, sj := issues[i].Severity, issues[j].Severity
		if si != sj {
			return severityRank(si) < severityRank(sj)
		}
		ri, rj := issueRank(issues[i].Code), issueRank(issues[j].Code)
		if ri != rj {
			return ri < rj
		}
		return issues[i].Code < issues[j].Code
	})

	io.WriteString(w, "\n")
	io.WriteString(w, "Setup issues:\n")
	for _, iss := range issues {
		if iss.Severity != "error" && iss.Severity != "warning" {
			continue
		}
		fmt.Fprintf(w, "  • [%s]  %s\n", iss.Severity, iss.Message)
		if iss.CLICommand != "" {
			io.WriteString(w, "    Try: ")
			io.WriteString(w, iss.CLICommand)
			io.WriteString(w, "\n")
		}
	}
}

// PrintBackendChecks prints one line per registered backend plus any runtime error hint.
func PrintBackendChecks(w io.Writer, res setupcheck.Result) {
	if len(res.BackendChecks) == 0 {
		return
	}

	checks := append([]setupcheck.BackendCheck(nil), res.BackendChecks...)
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].DefaultProvider != checks[j].DefaultProvider {
			return checks[i].DefaultProvider
		}
		if checks[i].Reachable != checks[j].Reachable {
			return !checks[i].Reachable
		}
		return strings.ToLower(checks[i].Name) < strings.ToLower(checks[j].Name)
	})

	io.WriteString(w, "\n")
	io.WriteString(w, "Backend diagnostics:\n")
	for _, check := range checks {
		prefix := "  • "
		if check.DefaultProvider {
			prefix = "  • [default] "
		}
		fmt.Fprintf(w, "%s%s (%s, %s)\n", prefix, check.Name, check.Type, check.BaseURL)
		switch check.Status {
		case "ready":
			fmt.Fprintf(w, "    Status: reachable; %d chat model(s), %d total model(s)\n", check.ChatModelCount, check.ModelCount)
			if len(check.ChatModels) > 0 {
				fmt.Fprintf(w, "    Chat models: %s\n", strings.Join(check.ChatModels, ", "))
			}
		case "pending":
			io.WriteString(w, "    Status: no runtime state yet\n")
		default:
			io.WriteString(w, "    Status: error\n")
		}
		if check.Error != "" {
			fmt.Fprintf(w, "    Error: %s\n", check.Error)
		}
		if check.Hint != "" {
			fmt.Fprintf(w, "    Hint: %s\n", check.Hint)
		}
	}
}

func severityRank(s string) int {
	switch s {
	case "error":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}
