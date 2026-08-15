package fleetservice

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/require"
)

type stubGuidance struct {
	notes []hitlservice.GuidanceNote
	err   error
}

func (s stubGuidance) AgentGuidanceFor(context.Context, string) ([]hitlservice.GuidanceNote, error) {
	return s.notes, s.err
}

// TestUnit_NudgeText_CarriesAdjudicatedGuidance pins the unsticking path: a
// call refused by an agent tells the unit what to do instead on the very next
// turn. Without this the unit only ever sees "rejected" and circles.
func TestUnit_NudgeText_CarriesAdjudicatedGuidance(t *testing.T) {
	t.Parallel()
	s := &service{guidance: stubGuidance{notes: []hitlservice.GuidanceNote{
		{ToolsName: "local_fs", ToolName: "write_file", DecidedBy: "oracle", Guidance: "write under ./out, not /tmp"},
		{ToolsName: "local_shell", ToolName: "local_shell", DecidedBy: "oracle", Guidance: "read the file instead of deleting it"},
	}}}

	got := s.nudgeText(context.Background(), "m-1")
	require.Contains(t, got, "local_fs.write_file was refused: write under ./out, not /tmp")
	require.Contains(t, got, "local_shell.local_shell was refused: read the file instead of deleting it")
	require.Contains(t, got, missionNudge, "the guidance prefixes the nudge; it never replaces it")
	require.True(t, strings.Index(got, "write under ./out") < strings.Index(got, missionNudge),
		"the reason the unit was blocked comes before the generic instruction")
}

// TestUnit_NudgeText_UnchangedWithoutGuidance pins the default: no adjudicator,
// no reader, or nothing refused all leave the nudge exactly as it was.
func TestUnit_NudgeText_UnchangedWithoutGuidance(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]*service{
		"no reader wired": {},
		"nothing refused": {guidance: stubGuidance{}},
		"reader errors":   {guidance: stubGuidance{err: fmt.Errorf("store down")}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, missionNudge, s.nudgeText(context.Background(), "m-1"))
		})
	}
}
