package scriptedtest

import (
	"context"
	"strconv"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type catalogProvider struct {
	scriptPath string
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider(modelrepo.ScriptedTestBackendType, func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		path, err := ResolvePath(spec.BaseURL)
		if err != nil {
			return nil, err
		}
		tracker := opts.Tracker
		if tracker == nil {
			tracker = libtracker.NoopTracker{}
		}
		return &catalogProvider{scriptPath: path, tracker: tracker}, nil
	})
}

func (p *catalogProvider) Type() string { return modelrepo.ScriptedTestBackendType }

func (p *catalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	script, err := Load(p.scriptPath)
	if err != nil {
		return nil, err
	}
	return []modelrepo.ObservedModel{{
		Name:             script.Model,
		ContextLength:    script.ContextLength,
		CapabilityConfig: capabilitiesOf(script),
		Meta: map[string]string{
			"script": script.Path,
			"turns":  strconv.Itoa(len(script.Turns)),
		},
	}}, nil
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	dimensions := defaultEmbedDimensions
	if script, err := Load(p.scriptPath); err == nil {
		dimensions = script.EmbedDimensions
	}
	return NewProvider(model.Name, p.scriptPath, model.CapabilityConfig, dimensions, p.tracker)
}

// capabilitiesOf claims every capability by default: a scripted reply is never refused for lack of one, and the type name already says it is not a model.
func capabilitiesOf(script *Script) modelrepo.CapabilityConfig {
	return modelrepo.CapabilityConfig{
		ContextLength:   script.ContextLength,
		MaxOutputTokens: script.MaxOutputTokens,
		CanChat:         flag(script.Capabilities.Chat, true),
		CanEmbed:        flag(script.Capabilities.Embed, true),
		CanStream:       flag(script.Capabilities.Stream, true),
		CanPrompt:       flag(script.Capabilities.Prompt, true),
		CanThink:        flag(script.Capabilities.Think, true),
		CanVision:       flag(script.Capabilities.Vision, true),
		CanAudio:        flag(script.Capabilities.Audio, true),
	}
}
