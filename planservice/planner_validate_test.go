package planservice

import (
	"strings"
	"testing"
)

func Test_validatePlannerStepStrings_corrupted(t *testing.T) {
	t.Parallel()
	cases := []string{
		"run npm build self.__next_f.push([1",
		"step with __next_f in stream",
	}
	for i, s := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			t.Parallel()
			err := validatePlannerStepStrings([]string{s})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func Test_validatePlannerStepStrings_escapeDensity(t *testing.T) {
	t.Parallel()
	s := strings.Repeat(`\\`, 250) // long escape-heavy blob
	err := validatePlannerStepStrings([]string{s})
	if err == nil {
		t.Fatal("expected error for escape-heavy step")
	}
}

func Test_validatePlannerStepStrings_tooManySteps(t *testing.T) {
	t.Parallel()
	steps := make([]string, maxPlannerSteps+1)
	for i := range steps {
		steps[i] = "x"
	}
	err := validatePlannerStepStrings(steps)
	if err == nil {
		t.Fatal("expected error")
	}
}
