package hookpost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmadraza-khan/hookpost/signature"
)

func internalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A worker that leases a delivery and dies must not lose it: once the lease
// expires another worker sends it again with the same webhook-id, and if the
// dead worker comes back late its result must not overwrite the newer one.
func TestLeaseExpiryAndStaleWorker(t *testing.T) {
	pool := internalPool(t)
	ctx := context.Background()
	c, err := New(pool, Config{
		AllowPrivateNetworks: true, PollInterval: 20 * time.Millisecond,
		Schedule: []time.Duration{time.Millisecond}, Retention: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE hookpost_endpoints, hookpost_messages,
		hookpost_deliveries, hookpost_attempts CASCADE`); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(signature.HeaderID))
		mu.Unlock()
	}))
	defer srv.Close()
	secret, _ := signature.NewSecret()
	if _, err := pool.Exec(ctx, `INSERT INTO hookpost_endpoints (id, owner_id, url, secret)
		VALUES ('ep_1', 'o', $1, $2)`, srv.URL, secret); err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(ctx, pool, Message{OwnerID: "o", EventType: "e"})
	if err != nil {
		t.Fatal(err)
	}

	// Worker 1 leases the delivery, then "crashes" before sending.
	stale, err := c.lease(ctx, 10)
	if err != nil || len(stale) != 1 || stale[0].attempt != 1 {
		t.Fatalf("lease: %v %+v", err, stale)
	}
	if again, err := c.lease(ctx, 10); err != nil || len(again) != 0 {
		t.Fatalf("leased row was handed out twice: %v %+v", err, again)
	}

	// Lease expires; a running dispatcher picks it up.
	if _, err := pool.Exec(ctx, `UPDATE hookpost_deliveries
		SET next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(runCtx) }()
	status := func() (s string, attempts int) {
		_ = pool.QueryRow(ctx, `SELECT status, attempts FROM hookpost_deliveries`).Scan(&s, &attempts)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for s, _ := status(); s != "succeeded"; s, _ = status() {
		if time.Now().After(deadline) {
			t.Fatal("delivery not resent after lease expiry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	// Worker 1 comes back and reports a failure for its old attempt.
	stale[0].url = "http://127.0.0.1:1/" // connection refused
	c.deliver(ctx, stale[0])
	if s, attempts := status(); s != "succeeded" || attempts != 2 {
		t.Fatalf("stale worker overwrote result: %s, %d attempts", s, attempts)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 1 || ids[0] != res.MessageID {
		t.Fatalf("receiver saw %v, want one request with %s", ids, res.MessageID)
	}
}
