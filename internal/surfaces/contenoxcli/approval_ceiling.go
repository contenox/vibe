package contenoxcli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
)

// approvalCeilingKey holds the wait an ask gets when its own grant states none. It is global rather than workspace-scoped: it bounds the host, not one project.
const approvalCeilingKey = "approval-ceiling"

// normalizeApprovalCeiling validates and canonicalises the ceiling at `config
// set`. It returns ("", nil) for keys it does not own.
func normalizeApprovalCeiling(key, value string) (string, error) {
	if key != approvalCeilingKey {
		return "", nil
	}
	wait, err := hitlservice.ParseWait(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	// Stored canonically so `config get` reads back what the runtime uses.
	return hitlservice.FormatWait(wait), nil
}

// approvalCeilingFromConfig reads the stored ceiling. Unset or unparseable
// comes back as zero, which leaves the compiled-in fallback in place.
func approvalCeilingFromConfig(ctx context.Context, store clikv.KVReader) time.Duration {
	raw := strings.TrimSpace(clikv.Read(ctx, store, approvalCeilingKey))
	if raw == "" {
		return 0
	}
	wait, err := hitlservice.ParseWait(raw)
	if err != nil {
		return 0
	}
	return wait
}

func applyApprovalCeiling(ctx context.Context, svc hitlservice.Service, store clikv.KVReader) {
	hitlservice.SetApprovalCeiling(svc, approvalCeilingFromConfig(ctx, store))
}
