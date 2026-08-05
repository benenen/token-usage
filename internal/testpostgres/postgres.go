// Package testpostgres starts disposable PostgreSQL containers for integration
// tests. It never accepts an external DSN, so tests cannot truncate a
// developer or production database by mistake.
package testpostgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	image    = "postgres:15.13-alpine"
	database = "tokenusage_codex_test_container"
	username = "tokenusage_test"
	password = "tokenusage_test"
)

// Run provisions one isolated database for a test package. Docker is a test
// prerequisite: startup failures fail the package instead of silently skipping
// the integration tests.
func Run(m *testing.M) int {
	// Go runs packages concurrently under one Testcontainers session. Starting
	// and stopping a shared Ryuk container can race between short-lived packages;
	// this fixture terminates its own container explicitly instead.
	if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true"); err != nil {
		fmt.Fprintf(os.Stderr, "disable shared Testcontainers reaper: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(database),
		postgres.WithUsername(username),
		postgres.WithPassword(password),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start disposable PostgreSQL %s: %v\n", image, err)
		return 2
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		fmt.Fprintf(os.Stderr, "get disposable PostgreSQL DSN: %v\n", err)
		return 2
	}
	if err := os.Setenv("TOKENUSAGE_TEST_DSN", dsn); err != nil {
		_ = testcontainers.TerminateContainer(container)
		fmt.Fprintf(os.Stderr, "publish disposable PostgreSQL DSN: %v\n", err)
		return 2
	}

	code := m.Run()
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate disposable PostgreSQL: %v\n", err)
		if code == 0 {
			return 2
		}
	}
	return code
}
