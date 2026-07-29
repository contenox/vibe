//go:build !openvino || !openvino_genai

package ovsession

import (
	"context"
	"errors"
)

type EmbedSession struct{}

func NewEmbed(modelDir, device string) (*EmbedSession, error) {
	return nil, errors.New("openvino GenAI backend not compiled")
}

func (s *EmbedSession) Embed(ctx context.Context, prompt string) ([]float32, error) {
	return nil, errors.New("openvino GenAI backend not compiled")
}

func (s *EmbedSession) Close() error { return nil }
