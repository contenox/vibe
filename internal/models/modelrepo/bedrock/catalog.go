package bedrock

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
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
	// Catalog listing is a non-streaming call: bound it end-to-end.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()

	// Same credential rules as inference: stored static-creds JSON when
	// present, ambient AWS chain otherwise.
	cfg, err := loadAWSConfig(ctx, regionFromURL(p.spec.BaseURL), p.spec.APIKey, p.httpClient)
	if err != nil {
		return nil, err
	}

	client := bedrock.NewFromConfig(cfg)

	output, err := client.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to list foundation models: %w", err)
	}

	// Newer models (Claude 3.5v2+) are invocable only through a cross-region
	// inference profile; the base model id returns a 400. Profile listing is
	// best-effort: without bedrock:ListInferenceProfiles permission the
	// profile-only models are omitted rather than listed uninvocable.
	profileIDs, _ := listInferenceProfileIDs(ctx, client)

	var models []modelrepo.ObservedModel
	for _, summary := range output.ModelSummaries {
		invokeID, ok := resolveInvocableModelID(aws.ToString(summary.ModelId), summary.InferenceTypesSupported, profileIDs)
		if !ok {
			continue
		}
		models = append(models, observedFromSummary(summary, invokeID))
	}

	return models, nil
}

// listInferenceProfileIDs returns every system-defined inference-profile id in
// the region (e.g. us.anthropic.claude-sonnet-4-5-20250929-v1:0), following
// nextToken pagination.
func listInferenceProfileIDs(ctx context.Context, client *bedrock.Client) ([]string, error) {
	var ids []string
	var nextToken *string
	for {
		out, err := client.ListInferenceProfiles(ctx, &bedrock.ListInferenceProfilesInput{
			NextToken:  nextToken,
			TypeEquals: types.InferenceProfileTypeSystemDefined,
		})
		if err != nil {
			return nil, err
		}
		for _, s := range out.InferenceProfileSummaries {
			if id := aws.ToString(s.InferenceProfileId); id != "" {
				ids = append(ids, id)
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return ids, nil
		}
		nextToken = out.NextToken
	}
}

// resolveInvocableModelID returns the id to invoke a model with. A model with
// ON_DEMAND inference is invocable by its base id; a profile-only model
// resolves to the system-defined profile named "<geo prefix>.<base id>"
// (us./eu./apac./jp./global.), preferring geographic profiles over global.
// ok=false means the model cannot be invoked from this account/region.
func resolveInvocableModelID(modelID string, inferenceTypes []types.InferenceType, profileIDs []string) (string, bool) {
	for _, t := range inferenceTypes {
		if t == types.InferenceTypeOnDemand {
			return modelID, true
		}
	}
	suffix := "." + modelID
	var matches []string
	for _, pid := range profileIDs {
		if strings.HasSuffix(pid, suffix) {
			matches = append(matches, pid)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	// Geographic profiles sort before "global." lexically only by accident;
	// order explicitly: non-global first, then stable order.
	sort.SliceStable(matches, func(i, j int) bool {
		gi := strings.HasPrefix(matches[i], "global.")
		gj := strings.HasPrefix(matches[j], "global.")
		return !gi && gj
	})
	return matches[0], true
}

// observedFromSummary maps a ListFoundationModels entry into an ObservedModel
// named by its invocable id. Pure function, no AWS calls, so it is unit
// tested without credentials.
//
// CanVision comes from InputModalities reporting ModelModalityImage, not a
// hardcoded model list. CanEmbed is always false: this provider speaks only
// the Converse API and GetEmbedConnection refuses, so advertising embeddings
// would lie to the request router.
func observedFromSummary(summary types.FoundationModelSummary, invokeID string) modelrepo.ObservedModel {
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
		Name: invokeID,
		CapabilityConfig: modelrepo.CapabilityConfig{
			CanChat:   !isEmbed,
			CanStream: !isEmbed,
			CanPrompt: !isEmbed,
			CanVision: canVision,
			CanThink:  bedrockModelSupportsReasoning(modelID),
		},
	}
}

// bedrockModelSupportsReasoning reports whether a Bedrock model supports the
// Claude extended-thinking reasoning config (Claude 3.7 and the Claude 4+ /
// Fable / Mythos generations). The list API reports no reasoning capability, so
// this is name-based like the vision allowlists in modelrepo.
func bedrockModelSupportsReasoning(modelID string) bool {
	base := strings.ToLower(bedrockBaseModelID(modelID))
	if !strings.Contains(base, "anthropic.") {
		return false
	}
	for _, marker := range []string{
		"claude-3-7", "claude-sonnet-4", "claude-opus-4", "claude-haiku-4",
		"claude-fable", "claude-sonnet-5", "mythos",
	} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return false
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
