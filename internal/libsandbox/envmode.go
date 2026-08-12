package libsandbox

// Scrub postures for a shell contenox spawns, named in operator configuration.
const (
	// ScrubOff inherits the full parent environment unscrubbed.
	ScrubOff = "off"
	// ScrubDenySecrets passes everything except the control plane and common
	// credential shapes (DefaultEnvDeny).
	ScrubDenySecrets = "deny-secrets"
	// ScrubStrict passes only DefaultEnvAllow plus operator additions.
	ScrubStrict = "strict"
)

// EnvPolicyForMode builds the EnvPolicy for a named scrub posture extended with extra allow/deny entries, returning active=false for ScrubOff and any unrecognized mode.
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
	default:
		return EnvPolicy{}, false
	}
}

// EnvScrub returns the scrub function for a posture, or nil when inactive (ScrubOff or an unrecognized mode).
func EnvScrub(mode string, extraAllow, extraDeny []string) func(parent []string) []string {
	policy, active := EnvPolicyForMode(mode, extraAllow, extraDeny)
	if !active {
		return nil
	}
	return policy.Apply
}

// ScrubModeValid reports whether mode is a recognized scrub posture.
func ScrubModeValid(mode string) bool {
	switch mode {
	case ScrubOff, ScrubDenySecrets, ScrubStrict:
		return true
	default:
		return false
	}
}
