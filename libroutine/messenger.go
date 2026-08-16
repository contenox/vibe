package libroutine

import (
	"context"

	"github.com/contenox/contenox/libbus"
)

// SubscribeMessenger triggers r every time a message is published to subject on
// bus. The subscription is torn down when ctx is done, or earlier via the
// returned Subscription.
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
