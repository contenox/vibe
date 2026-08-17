package runtimetypes

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

const TestPostgresRequiredEnv = "CONTENOX_TEST_REQUIRE_POSTGRES"

var (
	testPostgresOnce    sync.Once
	testPostgresDSN     string
	testPostgresAdmin   libdb.DBManager
	testPostgresErr     error
	testPostgresCleanup = func() {}
	testPostgresSeq     atomic.Uint64
)

func ShutdownTestBackends() {
	if testPostgresAdmin != nil {
		_ = testPostgresAdmin.Close()
		testPostgresAdmin = nil
	}
	testPostgresCleanup()
	testPostgresCleanup = func() {}
}

func setupTestPostgres(t *testing.T, ctx context.Context) libdb.DBManager {
	t.Helper()
	if os.Getenv(TestPostgresRequiredEnv) == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}

	testPostgresOnce.Do(startTestPostgres)
	if testPostgresErr != nil {
		msg := fmt.Sprintf("Postgres store NOT exercised: could not start the postgres:17-bookworm container (%v). "+
			"Docker is required; set %s=1 to turn this skip into a failure.", testPostgresErr, TestPostgresRequiredEnv)
		if os.Getenv(TestPostgresRequiredEnv) != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}

	name := fmt.Sprintf("store_test_%d", testPostgresSeq.Add(1))
	_, err := testPostgresAdmin.WithoutTransaction().ExecContext(ctx, `CREATE DATABASE `+name)
	require.NoError(t, err)

	dsn, err := testPostgresDatabaseDSN(name)
	require.NoError(t, err)

	dbManager, err := libdb.NewPostgresDBManager(ctx, dsn, SchemaPostgres)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
		if testPostgresAdmin == nil {
			return
		}
		_, err := testPostgresAdmin.WithoutTransaction().ExecContext(
			context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		require.NoError(t, err)
	})

	return dbManager
}

func startTestPostgres() {
	ctx := context.Background()

	dsn, _, cleanup, err := libdb.SetupLocalInstance(ctx, "contenox_store_test", "contenox", "contenox")
	if err != nil {
		cleanup()
		testPostgresErr = err
		return
	}
	admin, err := libdb.NewPostgresDBManager(ctx, dsn, "")
	if err != nil {
		cleanup()
		testPostgresErr = err
		return
	}
	testPostgresDSN, testPostgresAdmin, testPostgresCleanup = dsn, admin, cleanup
}

func testPostgresDatabaseDSN(database string) (string, error) {
	u, err := url.Parse(testPostgresDSN)
	if err != nil {
		return "", fmt.Errorf("parse test postgres dsn: %w", err)
	}
	u.Path = "/" + database
	return u.String(), nil
}
