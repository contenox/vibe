package bedrock

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type catalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider("bedrock", func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		return &catalogProvider{spec: spec, httpClient: opts.HTTPClient, tracker: opts.Tracker}, nil
	})
}

func (p *catalogProvider) Type() string { return "bedrock" }

func (p *catalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(regionFromURL(p.spec.BaseURL)))
	if err != nil {
		return nil, err
	}

	client := bedrock.NewFromConfig(cfg)

	output, err := client.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to list foundation models: %w", err)
	}

	var models []modelrepo.ObservedModel
	for _, summary := range output.ModelSummaries {
		models = append(models, observedFromSummary(summary))
	}

	return models, nil
}

// observedFromSummary maps a Bedrock ListFoundationModels result entry into an
// ObservedModel. It is a pure function (no AWS calls) so the capability mapping
// can be unit-tested without live credentials.
//
// CanVision is detected from the API rather than a hardcoded model list: the
// FoundationModelSummary.InputModalities field reports which input modalities
// the model accepts, and a model that lists ModelModalityImage ("IMAGE") among
// them accepts image input.
func observedFromSummary(summary types.FoundationModelSummary) modelrepo.ObservedModel {
	modelID := aws.ToString(summary.ModelId)
	isEmbed := strings.Contains(strings.ToLower(modelID), "embed")

	canVision := false
	for _, m := range summary.InputModalities {
		if strings.EqualFold(string(m), string(types.ModelModalityImage)) {
			canVision = true
			break
		}
	}

	return modelrepo.ObservedModel{
		Name: modelID,
		CapabilityConfig: modelrepo.CapabilityConfig{
			CanChat:   !isEmbed,
			CanStream: !isEmbed,
			CanPrompt: !isEmbed,
			CanEmbed:  isEmbed,
			CanVision: canVision,
		},
	}
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewBedrockProvider(
		regionFromURL(p.spec.BaseURL),
		p.spec.APIKey,
		model.Name,
		model.CapabilityConfig,
		p.httpClient,
		p.tracker,
	)
}
