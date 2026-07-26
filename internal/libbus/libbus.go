/*
Package bus provides an interface for core publish-subscribe messaging.

It is designed to be a simple, high-level abstraction over a message broker,
offering common patterns like fire-and-forget publishing, streaming subscriptions,
and request-reply.

Basic Usage:

	// Configuration (replace with your actual values)
	cfg := &bus.Config{
		NATSURL: "nats://127.0.0.1:4222",
	}

	// Create a new Messenger instance
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	messenger, err := bus.NewPubSub(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer messenger.Close()

	// --- Publish a message ---
	err = messenger.Publish(context.Background(), "updates.topic", []byte("hello world"))
	if err != nil {
		log.Printf("Publish failed: %v", err)
	}

	// --- Stream messages ---
	msgChan := make(chan []byte, 64)
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	sub, err := messenger.Stream(streamCtx, "updates.topic", msgChan)
	if err != nil {
		log.Fatalf("Stream failed: %v", err)
	}
	defer sub.Unsubscribe()

	// --- Serve requests ---
	handler := func(ctx context.Context, data []byte) ([]byte, error) {
		log.Printf("Handler received: %s", string(data))
		return []byte("ack"), nil
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveSub, err := messenger.Serve(serveCtx, "service.topic", handler)
	if err != nil {
		log.Fatalf("Serve failed: %v", err)
	}
	defer serveSub.Unsubscribe()
*/
package libbus
