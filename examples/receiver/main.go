// Command receiver is a minimal webhook receiver that verifies Standard
// Webhooks signatures with hookpost/signature.
//
//	WEBHOOK_SECRET=whsec_... go run ./examples/receiver
package main

import (
	"io"
	"log"
	"net/http"
	"os"

	"github.com/ahmadraza-khan/hookpost/signature"
)

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("set WEBHOOK_SECRET to the secret printed by examples/sender")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9000"
	}

	http.Handle("/hooks", signature.Middleware(secret,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) // already verified by Middleware
			// Deliveries are at-least-once: a real receiver dedupes on webhook-id.
			log.Printf("%s %s", r.Header.Get(signature.HeaderID), body)
			w.WriteHeader(http.StatusNoContent)
		})))

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil)) //nolint:gosec // demo server, no timeouts needed
}
