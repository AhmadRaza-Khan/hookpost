// Package signature signs and verifies webhooks per the Standard Webhooks
// specification (https://www.standardwebhooks.com).
//
// It has no dependencies outside the standard library, so webhook receivers
// can import it without pulling in a database driver:
//
//	http.Handle("/hooks", signature.Middleware(secret, myHandler))
package signature

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names defined by the Standard Webhooks specification.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
	secretPrefix    = "whsec_"
)

// DefaultTolerance is the maximum allowed clock skew when verifying.
const DefaultTolerance = 5 * time.Minute

// MaxBodyBytes is the largest request body Middleware will read.
const MaxBodyBytes = 5 << 20

// Errors returned by Verify and Sign.
var (
	ErrMissingHeaders   = errors.New("signature: missing webhook headers")
	ErrInvalidTimestamp = errors.New("signature: invalid timestamp")
	ErrTimestampTooOld  = errors.New("signature: timestamp outside tolerance")
	ErrNoMatch          = errors.New("signature: no matching signature")
	ErrInvalidSecret    = errors.New("signature: invalid secret")
)

// NewSecret returns a fresh "whsec_..." secret with 32 random bytes.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

func decodeSecret(secret string) ([]byte, error) {
	s := strings.TrimPrefix(secret, secretPrefix)
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, ErrInvalidSecret
	}
	return key, nil
}

func sign(key []byte, msgID string, ts int64, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID))
	mac.Write([]byte{'.'})
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return mac.Sum(nil)
}

// Sign returns the "v1,<base64>" signature of body for one secret.
func Sign(secret, msgID string, ts time.Time, body []byte) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return "v1," + base64.StdEncoding.EncodeToString(sign(key, msgID, ts.Unix(), body)), nil
}

// Headers builds the three Standard Webhooks headers, signing with every
// given secret. Pass both the new and the old secret during rotation.
func Headers(msgID string, ts time.Time, body []byte, secrets ...string) (http.Header, error) {
	if len(secrets) == 0 {
		return nil, ErrInvalidSecret
	}
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Sign(s, msgID, ts, body)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, sig)
	}
	h := http.Header{}
	h.Set(HeaderID, msgID)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	h.Set(HeaderSignature, strings.Join(sigs, " "))
	return h, nil
}

// Verify checks the headers and raw body against secret, allowing
// DefaultTolerance of clock skew.
func Verify(secret string, h http.Header, body []byte) error {
	return VerifyAt(secret, h, body, time.Now(), DefaultTolerance)
}

// VerifyAt is Verify with an explicit clock and tolerance, for tests.
func VerifyAt(secret string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	id, tsRaw, sigRaw := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || tsRaw == "" || sigRaw == "" {
		return ErrMissingHeaders
	}
	sec, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}
	ts := time.Unix(sec, 0)
	if now.Sub(ts) > tolerance || ts.Sub(now) > tolerance {
		return ErrTimestampTooOld
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return err
	}
	want := sign(key, id, sec, body)
	for _, part := range strings.Fields(sigRaw) {
		version, sig, ok := strings.Cut(part, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrNoMatch
}

// Middleware verifies each request against secret before calling next,
// replying 401 when the signature is invalid. The body is restored so next
// can read it.
func Middleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := Verify(secret, r.Header, body); err != nil {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
