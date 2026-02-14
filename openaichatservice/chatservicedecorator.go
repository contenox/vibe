package openaichatservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

// OpenAIChatCompletions wraps the non-streaming method to track its execution.
func (d *activityTrackerDecorator) OpenAIChatCompletions(ctx context.Context, chainID string, req taskengine.OpenAIChatRequest) (*taskengine.OpenAIChatResponse, []taskengine.CapturedStateUnit, error) {
	// Start tracking with relevant context
	reportErr, _, endFn := d.tracker.Start(
		ctx,
		"openai_chat_completions",
		"chat",
		"chain_id", chainID,
		"model", req.Model,
		"message_count", len(req.Messages),
		"max_tokens", req.MaxTokens,
	)
	defer endFn()

	// Execute the operation
	resp, traces, err := d.service.OpenAIChatCompletions(ctx, chainID, req)
	if err != nil {
		// Report error with additional context
		reportErr(fmt.Errorf("chat completions failed: %w", err))
		return nil, traces, err
	}

	return resp, traces, nil
}

// OpenAIChatCompletionsStream wraps the streaming method to track its initiation.
func (d *activityTrackerDecorator) OpenAIChatCompletionsStream(ctx context.Context, chainID string, req taskengine.OpenAIChatRequest, speed time.Duration) (<-chan OpenAIChatStreamChunk, error) {
	// Start tracking the initiation of the stream
	reportErr, _, endFn := d.tracker.Start(
		ctx,
		"openai_chat_completions_stream",
		"chat",
		"chain_id", chainID,
		"model", req.Model,
		"stream", req.Stream,
	)
	defer endFn()

	// Execute the operation to get the stream channel
	streamChan, err := d.service.OpenAIChatCompletionsStream(ctx, chainID, req, speed)
	if err != nil {
		// Report any error that occurs before the stream begins
		reportErr(fmt.Errorf("chat completions stream failed: %w", err))
		return nil, err
	}

	return streamChan, nil
}

// WithActivityTracker creates a new decorated service that tracks activity
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

// Ensure the decorator implements the Service interface
var _ Service = (*activityTrackerDecorator)(nil)
