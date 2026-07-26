package modelrepo

// ClampMaxOutputTokens returns the effective output-token request after applying
// a provider ceiling. A ceiling of 0 means unknown, so no clamp is applied.
func ClampMaxOutputTokens(requested, ceiling int) (int, bool) {
	if requested > 0 && ceiling > 0 && requested > ceiling {
		return ceiling, true
	}
	return requested, false
}

// ClampMaxOutputTokensPtr copies tokens and applies ClampMaxOutputTokens.
// Returning a fresh pointer avoids mutating ChatConfig values captured by args.
func ClampMaxOutputTokensPtr(tokens *int, ceiling int) *int {
	if tokens == nil {
		return nil
	}
	v, _ := ClampMaxOutputTokens(*tokens, ceiling)
	return &v
}
