package missiontools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/getkin/kin-openapi/openapi3"
)

type missionToolSchema struct {
	tool      taskengine.Tool
	component string
	response  func() *openapi3.SchemaRef
}

func missionToolSchemas() []missionToolSchema {
	return []missionToolSchema{
		{tool: reportToolSchema(), component: "MissionReport", response: reportResponseSchema},
		{tool: askAttentionToolSchema(), component: "MissionAskAttention", response: askAttentionResponseSchema},
		{tool: planToolSchema(), component: "MissionPlan", response: planResponseSchema},
		{tool: finishToolSchema(), component: "MissionFinish", response: finishResponseSchema},
		{tool: listMissionsToolSchema(), component: "MissionList", response: listMissionsResponseSchema},
		{tool: answerToolSchema(), component: "MissionAnswer", response: answerResponseSchema},
	}
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// one request/response pair per declared tool, plus the shape of the document
// mission_list returns as text.
func (p *provider) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	declared := missionToolSchemas()
	schemas := make(map[string]*openapi3.SchemaRef, 2*len(declared)+1)
	for _, d := range declared {
		req, err := schemaFromParameters(d.tool.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("missiontools: publish schema for %s: %w", d.tool.Function.Name, err)
		}
		schemas[d.component+"Request"] = req
		schemas[d.component+"Response"] = d.response()
	}
	schemas["MissionListPayload"] = listMissionsPayloadSchema()

	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Mission Tools",
			Description: "The per-mission back-channel a unit holds while running unattended — report progress, ask an operator, maintain a living plan, finish with a verdict — plus the supervisor half a session that fired missions holds: see what you dispatched, answer what it asks. The mission is never an argument: it rides the request context, so a call can only ever address the caller's own mission.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: schemas,
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

func schemaFromParameters(params any) (*openapi3.SchemaRef, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode parameters: %w", err)
	}
	var s openapi3.Schema
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("parameters are not a JSON Schema object: %w", err)
	}
	return &openapi3.SchemaRef{Value: &s}, nil
}

func textResponse(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeString},
		Description: description,
	}}
}

func reportResponseSchema() *openapi3.SchemaRef {
	return textResponse("One line naming what was filed, e.g. `recorded progress report \"rep-1\"`. A `result` whose claimed artifacts include a path that is positively missing is filed as `progress` instead, and the line then also carries `(downgraded from result: …)` naming the paths — the durable row and this reply say the same thing.")
}

func askAttentionResponseSchema() *openapi3.SchemaRef {
	return textResponse("The operator's answer, verbatim: the call blocks until a human replies and their words ARE this result, so the unit continues with them on the same turn. When no answer channel is wired or nobody answered, the question is filed as a durable blocker instead and the result is `attention requested (recorded as blocker — no operator answered)`. When the park window elapses with the question still open the call does not return a result at all — the run suspends and resumes with the answer.")
}

func finishResponseSchema() *openapi3.SchemaRef {
	return textResponse("`mission finished as <status>`, naming the terminal state the mission came to rest in. Finishing is immutable: a second call with a different status is an error, not a correction.")
}

func answerResponseSchema() *openapi3.SchemaRef {
	return textResponse("`answered <askId> — unit \"<agentName>\" has your reply and continues`. An askId that is not one of your own missions' open questions (already answered, expired, or never yours) is an error rather than a silent no-op.")
}

func listMissionsResponseSchema() *openapi3.SchemaRef {
	return textResponse("The MissionListPayload document, serialized as a JSON string: `{\"missions\":[…]}`, newest first, holding each mission's status, latest reports, and any question waiting on you.")
}

func planEntrySchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"id": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The entry's stable id, assigned by the runtime when the entry was introduced. Echo it on the next revision to carry the entry forward.",
			}},
			"content": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The step, as the planner wrote it.",
			}},
			"status": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Enum:        []any{string(missionservice.PlanEntryPending), string(missionservice.PlanEntryInProgress), string(missionservice.PlanEntryCompleted)},
				Description: "pending, in_progress, or completed. A completed entry is immutable.",
			}},
			"priority": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Enum:        []any{string(missionservice.PlanEntryPriorityHigh), string(missionservice.PlanEntryPriorityMedium), string(missionservice.PlanEntryPriorityLow)},
				Description: "high, medium, or low.",
			}},
		},
		Required: []string{"id", "content", "status", "priority"},
	}}
}

func planResponseSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"entries": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Items:       planEntrySchema(),
				Description: "The stored plan, in order, with the ids the runtime assigned — use these ids on the next revision.",
			}},
			"revision": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeInteger},
				Description: "How many successful revisions this plan has had; 1 after the first accepted call.",
			}},
			"explanation": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "The rationale given for the latest revision. Absent when none was given.",
			}},
		},
		Required: []string{"entries", "revision"},
	}}
}

func pendingAskSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"askId":     {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The handle to answer with — pass it to " + ToolNameAnswer + "."}},
			"missionId": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The mission whose unit is waiting."}},
			"question":  {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The unit's one-line question."}},
			"detail":    {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The longer detail the unit gave, when it gave any."}},
			"askedAt":   {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "When the question was raised."}},
		},
		Required: []string{"askId", "missionId", "question"},
	}}
}

func listMissionsPayloadSchema() *openapi3.SchemaRef {
	entry := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"missionId":    {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The mission's id."}},
			"agentName":    {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The agent the mission was dispatched as."}},
			"intent":       {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "What the mission was fired to do."}},
			"status":       {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The mission's current status."}},
			"statusReason": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Why it holds that status, when a reason was recorded."}},
			"lastHeartbeat": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeString},
				Description: "RFC3339 UTC timestamp of the unit's last proof of life. Absent on a mission that has not stirred yet.",
			}},
			"reports": {Value: &openapi3.Schema{
				Type: &openapi3.Types{openapi3.TypeArray},
				Items: &openapi3.SchemaRef{Value: &openapi3.Schema{
					Type: &openapi3.Types{openapi3.TypeObject},
					Properties: map[string]*openapi3.SchemaRef{
						"kind":    {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "progress, finding, blocker, or result."}},
						"summary": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "The report's one-line summary."}},
					},
					Required: []string{"kind", "summary"},
				}},
				Description: "The mission's latest reports, newest first. Absent when it has filed none.",
			}},
			"waitingOnYou": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Items:       pendingAskSchema(),
				Description: "The questions this mission's unit is blocked on right now. Absent when it is waiting on nothing.",
			}},
		},
		Required: []string{"missionId", "agentName", "intent", "status"},
	}}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type: &openapi3.Types{openapi3.TypeObject},
		Properties: map[string]*openapi3.SchemaRef{
			"missions": {Value: &openapi3.Schema{
				Type:        &openapi3.Types{openapi3.TypeArray},
				Items:       entry,
				Description: "The missions this session fired, newest first. Empty when it fired none.",
			}},
		},
		Required: []string{"missions"},
	}}
}
