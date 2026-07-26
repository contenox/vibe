package libdbexec

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func SetupLocalInstance(ctx context.Context, dbName, dbUser, dbPassword string) (string, *postgres.PostgresContainer, func(), error) {
	cleanup := func() {}
	container, err := postgres.Run(ctx,
		"postgres:17-bookworm",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(10*time.Second)),
	)
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

	connectionString, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", nil, nil, err
	}
	return connectionString, container, cleanup, nil
}
