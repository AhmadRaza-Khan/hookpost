package signature_test

import (
	"io"
	"log"
	"net/http"
	"os"

	"github.com/ahmadraza-khan/hookpost/signature"
)

func ExampleVerify() {
	secret := os.Getenv("WEBHOOK_SECRET") // "whsec_..."
	http.HandleFunc("/hooks", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := signature.Verify(secret, r.Header, body); err != nil {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		// Deliveries are at-least-once: dedupe on the webhook-id header.
		log.Printf("event %s: %s", r.Header.Get(signature.HeaderID), body)
		w.WriteHeader(http.StatusNoContent)
	})
}

func ExampleMiddleware() {
	secret := os.Getenv("WEBHOOK_SECRET")
	http.Handle("/hooks", signature.Middleware(secret,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) // already verified
			log.Printf("%s %s", r.Header.Get(signature.HeaderID), body)
			w.WriteHeader(http.StatusNoContent)
		})))
}
