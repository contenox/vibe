package planservice

import (
	"fmt"
	"strings"
)

// normalizeAndValidatePlannerSteps trims each step and runs validation.
func normalizeAndValidatePlannerSteps(steps []string) ([]string, error) {
	out := make([]string, len(steps))
	for i := range steps {
		out[i] = strings.TrimSpace(steps[i])
	}
	if err := validatePlannerStepStrings(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Limits for planner output (defensive; avoids huge DB rows and RSC/log paste pollution).
const (
	maxPlannerSteps       = 100
	maxPlannerStepBytes   = 12000
	plannerEscapeRatioNum = 3 // reject if backslashes exceed len/ratio (RSC/stream leak)
	plannerEscapeMinLen   = 400
)

// validatePlannerStepStrings checks count, per-step length, empties, and obvious garbage
// (e.g. Next.js flight stream pasted into a step). Called after JSON parse succeeds.
func validatePlannerStepStrings(steps []string) error {
	if len(steps) == 0 {
		return fmt.Errorf("planner returned no steps")
	}
	if len(steps) > maxPlannerSteps {
		return fmt.Errorf("planner returned too many steps (%d, max %d)", len(steps), maxPlannerSteps)
	}
	for i, s := range steps {
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("planner step %d is empty after trim", i+1)
		}
		if len(s) > maxPlannerStepBytes {
			return fmt.Errorf("planner step %d exceeds max length (%d bytes, max %d)", i+1, len(s), maxPlannerStepBytes)
		}
		if plannerStepLooksCorrupted(s) {
			return fmt.Errorf("planner step %d looks corrupted (stream or log paste); try a shorter goal or replan", i+1)
		}
	}
	return nil
}

// plannerStepLooksCorrupted detects accidental inclusion of framework build streams or similar.
func plannerStepLooksCorrupted(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "__next_f") || strings.Contains(lower, "self.__next_f") {
		return true
	}
	if len(s) >= plannerEscapeMinLen {
		n := strings.Count(s, "\\")
		if n*plannerEscapeRatioNum > len(s) {
			return true
		}
	}
	return false
}
