package sshtool

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// policyAllowedHosts is the [tools_policies.native-ssh] key enumerating the
// hosts a declaration may reach. Unlike every other native toolset, naming this
// one in an agent file is not by itself consent: the declaration says which
// machine, or the toolset reaches none.
const policyAllowedHosts = "_allowed_hosts"

// hostPattern is one allowlist entry, spelled `[user@]host[:port]`; host may
// carry a single leading "*." to name a domain, and nothing else may be a
// wildcard.
type hostPattern struct {
	raw    string
	user   string
	host   string
	suffix bool
	port   int
}

func (p hostPattern) match(cfg *SSHConfig) bool {
	if p.user != "" && p.user != cfg.User {
		return false
	}
	if p.port != 0 && p.port != cfg.Port {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(cfg.Host))
	if p.suffix {
		return len(host) > len(p.host) && strings.HasSuffix(host, p.host)
	}
	return host == p.host
}

func parseHostPatterns(entries []string) ([]hostPattern, error) {
	out := make([]hostPattern, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		p, err := parseHostPattern(trimmed)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseHostPattern(entry string) (hostPattern, error) {
	p := hostPattern{raw: entry}

	rest := entry
	if at := strings.Index(rest, "@"); at >= 0 {
		p.user = rest[:at]
		rest = rest[at+1:]
		if p.user == "" {
			return p, fmt.Errorf("ssh: host allowlist entry %q has an empty user", entry)
		}
	}

	if host, port, err := net.SplitHostPort(rest); err == nil {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return p, fmt.Errorf("ssh: host allowlist entry %q has an invalid port", entry)
		}
		rest, p.port = host, n
	}

	rest = strings.ToLower(strings.Trim(rest, "[]"))
	if rest == "" {
		return p, fmt.Errorf("ssh: host allowlist entry %q has an empty host", entry)
	}
	if strings.HasPrefix(rest, "*.") {
		p.suffix = true
		rest = rest[1:]
	}
	if strings.Contains(rest, "*") {
		// "*" alone, or a wildcard anywhere but a leading "*.", would restore
		// exactly the reach-any-machine default this allowlist exists to remove.
		return p, fmt.Errorf("ssh: host allowlist entry %q is a wildcard; enumerate hosts, or a domain as \"*.example.com\"", entry)
	}
	p.host = rest
	return p, nil
}

func patternsText(patterns []hostPattern) string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p.raw)
	}
	return strings.Join(out, ", ")
}

func matchAny(patterns []hostPattern, cfg *SSHConfig, source string) error {
	for _, p := range patterns {
		if p.match(cfg) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s@%s:%d is not named by %s, which allows: %s %s",
		ErrHostNotAllowed, cfg.User, cfg.Host, cfg.Port, source, patternsText(patterns), severityRecoverable)
}

// checkHostAllowed enforces both allowlists as an intersection: the operator's
// ceiling from WithAllowedHosts and the declaration's own block. Either alone is
// sufficient consent; neither means no host at all.
func (h *SSHTools) checkHostAllowed(ctx context.Context, cfg *SSHConfig) error {
	if len(h.allowedHosts) > 0 {
		if err := matchAny(h.allowedHosts, cfg, "the runtime's SSH host allowlist"); err != nil {
			return err
		}
	}

	source := "tools_policies." + h.name + "." + policyAllowedHosts
	raw := strings.TrimSpace(taskengine.ToolsArgsFromContext(ctx, h.name)[policyAllowedHosts])
	if raw == "" {
		if len(h.allowedHosts) > 0 {
			return nil
		}
		return fmt.Errorf("%w: this toolset reaches OTHER machines, so naming it in an agent declaration grants nothing on its own; "+
			"set %s to the hosts this agent may reach, spelled `[user@]host[:port]` or `*.example.com` and separated by commas (fatal: no host allowlist)",
			ErrNoAllowedHosts, source)
	}

	patterns, err := parseHostPatterns(strings.Split(raw, ","))
	if err != nil {
		return fatalf("misconfigured host allowlist", "%s: %s", source, err)
	}
	if len(patterns) == 0 {
		return fmt.Errorf("%w: %s is set but names no host (fatal: no host allowlist)", ErrNoAllowedHosts, source)
	}
	return matchAny(patterns, cfg, source)
}
