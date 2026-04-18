package local

import (
	"context"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/modelrepo"
)

func init() {
	modelrepo.RegisterCatalogProvider("local", func(spec modelrepo.BackendSpec, _ modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		return &catalogProvider{dir: spec.BaseURL}, nil
	})
}

type catalogProvider struct {
	dir string
}

func (c *catalogProvider) Type() string { return "local" }

func (c *catalogProvider) ListModels(_ context.Context) ([]modelrepo.ObservedModel, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var out []modelrepo.ObservedModel
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gguf := filepath.Join(c.dir, e.Name(), "model.gguf")
		if _, err := os.Stat(gguf); err != nil {
			continue
		}
		info, _ := e.Info()
		out = append(out, modelrepo.ObservedModel{
			Name: e.Name(),
			CapabilityConfig: modelrepo.CapabilityConfig{
				CanChat:   true,
				CanPrompt: true,
				CanStream: true,
				CanEmbed:  true,
			},
			ModifiedAt: info.ModTime(),
		})
	}
	return out, nil
}

func (c *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return newLocalProvider(model.Name, c.dir, model.CapabilityConfig)
}
