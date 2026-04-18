package local

import (
	"fmt"
	"sync"

	"github.com/ollama/ollama/llama"
)

type loadedModel struct {
	model *llama.Model
	mu    sync.Mutex
}

var modelPool sync.Map // map[string]*loadedModel

func acquireModel(modelPath string) (*loadedModel, error) {
	if v, ok := modelPool.Load(modelPath); ok {
		return v.(*loadedModel), nil
	}

	params := llama.ModelParams{
		VocabOnly:    false,
		NumGpuLayers: 0,
	}
	m, err := llama.LoadModelFromFile(modelPath, params)
	if err != nil {
		return nil, fmt.Errorf("load model %s: %w", modelPath, err)
	}

	lm := &loadedModel{model: m}
	actual, _ := modelPool.LoadOrStore(modelPath, lm)
	return actual.(*loadedModel), nil
}
