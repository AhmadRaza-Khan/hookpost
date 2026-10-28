package hookpost_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool is nil when DATABASE_URL is unset; database tests then skip.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	code := func() int {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			fmt.Fprintln(os.Stderr, "DATABASE_URL not set: skipping database tests")
			return m.Run()
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect:", err)
			return 1
		}
		defer pool.Close()
		testPool = pool
		return m.Run()
	}()
	os.Exit(code)
}

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	return testPool
}
