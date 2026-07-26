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
