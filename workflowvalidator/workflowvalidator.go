package workflowvalidator

import (
	"fmt"

	"github.com/contenox/vibe/taskengine"
)

// Validator validates workflows for specific use cases
type Validator struct{}

// New creates a new workflow validator
func New() *Validator {
	return &Validator{}
}

// ValidationProfile defines what a workflow should guarantee
type ValidationProfile struct {
	// RequiredReturnType specifies the data type that must be returned
	RequiredReturnType string
	// AllowConvertibleTypes specifies if types that can be converted are acceptable
	AllowConvertibleTypes bool
	// RequiredInputType specifies the expected input type (optional)
	RequiredInputType string
}

// Standard validation profiles
var (
	// ChatServiceProfile validates workflows for the chat service
	ChatServiceProfile = ValidationProfile{
		RequiredReturnType:    "chat_history",
		AllowConvertibleTypes: false,
		RequiredInputType:     "chat_history",
	}

	// OpenAIChatServiceProfile validates workflows for OpenAI-compatible service
	OpenAIChatServiceProfile = ValidationProfile{
		RequiredReturnType:    "openai_chat_response",
		AllowConvertibleTypes: true, // Allow chat_history since it can be converted
		RequiredInputType:     "openai_chat",
	}
)

// ValidateWorkflow ensures a workflow is compatible with the given profile
func (v *Validator) ValidateWorkflow(chain *taskengine.TaskChainDefinition, profile ValidationProfile) error {
	if chain == nil {
		return fmt.Errorf("workflow cannot be nil")
	}

	// Check input type if specified
	if profile.RequiredInputType != "" {
		if err := v.validateInputType(chain, profile.RequiredInputType); err != nil {
			return fmt.Errorf("invalid input type: %w", err)
		}
	}

	// Check that the workflow can return the required type
	if err := v.validateReturnType(chain, profile); err != nil {
		return fmt.Errorf("invalid return type: %w", err)
	}

	return nil
}

func (v *Validator) validateInputType(chain *taskengine.TaskChainDefinition, requiredType string) error {
	// For now, we assume workflows can handle the required input type
	// In the future, we could analyze the first task's expected input
	return nil
}

func (v *Validator) validateReturnType(chain *taskengine.TaskChainDefinition, profile ValidationProfile) error {
	graph := v.buildTaskGraph(chain.Tasks)
	terminalTasks := v.findTerminalTasks(chain.Tasks, graph)

	for _, task := range terminalTasks {
		if !v.taskReturnsCompatibleType(task, profile) &&
			!v.pathLeadsToCompatibleType(task, chain.Tasks, graph, profile) {
			return fmt.Errorf("task %q does not guarantee %s return type", task.ID, profile.RequiredReturnType)
		}
	}

	return nil
}

// Helper methods
func (v *Validator) buildTaskGraph(tasks []taskengine.TaskDefinition) map[string][]string {
	graph := make(map[string][]string)
	for _, task := range tasks {
		var nextTasks []string

		// Add normal transitions
		for _, branch := range task.Transition.Branches {
			if branch.Goto != "end" && branch.Goto != "" {
				nextTasks = append(nextTasks, branch.Goto)
			}
		}

		// Add failure transitions
		if task.Transition.OnFailure != "" && task.Transition.OnFailure != "end" {
			nextTasks = append(nextTasks, task.Transition.OnFailure)
		}

		graph[task.ID] = nextTasks
	}
	return graph
}

func (v *Validator) findTerminalTasks(tasks []taskengine.TaskDefinition, graph map[string][]string) []taskengine.TaskDefinition {
	var terminals []taskengine.TaskDefinition

	for _, task := range tasks {
		if v.canTransitionToEnd(task) {
			terminals = append(terminals, task)
		}
	}

	return terminals
}

func (v *Validator) canTransitionToEnd(task taskengine.TaskDefinition) bool {
	// Check normal branches
	for _, branch := range task.Transition.Branches {
		if branch.Goto == "end" {
			return true
		}
	}

	// Check failure transition
	if task.Transition.OnFailure == "end" {
		return true
	}

	return false
}

func (v *Validator) taskReturnsCompatibleType(task taskengine.TaskDefinition, profile ValidationProfile) bool {
	switch task.Handler {
	case taskengine.HandleChatCompletion, taskengine.HandleExecuteToolCalls:
		return profile.RequiredReturnType == "chat_history" ||
			(profile.AllowConvertibleTypes && profile.RequiredReturnType == "openai_chat_response")
	case taskengine.HandleConvertToOpenAIChatResponse:
		return profile.RequiredReturnType == "openai_chat_response"
	case taskengine.HandlePromptToString:
		// Check if this task composes with chat_history
		for _, branch := range task.Transition.Branches {
			if branch.Compose != nil &&
				(branch.Compose.Strategy == "append_string_to_chat_history" ||
					branch.Compose.Strategy == "merge_chat_histories") {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (v *Validator) pathLeadsToCompatibleType(task taskengine.TaskDefinition, allTasks []taskengine.TaskDefinition, graph map[string][]string, profile ValidationProfile) bool {
	visited := make(map[string]bool)
	queue := []string{task.ID}

	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]

		if visited[currentID] {
			continue
		}
		visited[currentID] = true

		currentTask := v.findTaskByID(allTasks, currentID)
		if currentTask == nil {
			continue
		}

		if v.taskReturnsCompatibleType(*currentTask, profile) {
			return true
		}

		// Add next tasks to queue
		if nextTasks, exists := graph[currentID]; exists {
			queue = append(queue, nextTasks...)
		}
	}

	return false
}

func (v *Validator) findTaskByID(tasks []taskengine.TaskDefinition, id string) *taskengine.TaskDefinition {
	for i, task := range tasks {
		if task.ID == id {
			return &tasks[i]
		}
	}
	return nil
}
