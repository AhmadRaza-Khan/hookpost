package hookpost_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmadraza-khan/hookpost"
	"github.com/ahmadraza-khan/hookpost/signature"
)

const testOwner = "shop_1"

// newDispatchClient empties all hookpost tables and returns a client tuned
// for fast tests: 20ms polls, two 10ms retries, no pruning.
func newDispatchClient(t *testing.T, cfg hookpost.Config) *hookpost.Client {
	t.Helper()
	pool := needDB(t)
	cfg.AllowPrivateNetworks = true // httptest servers listen on 127.0.0.1
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 20 * time.Millisecond
	}
	if cfg.Schedule == nil {
		cfg.Schedule = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	}
	if cfg.Retention == 0 {
		cfg.Retention = -1
	}
	c, err := hookpost.New(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE hookpost_endpoints, hookpost_messages,
		hookpost_deliveries, hookpost_attempts CASCADE`); err != nil {
		t.Fatal(err)
	}
	return c
}

// run starts c.Run and stops it (waiting for it to return) at test cleanup.
func run(t *testing.T, c *hookpost.Client) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// receiver is a test webhook endpoint that records what it gets.
type receiver struct {
	t      *testing.T
	secret string
	// status picks the response code for the n-th request (1-based). Default 200.
	status func(n int) int
	// before runs before replying, e.g. to block or sleep.
	before func(n int, r *http.Request)
	reply  string

	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
	badSigs int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	if err := signature.Verify(rc.secret, r.Header, body); err != nil {
		rc.badSigs++
	}
	rc.headers = append(rc.headers, r.Header.Clone())
	rc.bodies = append(rc.bodies, body)
	n := len(rc.headers)
	rc.mu.Unlock()

	if rc.before != nil {
		rc.before(n, r)
	}
	code := http.StatusOK
	if rc.status != nil {
		code = rc.status(n)
	}
	if code >= 300 && code < 400 {
		w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, rc.reply)
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.headers)
}

// serve starts rc and registers an endpoint for it, returning the endpoint ID.
func serve(t *testing.T, rc *receiver) string {
	t.Helper()
	secret, err := signature.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	rc.t, rc.secret = t, secret
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)

	id := "ep_" + strings.ReplaceAll(srv.Listener.Addr().String(), ":", "_")
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO hookpost_endpoints (id, owner_id, url, secret)
		VALUES ($1, $2, $3, $4)`, id, testOwner, srv.URL+"/hooks", secret); err != nil {
		t.Fatal(err)
	}
	return id
}

