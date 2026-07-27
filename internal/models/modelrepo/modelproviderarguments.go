package modelrepo

type chatArgument struct {
	applyFunc func(*ChatConfig)
}

func (c *chatArgument) Apply(config *ChatConfig) {
	c.applyFunc(config)
}

func WithTemperature(temp float64) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.Temperature = &temp
		},
	}
}

func WithMaxTokens(tokens int) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.MaxTokens = &tokens
		},
	}
}

func WithTopP(p float64) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.TopP = &p
		},
	}
}

func WithSeed(seed int) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.Seed = &seed
		},
	}
}

func WithTool(tool Tool) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.Tools = append(config.Tools, tool)
		},
	}
}

func WithTools(tools ...Tool) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			config.Tools = append(config.Tools, tools...)
		},
	}
}

// WithCacheHints declares the request's stable/volatile boundary for
// provider-side prompt caching (see CacheHints). The hints are copied so the
// caller's value cannot be mutated through the config afterwards.
func WithCacheHints(hints CacheHints) ChatArgument {
	return &chatArgument{
		applyFunc: func(config *ChatConfig) {
			h := hints
			config.CacheHints = &h
		},
	}
}
