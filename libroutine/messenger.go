package libroutine

import (
	"context"

	"github.com/contenox/contenox/libbus"
)

// SubscribeMessenger triggers r (see Trigger) every time a message is
// published to subject on bus, so a Job chain can react to an external
// event — e.g. another component publishing "process.myproc.running" —
// through this codebase's existing pub/sub abstraction (libbus.Messenger)
// rather than a bespoke event mechanism.
//
// The subscription is torn down automatically when ctx is done (see
// libbus.Messenger.Stream); the returned Subscription can also be used to
// unsubscribe earlier.
func (r *Runner) SubscribeMessenger(ctx context.Context, bus libbus.Messenger, subject string) (libbus.Subscription, error) {
	ch := make(chan []byte, 1)
	sub, err := bus.Stream(ctx, subject, ch)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				r.Trigger(ctx)
			}
		}
	}()

	return sub, nil
}
