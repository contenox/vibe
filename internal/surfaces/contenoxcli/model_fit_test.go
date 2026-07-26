package contenoxcli

import (
	"testing"

	"github.com/contenox/beam/internal/models/hostcapacity"
	"github.com/contenox/beam/internal/models/modelregistry"
	"github.com/stretchr/testify/assert"
)

func TestUnit_ModelFitFor(t *testing.T) {
	model := modelregistry.ModelDescriptor{SizeBytes: 4 << 30} // 5 GiB resident estimate

	tests := []struct {
		name   string
		budget hostcapacity.Budget
		want   fitLevel
	}{
		{
			name:   "unknown without host signal",
			budget: hostcapacity.Budget{},
			want:   fitUnknown,
		},
		{
			name:   "fits currently free memory",
			budget: hostcapacity.Budget{Known: true, TotalBytes: 8 << 30, FreeBytes: 6 << 30},
			want:   fitFree,
		},
		{
			name:   "could fit if memory is freed",
			budget: hostcapacity.Budget{Known: true, TotalBytes: 8 << 30, FreeBytes: 3 << 30},
			want:   fitTotal,
		},
		{
			name:   "does not fit host budget",
			budget: hostcapacity.Budget{Known: true, TotalBytes: 4 << 30, FreeBytes: 3 << 30},
			want:   fitNo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fitFor(model, tt.budget))
		})
	}
}

func TestUnit_ModelFitForUnknownWhenModelSizeMissing(t *testing.T) {
	got := fitFor(modelregistry.ModelDescriptor{}, hostcapacity.Budget{
		Known:      true,
		TotalBytes: 8 << 30,
		FreeBytes:  8 << 30,
	})
	assert.Equal(t, fitUnknown, got)
}

func TestUnit_ModelFitMark(t *testing.T) {
	assert.Equal(t, "yes", fitMark(fitFree))
	assert.Equal(t, "maybe", fitMark(fitTotal))
	assert.Equal(t, "no", fitMark(fitNo))
	assert.Equal(t, "-", fitMark(fitUnknown))
}

func TestUnit_ModelRecommendationLabelsAndSort(t *testing.T) {
	assert.Equal(t, "-", modelUseCaseLabel(modelregistry.ModelDescriptor{}))
	assert.Equal(t, "coding", modelUseCaseLabel(modelregistry.ModelDescriptor{UseCase: "coding"}))
	assert.Equal(t, "chat+vision", modelUseCaseLabel(modelregistry.ModelDescriptor{UseCase: "chat", Vision: true}))
	assert.Equal(t, "vision", modelUseCaseLabel(modelregistry.ModelDescriptor{Vision: true}))

	smallCoding := modelregistry.ModelDescriptor{Name: "a", UseCase: "coding", Family: "qwen", SizeBytes: 1, RecommendedVRAMGB: 6}
	largeCoding := modelregistry.ModelDescriptor{Name: "b", UseCase: "coding", Family: "qwen", SizeBytes: 2, RecommendedVRAMGB: 16}
	unknownTier := modelregistry.ModelDescriptor{Name: "c", UseCase: "coding", Family: "qwen", SizeBytes: 1}

	assert.True(t, lessModelRecommendation(smallCoding, largeCoding))
	assert.True(t, lessModelRecommendation(largeCoding, unknownTier))
	assert.False(t, lessModelRecommendation(unknownTier, smallCoding))
}

func TestUnit_HumanModelBytes(t *testing.T) {
	assert.Equal(t, "-", humanModelBytes(0))
	assert.Equal(t, "512 B", humanModelBytes(512))
	assert.Equal(t, "1 KiB", humanModelBytes(1<<10))
	assert.Equal(t, "1.5 GiB", humanModelBytes(1536<<20))
}
