package runtimestate

import (
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/statetype"
)

const observedDisplayNameMetaKey = "display_name"

func observedModelFromPullStatus(model statetype.ModelPullStatus) modelrepo.ObservedModel {
	name := strings.TrimSpace(model.Model)
	if name == "" {
		name = strings.TrimSpace(model.Name)
	}

	meta := map[string]string{}
	if display := strings.TrimSpace(model.Name); display != "" && display != name {
		meta[observedDisplayNameMetaKey] = display
	}
	if len(meta) == 0 {
		meta = nil
	}

	return modelrepo.ObservedModel{
		Name:          name,
		ContextLength: model.ContextLength,
		ModifiedAt:    model.ModifiedAt,
		Size:          model.Size,
		Digest:        model.Digest,
		CapabilityConfig: modelrepo.CapabilityConfig{
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxOutputTokens,
			CanChat:         model.CanChat,
			CanEmbed:        model.CanEmbed,
			CanPrompt:       model.CanPrompt,
			CanStream:       model.CanStream,
			CanThink:        model.CanThink,
			CanVision:       model.CanVision,
		},
		Meta: meta,
	}
}

func pullStatusFromObservedModel(model modelrepo.ObservedModel) statetype.ModelPullStatus {
	displayName := model.Name
	if model.Meta != nil {
		if display := strings.TrimSpace(model.Meta[observedDisplayNameMetaKey]); display != "" {
			displayName = display
		}
	}

	return statetype.ModelPullStatus{
		Name:            displayName,
		Model:           model.Name,
		ModifiedAt:      model.ModifiedAt,
		Size:            model.Size,
		Digest:          model.Digest,
		ContextLength:   model.ContextLength,
		MaxOutputTokens: model.MaxOutputTokens,
		CanChat:         model.CanChat,
		CanEmbed:        model.CanEmbed,
		CanPrompt:       model.CanPrompt,
		CanStream:       model.CanStream,
		CanThink:        model.CanThink,
		CanVision:       model.CanVision,
	}
}
