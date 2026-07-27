package runtimestate

import (
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/statetype"
	"github.com/contenox/beam/internal/store/runtimetypes"
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

// mergeDeclaredOverObserved builds the runtime entry for a declared model from
// the observed catalog entry, overlaying admin intent: identity fields come
// from the declaration, a declared context length > 0 wins over the observed
// one, and declared capability trues merge in additively. Capabilities the
// declared row cannot express (CanVision, CanThink) always survive from
// observation; a declared false never suppresses an observed true — manual
// suppression goes through capability overrides instead.
func mergeDeclaredOverObserved(declared *runtimetypes.Model, observed modelrepo.ObservedModel) statetype.ModelPullStatus {
	lmr := pullStatusFromObservedModel(observed)
	lmr.Name = declared.ID
	lmr.Model = declared.Model
	lmr.ModifiedAt = declared.UpdatedAt
	if declared.ContextLength > 0 {
		lmr.ContextLength = declared.ContextLength
	}
	if declared.CanChat {
		lmr.CanChat = true
	}
	if declared.CanEmbed {
		lmr.CanEmbed = true
	}
	if declared.CanPrompt {
		lmr.CanPrompt = true
	}
	if declared.CanStream {
		lmr.CanStream = true
	}
	return lmr
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
