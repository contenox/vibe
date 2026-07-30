package transport

import "testing"

func TestResolvePlannerEffectiveContext(t *testing.T) {
	info := ModelInfo{
		ModelMaxContext:         32768,
		PlannerEffectiveContext: 256,
	}

	tests := []struct {
		name      string
		requested int
		numCtx    int
		info      ModelInfo
		want      int
	}{
		{
			name:      "honors caller planner above hot window",
			requested: 384,
			numCtx:    256,
			info:      info,
			want:      384,
		},
		{
			name:      "zero uses daemon planner",
			requested: 0,
			numCtx:    256,
			info: ModelInfo{
				ModelMaxContext:         32768,
				PlannerEffectiveContext: 1024,
			},
			want: 1024,
		},
		{
			name:      "clamps to model max",
			requested: 65536,
			numCtx:    256,
			info:      info,
			want:      32768,
		},
		{
			name:      "never below hot window",
			requested: 128,
			numCtx:    256,
			info:      info,
			want:      256,
		},
		{
			name:      "missing daemon planner falls back to hot window",
			requested: 0,
			numCtx:    256,
			info:      ModelInfo{ModelMaxContext: 32768},
			want:      256,
		},
		{
			name:      "unknown model max does not clamp caller request",
			requested: 65536,
			numCtx:    256,
			info:      ModelInfo{},
			want:      65536,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolvePlannerEffectiveContext(tt.requested, tt.numCtx, tt.info)
			if got != tt.want {
				t.Fatalf("ResolvePlannerEffectiveContext(%d, %d, %+v) = %d, want %d",
					tt.requested, tt.numCtx, tt.info, got, tt.want)
			}
		})
	}
}
