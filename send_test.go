package hookpost_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmadraza-khan/hookpost"
)

// newSendClient migrates and returns a client plus an owner ID unique to the
// test, so tests don't see each other's rows.
func newSendClient(t *testing.T, cfg hookpost.Config) (*hookpost.Client, string) {
	t.Helper()
	pool := needDB(t)
	c, err := hookpost.New(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner := "owner_" + strings.ReplaceAll(t.Name(), "/", "_") + "_" + time.Now().Format("150405.000000")
	return c, owner
}

// addEndpoint inserts an endpoint row directly (CreateEndpoint comes later).
func addEndpoint(t *testing.T, owner string, eventTypes []string, disabled bool) string {
	t.Helper()
	if eventTypes == nil {
		eventTypes = []string{}
	}
	id := "ep_" + owner + "_" + strings.Join(eventTypes, "_")
	if disabled {
		id += "_off"
	}
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO hookpost_endpoints (id, owner_id, url, secret, event_types, disabled)
		VALUES ($1, $2, 'https://example.com/hooks', 'whsec_dGVzdA==', $3, $4)`,
		id, owner, eventTypes, disabled)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deliveriesFor(t *testing.T, msgID string) int {
	return countRows(t, `SELECT count(*) FROM hookpost_deliveries WHERE message_id = $1`, msgID)
}

func TestSendRollbackLeavesNothing(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	addEndpoint(t, owner, nil, false)
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(ctx, tx, hookpost.Message{OwnerID: owner, EventType: "order.created", Payload: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoints != 1 {
		t.Fatalf("queued %d inside tx, want 1", res.Endpoints)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, `SELECT count(*) FROM hookpost_messages WHERE id = $1`, res.MessageID); n != 0 {
		t.Fatalf("message survived rollback")
	}
	if n := deliveriesFor(t, res.MessageID); n != 0 {
		t.Fatalf("deliveries survived rollback")
	}
}

func TestSendCommitQueuesDelivery(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	ep := addEndpoint(t, owner, nil, false)
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(ctx, tx, hookpost.Message{OwnerID: owner, EventType: "order.created", Payload: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.MessageID, "msg_") || res.Duplicate {
		t.Fatalf("result %+v", res)
	}
	var status, endpoint string
	var attempts int
	if err := testPool.QueryRow(ctx, `
		SELECT status, endpoint_id, attempts FROM hookpost_deliveries WHERE message_id = $1`,
		res.MessageID).Scan(&status, &endpoint, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || endpoint != ep || attempts != 0 {
		t.Fatalf("delivery %s %s %d", status, endpoint, attempts)
	}
}

func TestSendWithPool(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	addEndpoint(t, owner, nil, false)
	res, err := c.Send(context.Background(), testPool, hookpost.Message{OwnerID: owner, EventType: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoints != 1 || deliveriesFor(t, res.MessageID) != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestSendDuplicateID(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	addEndpoint(t, owner, nil, false)
	ctx := context.Background()
	msg := hookpost.Message{ID: owner + "-order-1-created", OwnerID: owner, EventType: "order.created"}

	first, err := c.Send(ctx, testPool, msg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Send(ctx, testPool, msg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || first.Endpoints != 1 {
		t.Fatalf("first %+v", first)
	}
	if !second.Duplicate || second.Endpoints != 0 || second.MessageID != msg.ID {
		t.Fatalf("second %+v", second)
	}
	if n := deliveriesFor(t, msg.ID); n != 1 {
		t.Fatalf("%d deliveries, want 1", n)
	}
}

func TestSendEndpointMatching(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	all := addEndpoint(t, owner, nil, false)
	created := addEndpoint(t, owner, []string{"order.created", "order.paid"}, false)
	addEndpoint(t, owner, []string{"invoice.paid"}, false)
	addEndpoint(t, owner, nil, true)                  // disabled
	addEndpoint(t, owner+"_other", nil, false)        // other owner
	addEndpoint(t, owner, []string{"order.*"}, false) // no wildcards in v0.1

	res, err := c.Send(context.Background(), testPool,
		hookpost.Message{OwnerID: owner, EventType: "order.created"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := testPool.Query(context.Background(),
		`SELECT endpoint_id FROM hookpost_deliveries WHERE message_id = $1 ORDER BY endpoint_id`, res.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if res.Endpoints != 2 || len(got) != 2 || !got[all] || !got[created] {
		t.Fatalf("queued %d to %v, want %s and %s", res.Endpoints, got, all, created)
	}
}

func TestSendNoEndpointsStillStored(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	res, err := c.Send(context.Background(), testPool, hookpost.Message{OwnerID: owner, EventType: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoints != 0 || res.Duplicate {
		t.Fatalf("result %+v", res)
	}
	if n := countRows(t, `SELECT count(*) FROM hookpost_messages WHERE id = $1`, res.MessageID); n != 1 {
		t.Fatal("message not stored")
	}
}

func storedPayload(t *testing.T, id string) []byte {
	t.Helper()
	var b []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT payload FROM hookpost_messages WHERE id = $1`, id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSendEnvelope(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	before := time.Now().UTC().Add(-time.Second)
	res, err := c.Send(context.Background(), testPool, hookpost.Message{
		OwnerID: owner, EventType: "order.created", Payload: map[string]int{"id": 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(storedPayload(t, res.MessageID), &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "order.created" || string(env.Data) != `{"id":7}` ||
		env.Timestamp.Before(before) || env.Timestamp.After(time.Now().Add(time.Second)) {
		t.Fatalf("envelope %+v data %s", env, env.Data)
	}
}

func TestSendRawPayloadKeepsBytes(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{RawPayload: true})
	body := []byte(`{"z": 1,   "a": [true]}`)
	for _, p := range []any{body, json.RawMessage(body)} {
		res, err := c.Send(context.Background(), testPool, hookpost.Message{OwnerID: owner, EventType: "e", Payload: p})
		if err != nil {
			t.Fatal(err)
		}
		if got := storedPayload(t, res.MessageID); string(got) != string(body) {
			t.Fatalf("stored %q, want %q", got, body)
		}
	}
}

func TestSendValidation(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	ctx := context.Background()
	cases := []struct {
		msg  hookpost.Message
		want error
	}{
		{hookpost.Message{EventType: "e"}, hookpost.ErrNoOwner},
		{hookpost.Message{OwnerID: owner}, hookpost.ErrNoEventType},
		{hookpost.Message{OwnerID: owner, EventType: "e", Payload: []byte("{nope")}, hookpost.ErrBadPayload},
		{hookpost.Message{OwnerID: owner, EventType: "e", Payload: json.RawMessage("")}, hookpost.ErrBadPayload},
	}
	for _, tc := range cases {
		if _, err := c.Send(ctx, testPool, tc.msg); !errors.Is(err, tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.msg, err, tc.want)
		}
	}
	if _, err := c.Send(ctx, testPool, hookpost.Message{OwnerID: owner, EventType: "e", Payload: make(chan int)}); err == nil {
		t.Error("unencodable payload: want error")
	}
}

// NOTIFY inside a transaction must reach listeners only after commit.
func TestSendNotifiesOnCommit(t *testing.T) {
	c, owner := newSendClient(t, hookpost.Config{})
	addEndpoint(t, owner, nil, false)
	ctx := context.Background()

	listener := listen(t, testPool)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, tx, hookpost.Message{OwnerID: owner, EventType: "e"}); err != nil {
		t.Fatal(err)
	}
	if listener(200 * time.Millisecond) {
		t.Fatal("notified before commit")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !listener(2 * time.Second) {
		t.Fatal("no notification after commit")
	}
}

// listen LISTENs on the hookpost channel and returns a func that reports
// whether a notification arrives within d.
func listen(t *testing.T, pool *pgxpool.Pool) func(d time.Duration) bool {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Conn().Close(context.Background()) // don't return a LISTENing conn to the pool
		conn.Release()
	})
	if _, err := conn.Exec(context.Background(), "LISTEN hookpost"); err != nil {
		t.Fatal(err)
	}
	return func(d time.Duration) bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, err := conn.Conn().WaitForNotification(ctx)
		return err == nil
	}
}

func BenchmarkSend(b *testing.B) {
	if testPool == nil {
		b.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	c, err := hookpost.New(testPool, hookpost.Config{})
	if err != nil {
		b.Fatal(err)
	}
	if err := c.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	owner := "bench_" + time.Now().Format("150405.000000")
	for i := 0; i < 3; i++ {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO hookpost_endpoints (id, owner_id, url, secret)
			VALUES ($1, $2, 'https://example.com', 'whsec_dGVzdA==')`,
			owner+"_ep"+string(rune('a'+i)), owner); err != nil {
			b.Fatal(err)
		}
	}
	payload := map[string]any{"order_id": 123, "total": "9.99"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := c.Send(ctx, tx, hookpost.Message{OwnerID: owner, EventType: "order.created", Payload: payload}); err != nil {
			b.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
