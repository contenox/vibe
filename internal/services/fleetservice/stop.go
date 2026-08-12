// stop.go is the operator's mission kill switch. StopMission is the durable
// half (status, asks, checkpoints, over the shared store); the live half is
// the StatusChanged subscriber BuildInProcess wires, which reaps the unit
// subprocess in whichever process hosts it.
package fleetservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
)

// StopMission abandons a running mission — finishes it (a terminal mission returns a conflict instead), closes every pending ask it filed, and deletes their checkpoints; the subprocess itself is reaped separately by the host's StatusChanged subscriber.
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
