// Command sender is a runnable hookpost demo: it registers an endpoint for
// a local receiver and sends one "order.created" event per second.
//
//	export DATABASE_URL=postgres://postgres:postgres@localhost:5432/hookpost?sslmode=disable
//	go run ./examples/sender
//
// Then start examples/receiver with the printed secret. Deliveries made
// before the receiver is up fail and are retried, so you can watch retries
// happen: stop the receiver, wait, start it again.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmadraza-khan/hookpost"
)

const owner = "demo_shop"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	wh, err := hookpost.New(pool, hookpost.Config{
		// NEVER set this in production: it allows http:// and localhost.
		AllowPrivateNetworks: true,
		// Short retries so the demo is lively.
		Schedule: []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second},
		OnAttempt: func(a hookpost.AttemptInfo) {
			log.Printf("attempt %d for %s: status=%d err=%v dead=%v",
				a.Attempt, a.MessageID, a.StatusCode, a.Err, a.Dead)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := wh.Migrate(ctx); err != nil {
		log.Fatal(err)
	}

	url := os.Getenv("RECEIVER_URL")
	if url == "" {
		url = "http://localhost:9000/hooks"
	}
	// Start fresh each run so the printed secret is the one in use.
	eps, err := wh.ListEndpoints(ctx, owner)
	if err != nil {
		log.Fatal(err)
	}
	for _, ep := range eps {
		_ = wh.DeleteEndpoint(ctx, owner, ep.ID)
	}
	ep, err := wh.CreateEndpoint(ctx, hookpost.EndpointInput{OwnerID: owner, URL: url})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("endpoint %s -> %s", ep.ID, url)
	log.Printf("start the receiver with:\n\n  WEBHOOK_SECRET=%s go run ./examples/receiver\n", ep.Secret)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := wh.Run(ctx); err != nil {
			log.Print(err)
		}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			<-done // let in-flight deliveries finish
			return
		case <-tick.C:
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			log.Print(err)
			continue
		}
		// In a real app your business writes happen on tx too.
		res, err := wh.Send(ctx, tx, hookpost.Message{
			OwnerID: owner, EventType: "order.created",
			Payload: map[string]any{"order_id": n, "total": "9.99"},
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			log.Print(err)
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			log.Print(err)
			continue
		}
		log.Printf("sent %s (order %d)", res.MessageID, n)
	}
}
