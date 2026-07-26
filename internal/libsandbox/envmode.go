package libsandbox

// Scrub postures for a shell contenox spawns. They select how much of the parent
// environment survives, and are what an operator names in configuration (e.g.
// SANDBOX_SHELL_SCRUB); EnvPolicyForMode turns the name plus the operator's extra
// allow/deny lists into a concrete EnvPolicy.
const (
	// ScrubOff inherits the full parent environment — the legacy behavior, no
	// scrubbing. EnvPolicyForMode reports it as inactive so the caller leaves the
	// environment untouched.
	ScrubOff = "off"
	// ScrubDenySecrets passes everything EXCEPT the control plane and the common
	// credential shapes (DefaultEnvDeny). Lowest-breakage: a toolchain keeps the
	// environment it expects while known secrets are stripped. This is the sane
	// default for an agent-reachable shell.
	ScrubDenySecrets = "deny-secrets"
	// ScrubStrict passes only the safe base set (DefaultEnvAllow) plus whatever the
	// operator explicitly allows; everything else is absent. Its deny is only the
	// control plane, so — unlike deny-secrets — an operator can hand the shell one
	// trusted credential by naming it in the allow list.
	ScrubStrict = "strict"
)

// EnvPolicyForMode builds the EnvPolicy for a named scrub posture, extended with
// the operator's extra allow/deny entries (names or globs). It returns
// active=false for ScrubOff and for any UNRECOGNIZED mode — a caller that treats
// "unknown" as "off" would fail open, so callers that care about failing closed
// should validate the mode against the constants above BEFORE calling and pick a
// safe default; this function does not guess a posture it was not given.
//
// The two active postures differ only in their starting allow/deny:
//   - strict:       allow = DefaultEnvAllow + extra, deny = ControlPlane + extra.
//   - deny-secrets: allow = "*" + extra,            deny = DefaultEnvDeny + extra.
//
// so strict is an allowlist an operator curates (and can re-permit a secret in),
// while deny-secrets is a denylist that strips known credentials from an
// otherwise-inherited environment.
func EnvPolicyForMode(mode string, extraAllow, extraDeny []string) (policy EnvPolicy, active bool) {
	switch mode {
	case ScrubStrict:
		return EnvPolicy{
			Allow: concat(DefaultEnvAllow(), extraAllow),
			Deny:  concat(ControlPlaneEnvDeny(), extraDeny),
		}, true
	case ScrubDenySecrets:
		return EnvPolicy{
			Allow: concat([]string{"*"}, extraAllow),
			Deny:  concat(DefaultEnvDeny(), extraDeny),
		}, true
	default: // ScrubOff or unrecognized
		return EnvPolicy{}, false
	}
}

// EnvScrub returns the scrub hook for a posture — a function mapping a parent
// environment ("KEY=VALUE" entries) to the confined one — or nil when the posture
// is inactive (ScrubOff / unrecognized), so a caller can wire the result straight
// into an exec site: `if scrub != nil { cmd.Env = scrub(os.Environ()) }`.
func EnvScrub(mode string, extraAllow, extraDeny []string) func(parent []string) []string {
	policy, active := EnvPolicyForMode(mode, extraAllow, extraDeny)
	if !active {
		return nil
	}
	return policy.Apply
}

// ScrubModeValid reports whether mode is one of the recognized postures. Callers
// resolving operator configuration use it to reject a typo and fall back to a
// safe default rather than silently disabling the scrub.
func ScrubModeValid(mode string) bool {
	switch mode {
	case ScrubOff, ScrubDenySecrets, ScrubStrict:
		return true
	default:
		return false
	}
}
