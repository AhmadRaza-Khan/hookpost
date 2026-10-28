package hookpost_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmadraza-khan/hookpost"
)

func ExampleNew() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	wh, err := hookpost.New(pool, hookpost.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if err := wh.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
	go func() { _ = wh.Run(ctx) }() // background dispatcher
}

func ExampleClient_Send() {
	var (
		ctx  = context.Background()
		pool *pgxpool.Pool    // your pool
		wh   *hookpost.Client // from hookpost.New
	)

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ... your business writes on tx, e.g. INSERT INTO orders ...

	// Only delivered if the transaction commits.
	_, err = wh.Send(ctx, tx, hookpost.Message{
		ID:        "order-123-created", // optional: makes Send idempotent
		OwnerID:   "shop_42",
		EventType: "order.created",
		Payload:   map[string]any{"order_id": 123, "total": "9.99"},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}
}

func ExampleClient_CreateEndpoint() {
	var wh *hookpost.Client // from hookpost.New

	ep, err := wh.CreateEndpoint(context.Background(), hookpost.EndpointInput{
		OwnerID:    "shop_42",
		URL:        "https://example.com/webhooks",
		EventTypes: []string{"order.created", "order.paid"}, // empty = all events
	})
	if err != nil {
		log.Fatal(err)
	}
	// The secret is only returned here; give it to the customer.
	fmt.Println(ep.ID, ep.Secret)
}

func ExampleClient_AdminHandler() {
	var wh *hookpost.Client // from hookpost.New
	shopID := func(r *http.Request) string {
		return r.Header.Get("X-Shop-ID") // use your real session/auth here
	}

	mux := http.NewServeMux()
	mux.Handle("/api/webhooks/", http.StripPrefix("/api/webhooks",
		wh.AdminHandler(hookpost.AdminOptions{Owner: shopID})))
	log.Fatal(http.ListenAndServe(":8080", mux))
}
