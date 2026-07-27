// stop.go — the operator's mission kill switch. StopMission is the durable
// half (status, asks, checkpoints — any process may run it over the shared
// store); the live half is the StatusChanged subscriber BuildInProcess wires,
// which reaps the unit subprocess in whichever process hosts it.
package fleetservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// StopMission abandons a running mission: the guarded terminal transition
// first (so a finished mission returns a teaching conflict instead of being
// re-finished), then every pending ask the mission filed closes WITHOUT the
// resume hook, and every checkpoint suspended under those asks is deleted —
// nothing of a stopped mission may resume. The unit subprocess itself is torn
// down by the host's StatusChanged subscriber (see BuildInProcess), which the
// publisher-wired missionservice reaches cross-process over the SQLite bus.
func StopMission(ctx context.Context, missions missionservice.Service, hitl hitlservice.Service, store runtimetypes.Store, missionID, reason string) error {
	if reason == "" {
		reason = "stopped by operator"
	}
	if _, err := missions.Finish(ctx, missionID, missionservice.StatusAbandoned, reason); err != nil {
		return err
	}
	closed, err := hitl.AbandonMissionAsks(ctx, missionID)
	if err != nil {
		return fmt.Errorf("mission %s is abandoned, but closing its pending asks failed: %w", missionID, err)
	}
	for _, askID := range closed {
		if err := store.DeleteChainCheckpoint(ctx, askID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			return fmt.Errorf("mission %s is abandoned, but deleting checkpoint %s failed: %w", missionID, askID, err)
		}
	}
	return nil
}

// runStatusTeardown subscribes kernel teardown to mission terminal events: when
// a mission this kernel hosts reaches a terminal status, its unit subprocess is
// stopped. Cross-process by construction — the SQLite bus broadcasts every
// event to every subscriber, and a kernel that does not host the instance gets
// a not-found from Stop and moves on. Returns an unsubscribe func.
func runStatusTeardown(ctx context.Context, bus libbus.Messenger, missions missionservice.Service, kernel agentinstance.Manager) (func(), error) {
	ch := make(chan []byte, 16)
	sub, err := bus.Stream(ctx, missionservice.StatusChangedSubject, ch)
	if err != nil {
		return nil, fmt.Errorf("subscribe mission status events: %w", err)
	}
	go func() {
		for data := range ch {
			var ev missionservice.StatusChangedEvent
			if err := json.Unmarshal(data, &ev); err != nil || ev.MissionID == "" {
				continue
			}
			// The event is self-contained but names no instance; the mission
			// row holds which unit to reap.
			m, err := missions.Get(ctx, ev.MissionID)
			if err != nil || m.InstanceID == "" {
				continue
			}
			_ = kernel.Stop(m.InstanceID) // not hosted here → not-found → fine
		}
	}()
	return func() { _ = sub.Unsubscribe() }, nil
}
