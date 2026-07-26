package libkvstore

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/valkey"
)

func SetupLocalValKeyInstance(ctx context.Context) (string, testcontainers.Container, func(), error) {
	cleanup := func() {}

	container, err := valkey.Run(ctx, "docker.io/valkey/valkey:7.2.5")
	if err != nil {
		return "", nil, cleanup, err
	}

	cleanup = func() {
		timeout := time.Second
		err := container.Stop(ctx, &timeout)
		if err != nil {
			fmt.Println(err, "failed to terminate container")
		}
	}

	conn, err := container.ConnectionString(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}
	return conn, container, cleanup, nil
}
