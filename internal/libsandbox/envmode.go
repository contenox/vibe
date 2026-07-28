package libsandbox

// Scrub postures for a shell contenox spawns, named in operator configuration
// (e.g. SANDBOX_SHELL_SCRUB); EnvPolicyForMode turns a name plus extra
// allow/deny lists into a concrete EnvPolicy.
const (
	// ScrubOff inherits the full parent environment unscrubbed.
	ScrubOff = "off"
	// ScrubDenySecrets passes everything except the control plane and common
	// credential shapes (DefaultEnvDeny) — lowest breakage, the sane default.
	ScrubDenySecrets = "deny-secrets"
	// ScrubStrict passes only DefaultEnvAllow plus operator additions; deny
	// is just the control plane, so unlike deny-secrets an operator can
	// re-permit one trusted credential via the allow list.
	ScrubStrict = "strict"
)

// EnvPolicyForMode builds the EnvPolicy for a named scrub posture, extended
// with extra allow/deny entries. Returns active=false for ScrubOff and any
// unrecognized mode — it does not guess a posture; callers that must fail
// closed should validate mode against the constants above first.
//
//   - strict:       allow = DefaultEnvAllow + extra, deny = ControlPlane + extra.
//   - deny-secrets: allow = "*" + extra,            deny = DefaultEnvDeny + extra.
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

// EnvScrub returns the scrub function for a posture, or nil when inactive
// (ScrubOff / unrecognized): `if scrub != nil { cmd.Env = scrub(os.Environ()) }`.
func EnvScrub(mode string, extraAllow, extraDeny []string) func(parent []string) []string {
	policy, active := EnvPolicyForMode(mode, extraAllow, extraDeny)
	if !active {
		return nil
	}
	return policy.Apply
}

// ScrubModeValid reports whether mode is a recognized posture, so callers can
// reject a typo and fall back to a safe default instead of silently
// disabling the scrub.
func ScrubModeValid(mode string) bool {
	switch mode {
	case ScrubOff, ScrubDenySecrets, ScrubStrict:
		return true
	default:
		return false
	}
}