func send(t *testing.T, c *hookpost.Client, payload any) string {
	t.Helper()
	res, err := c.Send(context.Background(), testPool,
		hookpost.Message{OwnerID: testOwner, EventType: "order.created", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return res.MessageID
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type deliveryRow struct {
	status    string
	attempts  int
	lastCode  *int
	lastError *string
}

func delivery(t *testing.T, msgID string) deliveryRow {
	t.Helper()
	var d deliveryRow
	if err := testPool.QueryRow(context.Background(), `
		SELECT status, attempts, last_status_code, last_error
		FROM hookpost_deliveries WHERE message_id = $1`, msgID).
		Scan(&d.status, &d.attempts, &d.lastCode, &d.lastError); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitStatus(t *testing.T, msgID, status string) deliveryRow {
	t.Helper()
	var d deliveryRow
	waitFor(t, "delivery "+status, func() bool { d = delivery(t, msgID); return d.status == status })
	return d
}

func TestDeliverSignedRequest(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	rc := &receiver{}
	serve(t, rc)
	run(t, c)

	msgID := send(t, c, map[string]int{"id": 1})
	d := waitStatus(t, msgID, "succeeded")
	if d.attempts != 1 || d.lastCode == nil || *d.lastCode != 200 || d.lastError != nil {
		t.Fatalf("delivery %+v", d)
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.badSigs != 0 {
		t.Fatal("signature did not verify")
	}
	h := rc.headers[0]
	if h.Get("webhook-id") != msgID || h.Get("Content-Type") != "application/json" ||
		h.Get("User-Agent") != "hookpost/"+hookpost.Version {
		t.Fatalf("headers %v", h)
	}
	var stored []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT payload FROM hookpost_messages WHERE id = $1`, msgID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(rc.bodies[0]) != string(stored) {
		t.Fatalf("body %q, stored %q", rc.bodies[0], stored)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	rc := &receiver{status: func(n int) int {
		if n < 3 {
			return 500
		}
		return 200
	}}
	serve(t, rc)
	run(t, c)

	msgID := send(t, c, 1)
	if d := waitStatus(t, msgID, "succeeded"); d.attempts != 3 {
		t.Fatalf("attempts %d, want 3", d.attempts)
	}
	rc.mu.Lock()
	for i, h := range rc.headers {
		if h.Get("webhook-id") != msgID {
			t.Errorf("request %d webhook-id %q, want %q", i, h.Get("webhook-id"), msgID)
		}
	}
	rc.mu.Unlock()
	if n := countRows(t, `SELECT count(*) FROM hookpost_attempts`); n != 3 {
		t.Fatalf("%d attempt rows, want 3", n)
	}
}

func TestDeadAfterScheduleExhausted(t *testing.T) {
	var mu sync.Mutex
	var infos []hookpost.AttemptInfo
	c := newDispatchClient(t, hookpost.Config{OnAttempt: func(a hookpost.AttemptInfo) {
		mu.Lock()
		infos = append(infos, a)
		mu.Unlock()
	}})
	rc := &receiver{status: func(int) int { return 500 }}
	ep := serve(t, rc)
	run(t, c)

	msgID := send(t, c, 1)
	d := waitStatus(t, msgID, "dead")
	if d.attempts != 3 || d.lastError == nil || *d.lastError != "HTTP 500" {
		t.Fatalf("delivery %+v", d)
	}
	time.Sleep(100 * time.Millisecond)
	if rc.count() != 3 {
		t.Fatalf("receiver got %d requests, want 3", rc.count())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(infos) != 3 {
		t.Fatalf("OnAttempt called %d times, want 3", len(infos))
	}
	for i, a := range infos {
		if a.Attempt != i+1 || a.StatusCode != 500 || a.MessageID != msgID ||
			a.EndpointID != ep || a.EventType != "order.created" || a.Dead != (i == 2) {
			t.Errorf("attempt %d: %+v", i+1, a)
		}
	}
}

func TestGoneDisablesEndpoint(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ep := serve(t, &receiver{status: func(int) int { return http.StatusGone }})
	run(t, c)

	msgID := send(t, c, 1)
	if d := waitStatus(t, msgID, "dead"); d.attempts != 1 {
		t.Fatalf("attempts %d, want 1", d.attempts)
	}
	var disabled bool
	var reason string
	if err := testPool.QueryRow(context.Background(),
		`SELECT disabled, disabled_reason FROM hookpost_endpoints WHERE id = $1`, ep).
		Scan(&disabled, &reason); err != nil {
		t.Fatal(err)
	}
	if !disabled || !strings.Contains(reason, "410") {
		t.Fatalf("disabled %v reason %q", disabled, reason)
	}
}

func TestTimeoutIsRetried(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{Timeout: 100 * time.Millisecond})
	rc := &receiver{before: func(n int, r *http.Request) {
		if n == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
	}}
	serve(t, rc)
	run(t, c)

	msgID := send(t, c, 1)
	if d := waitStatus(t, msgID, "succeeded"); d.attempts != 2 {
		t.Fatalf("attempts %d, want 2", d.attempts)
	}
	var errText *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT error FROM hookpost_attempts WHERE attempt = 1`).Scan(&errText); err != nil {
		t.Fatal(err)
	}
	if errText == nil || *errText == "" {
		t.Fatal("first attempt recorded no error")
	}
}

func TestRedirectIsFailure(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{Schedule: []time.Duration{}})
	serve(t, &receiver{status: func(int) int { return http.StatusFound }})
	run(t, c)

	msgID := send(t, c, 1)
	if d := waitStatus(t, msgID, "dead"); d.lastCode == nil || *d.lastCode != 302 {
		t.Fatalf("delivery %+v", d)
	}
}

func TestResponseBodyTruncated(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{MaxResponseBytes: 10})
	serve(t, &receiver{reply: strings.Repeat("x", 100)})
	run(t, c)

	msgID := send(t, c, 1)
	waitStatus(t, msgID, "succeeded")
	var body string
	if err := testPool.QueryRow(context.Background(),
		`SELECT response_body FROM hookpost_attempts`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != strings.Repeat("x", 10) {
		t.Fatalf("stored %q", body)
	}
}

func TestTwoDispatchersDeliverOnce(t *testing.T) {
	c1 := newDispatchClient(t, hookpost.Config{Workers: 5})
	c2, err := hookpost.New(testPool, hookpost.Config{
		Workers: 5, PollInterval: 20 * time.Millisecond, Retention: -1, AllowPrivateNetworks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{}
	serve(t, rc)

	const n = 200
	for i := 0; i < n; i++ {
		send(t, c1, i)
	}
	run(t, c1)
	run(t, c2)

	waitFor(t, "all succeeded", func() bool {
		return countRows(t, `SELECT count(*) FROM hookpost_deliveries WHERE status = 'succeeded'`) == n
	})
	rc.mu.Lock()
	defer rc.mu.Unlock()
	seen := map[string]int{}
	for _, h := range rc.headers {
		seen[h.Get("webhook-id")]++
	}
	if len(rc.headers) != n || len(seen) != n {
		t.Fatalf("%d requests for %d distinct messages, want %d each", len(rc.headers), len(seen), n)
	}
}

func TestShutdownFinishesInFlight(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	started, release := make(chan struct{}), make(chan struct{})
	serve(t, &receiver{before: func(int, *http.Request) {
		close(started)
		<-release
	}})
	stop := run(t, c)

	msgID := send(t, c, 1)
	<-started
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("Run returned while a request was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-stopped
	if d := delivery(t, msgID); d.status != "succeeded" {
		t.Fatalf("status %q after shutdown, want succeeded", d.status)
	}
}

func TestListenWakesDispatcher(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{PollInterval: time.Hour})
	serve(t, &receiver{})
	run(t, c)
	time.Sleep(200 * time.Millisecond) // let the first poll pass and LISTEN start

	msgID := send(t, c, 1)
	waitStatus(t, msgID, "succeeded")
}

func TestDisableListenStillPolls(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{DisableListen: true, PollInterval: 50 * time.Millisecond})
	serve(t, &receiver{})
	run(t, c)
	time.Sleep(100 * time.Millisecond)

	msgID := send(t, c, 1)
	waitStatus(t, msgID, "succeeded")
}

func TestDisabledEndpointPausesDeliveries(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	rc := &receiver{}
	ep := serve(t, rc)
	msgID := send(t, c, 1)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE hookpost_endpoints SET disabled = true WHERE id = $1`, ep); err != nil {
		t.Fatal(err)
	}
	run(t, c)

	time.Sleep(200 * time.Millisecond)
	if rc.count() != 0 || delivery(t, msgID).status != "pending" {
		t.Fatal("delivered to a disabled endpoint")
	}
	if _, err := testPool.Exec(context.Background(),
		`UPDATE hookpost_endpoints SET disabled = false WHERE id = $1`, ep); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, msgID, "succeeded")
}

func TestEndpointDeleteCascades(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	ep := serve(t, &receiver{})
	msgID := send(t, c, 1)
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM hookpost_endpoints WHERE id = $1`, ep); err != nil {
		t.Fatal(err)
	}
	if n := deliveriesFor(t, msgID); n != 0 {
		t.Fatalf("%d deliveries left", n)
	}
}

func TestPrune(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	serve(t, &receiver{})
	done, pending, fresh := send(t, c, 1), send(t, c, 2), send(t, c, 3)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE hookpost_messages SET created_at = now() - interval '2 hours'
		WHERE id = ANY($1)`, []string{done, pending}); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE hookpost_deliveries SET status = 'succeeded'
		WHERE message_id = ANY($1)`, []string{done, fresh}); err != nil {
		t.Fatal(err)
	}

	n, err := c.Prune(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for id, want := range map[string]int{done: 0, pending: 1, fresh: 1} {
		if got := countRows(t, `SELECT count(*) FROM hookpost_messages WHERE id = $1`, id); got != want {
			t.Errorf("message %s: %d rows, want %d", id, got, want)
		}
	}
}
