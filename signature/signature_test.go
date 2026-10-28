package signature

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	now  = time.Unix(1700000000, 0)
	body = []byte(`{"a":1}`)
)

func newSecret(t *testing.T) string {
	t.Helper()
	s, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func headers(t *testing.T, ts time.Time, secrets ...string) http.Header {
	t.Helper()
	h, err := Headers("msg_1", ts, body, secrets...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Test vector published by the Standard Webhooks reference libraries.
// Passing it means we interoperate with every other language's verifier.
func TestSpecVector(t *testing.T) {
	const (
		secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		id     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
		want   = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	)
	ts := time.Unix(1614265330, 0)
	payload := []byte(`{"test": 2432232314}`)

	got, err := Sign(secret, id, ts, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}

	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, "1614265330")
	h.Set(HeaderSignature, want)
	if err := VerifyAt(secret, h, payload, ts, DefaultTolerance); err != nil {
		t.Fatalf("VerifyAt: %v", err)
	}
}

func TestNewSecret(t *testing.T) {
	s := newSecret(t)
	if !strings.HasPrefix(s, "whsec_") {
		t.Fatalf("missing prefix: %q", s)
	}
	key, err := decodeSecret(s)
	if err != nil || len(key) != 32 {
		t.Fatalf("decode: %v, len %d", err, len(key))
	}
	if newSecret(t) == s {
		t.Fatal("secrets repeat")
	}
}

func TestRoundTrip(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	if h.Get(HeaderID) != "msg_1" || h.Get(HeaderTimestamp) != "1700000000" {
		t.Fatalf("headers: %v", h)
	}
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSecretWithoutPrefix(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	if err := VerifyAt(strings.TrimPrefix(secret, "whsec_"), h, body, now, DefaultTolerance); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestWrongSecret(t *testing.T) {
	h := headers(t, now, newSecret(t))
	err := VerifyAt(newSecret(t), h, body, now, DefaultTolerance)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v, want ErrNoMatch", err)
	}
}

func TestBodyTampered(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	err := VerifyAt(secret, h, []byte(`{"a":2}`), now, DefaultTolerance)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v, want ErrNoMatch", err)
	}
}

func TestIDTampered(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	h.Set(HeaderID, "msg_2")
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v, want ErrNoMatch", err)
	}
}

func TestTimestampTolerance(t *testing.T) {
	secret := newSecret(t)
	for _, skew := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		h := headers(t, now.Add(skew), secret)
		err := VerifyAt(secret, h, body, now, DefaultTolerance)
		if !errors.Is(err, ErrTimestampTooOld) {
			t.Fatalf("skew %v: got %v, want ErrTimestampTooOld", skew, err)
		}
	}
	for _, skew := range []time.Duration{-4 * time.Minute, 4 * time.Minute} {
		h := headers(t, now.Add(skew), secret)
		if err := VerifyAt(secret, h, body, now, DefaultTolerance); err != nil {
			t.Fatalf("skew %v: %v", skew, err)
		}
	}
}

func TestTimestampChangedAfterSigning(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix()+1, 10))
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v, want ErrNoMatch", err)
	}
}

func TestInvalidTimestamp(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	h.Set(HeaderTimestamp, "yesterday")
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("got %v, want ErrInvalidTimestamp", err)
	}
}

func TestRotationSecondSignatureValid(t *testing.T) {
	oldSecret, newSec := newSecret(t), newSecret(t)
	h := headers(t, now, newSec, oldSecret)
	if n := len(strings.Fields(h.Get(HeaderSignature))); n != 2 {
		t.Fatalf("want 2 signatures, got %d", n)
	}
	for _, s := range []string{oldSecret, newSec} {
		if err := VerifyAt(s, h, body, now, DefaultTolerance); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
}

func TestUnknownVersionIgnored(t *testing.T) {
	secret := newSecret(t)
	h := headers(t, now, secret)
	v1 := h.Get(HeaderSignature)
	sig := strings.TrimPrefix(v1, "v1,")

	h.Set(HeaderSignature, "v2,"+sig)
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("v2 only: got %v, want ErrNoMatch", err)
	}
	h.Set(HeaderSignature, "v2,"+sig+" garbage v1,!!!notbase64 "+v1)
	if err := VerifyAt(secret, h, body, now, DefaultTolerance); err != nil {
		t.Fatalf("mixed: %v", err)
	}
}

func TestMissingHeaders(t *testing.T) {
	secret := newSecret(t)
	for _, name := range []string{HeaderID, HeaderTimestamp, HeaderSignature} {
		h := headers(t, now, secret)
		h.Del(name)
		if err := VerifyAt(secret, h, body, now, DefaultTolerance); !errors.Is(err, ErrMissingHeaders) {
			t.Fatalf("without %s: got %v, want ErrMissingHeaders", name, err)
		}
	}
}

func TestInvalidSecret(t *testing.T) {
	if _, err := Sign("whsec_!!!", "msg_1", now, body); !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("Sign: got %v", err)
	}
	if _, err := Headers("msg_1", now, body); !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("Headers with no secrets: got %v", err)
	}
	h := headers(t, now, newSecret(t))
	if err := VerifyAt("whsec_", h, body, now, DefaultTolerance); !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("VerifyAt: got %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	secret := newSecret(t)
	var got []byte
	srv := Middleware(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	h, err := Headers("msg_1", time.Now(), body, secret)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(string(body)))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || string(got) != string(body) {
		t.Fatalf("valid request: code %d, body %q", rec.Code, got)
	}

	req = httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"a":2}`))
	for k, v := range h {
		req.Header[k] = v
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered request: code %d, want 401", rec.Code)
	}
}

func FuzzVerify(f *testing.F) {
	f.Add("msg_1", "1700000000", "v1,abc", []byte("{}"))
	f.Add("", "", "", []byte(nil))
	f.Add("msg_1", "-1", "v1, v1,,, v2", []byte("x"))
	f.Fuzz(func(t *testing.T, id, ts, sig string, body []byte) {
		h := http.Header{}
		h.Set(HeaderID, id)
		h.Set(HeaderTimestamp, ts)
		h.Set(HeaderSignature, sig)
		_ = Verify("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", h, body) // must never panic
	})
}
