package contenoxcli

import (
	"strconv"

	"github.com/contenox/beam/internal/models/hostcapacity"
	"github.com/contenox/beam/internal/models/modelregistry"
)

type fitLevel int

const (
	fitUnknown fitLevel = iota
	fitFree
	fitTotal
	fitNo
)

func fitFor(model modelregistry.ModelDescriptor, budget hostcapacity.Budget) fitLevel {
	required := model.EstimatedResidentBytes()
	if !budget.Known || required <= 0 {
		return fitUnknown
	}
	if budget.FreeBytes > 0 && required <= budget.FreeBytes {
		return fitFree
	}
	if budget.TotalBytes > 0 && required <= budget.TotalBytes {
		return fitTotal
	}
	return fitNo
}

func fitMark(level fitLevel) string {
	switch level {
	case fitFree:
		return "yes"
	case fitTotal:
		return "maybe"
	case fitNo:
		return "no"
	default:
		return "-"
	}
}

func modelUseCaseLabel(model modelregistry.ModelDescriptor) string {
	if model.UseCase == "" {
		if model.Vision {
			return "vision"
		}
		return "-"
	}
	if model.Vision {
		return model.UseCase + "+vision"
	}
	return model.UseCase
}

func modelRecommendationTierSort(model modelregistry.ModelDescriptor) int {
	if model.RecommendedVRAMGB <= 0 {
		return int(^uint(0) >> 1)
	}
	return model.RecommendedVRAMGB
}

func lessModelRecommendation(left, right modelregistry.ModelDescriptor) bool {
	if modelRecommendationTierSort(left) != modelRecommendationTierSort(right) {
		return modelRecommendationTierSort(left) < modelRecommendationTierSort(right)
	}
	if left.UseCase != right.UseCase {
		return left.UseCase < right.UseCase
	}
	if left.Family != right.Family {
		return left.Family < right.Family
	}
	if left.SizeBytes != right.SizeBytes {
		return left.SizeBytes < right.SizeBytes
	}
	return left.Name < right.Name
}

func humanModelBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return formatOneDecimal(float64(n)/gib) + " GiB"
	case n >= mib:
		return formatOneDecimal(float64(n)/mib) + " MiB"
	case n >= kib:
		return formatOneDecimal(float64(n)/kib) + " KiB"
	case n > 0:
		return formatOneDecimal(float64(n)) + " B"
	default:
		return "-"
	}
}

func formatOneDecimal(v float64) string {
	rounded := int64(v*10 + 0.5)
	if rounded%10 == 0 {
		return strconv.FormatInt(rounded/10, 10)
	}
	return strconv.FormatInt(rounded/10, 10) + "." + strconv.FormatInt(rounded%10, 10)
}
