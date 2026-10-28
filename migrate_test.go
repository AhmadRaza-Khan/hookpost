package hookpost_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ahmadraza-khan/hookpost"
)

func newMigrateClient(t *testing.T) *hookpost.Client {
	t.Helper()
	c, err := hookpost.New(needDB(t), hookpost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func dropTables(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `DROP TABLE IF EXISTS
		hookpost_attempts, hookpost_deliveries, hookpost_messages,
		hookpost_endpoints, hookpost_schema_version CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewNilPool(t *testing.T) {
	if _, err := hookpost.New(nil, hookpost.Config{}); !errors.Is(err, hookpost.ErrNoPool) {
		t.Fatalf("got %v, want ErrNoPool", err)
	}
}

func TestMigrateFreshAndTwice(t *testing.T) {
	c := newMigrateClient(t)
	dropTables(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := c.Migrate(ctx); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}

	var version int
	if err := testPool.QueryRow(ctx,
		`SELECT MAX(version) FROM hookpost_schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("schema version %d, want 1", version)
	}
	for _, table := range []string{
		"hookpost_endpoints", "hookpost_messages", "hookpost_deliveries", "hookpost_attempts",
	} {
		var ok bool
		if err := testPool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("table %s missing", table)
		}
	}
}

func TestMigrateConcurrent(t *testing.T) {
	c := newMigrateClient(t)
	dropTables(t)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Migrate(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}

// Payload is BYTEA so the exact bytes survive; JSONB would reorder keys.
func TestPayloadBytesPreserved(t *testing.T) {
	c := newMigrateClient(t)
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"z":1,  "a":2}`)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO hookpost_messages (id, owner_id, event_type, payload)
		VALUES ('msg_bytes', 'o', 't', $1)
		ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload`, body); err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := testPool.QueryRow(ctx,
		`SELECT payload FROM hookpost_messages WHERE id = 'msg_bytes'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("got %q, want %q", got, body)
	}
}
