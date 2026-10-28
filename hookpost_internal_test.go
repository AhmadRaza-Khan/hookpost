package hookpost

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New never touches the pool, so an unconnected one is enough here.
func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(&pgxpool.Pool{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewDefaults(t *testing.T) {
	c := newTestClient(t, Config{})
	cfg := c.cfg
	if cfg.Workers != 10 || cfg.BatchSize != 10 {
		t.Errorf("workers %d batch %d, want 10/10", cfg.Workers, cfg.BatchSize)
	}
	if cfg.PollInterval != time.Second || cfg.Timeout != 15*time.Second {
		t.Errorf("poll %v timeout %v", cfg.PollInterval, cfg.Timeout)
	}
	if len(cfg.Schedule) != len(DefaultSchedule) {
		t.Errorf("schedule len %d", len(cfg.Schedule))
	}
	if cfg.MaxResponseBytes != 4096 || cfg.UserAgent != "hookpost/"+Version {
		t.Errorf("max %d ua %q", cfg.MaxResponseBytes, cfg.UserAgent)
	}
	if c.log == nil || c.http == nil || c.http.Timeout != 15*time.Second {
		t.Errorf("logger/http client not set")
	}
}

func TestNewKeepsExplicitValues(t *testing.T) {
	hc := &http.Client{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newTestClient(t, Config{
		Workers: 3, BatchSize: 7, PollInterval: time.Minute, Timeout: time.Second,
		MaxResponseBytes: 10, UserAgent: "x", HTTPClient: hc, Logger: logger,
	})
	if c.cfg.Workers != 3 || c.cfg.BatchSize != 7 || c.cfg.PollInterval != time.Minute ||
		c.cfg.Timeout != time.Second || c.cfg.MaxResponseBytes != 10 || c.cfg.UserAgent != "x" {
		t.Errorf("explicit values overwritten: %+v", c.cfg)
	}
	if c.http != hc || c.log != logger {
		t.Error("HTTPClient/Logger not used")
	}
}

func TestBatchSizeDefaultsToWorkers(t *testing.T) {
	if c := newTestClient(t, Config{Workers: 4}); c.cfg.BatchSize != 4 {
		t.Fatalf("batch %d, want 4", c.cfg.BatchSize)
	}
}

func TestScheduleIsCopied(t *testing.T) {
	s := []time.Duration{time.Second, time.Minute}
	c := newTestClient(t, Config{Schedule: s})
	s[0] = time.Hour
	if c.cfg.Schedule[0] != time.Second {
		t.Fatal("Client shares the caller's Schedule slice")
	}
}

func TestDefaultHTTPClientDoesNotFollowRedirects(t *testing.T) {
	c := newTestClient(t, Config{})
	if err := c.http.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v", err)
	}
}

func TestNextDelay(t *testing.T) {
	c := newTestClient(t, Config{Schedule: []time.Duration{10 * time.Second, time.Minute}})
	for i := 0; i < 200; i++ {
		d, ok := c.nextDelay(1)
		if !ok || d < 9*time.Second || d > 11*time.Second {
			t.Fatalf("attempt 1: %v %v, want 10s ±10%%", d, ok)
		}
		d, ok = c.nextDelay(2)
		if !ok || d < 54*time.Second || d > 66*time.Second {
			t.Fatalf("attempt 2: %v %v, want 1m ±10%%", d, ok)
		}
	}
	for _, attempt := range []int{0, 3, 100} {
		if _, ok := c.nextDelay(attempt); ok {
			t.Fatalf("attempt %d: want exhausted", attempt)
		}
	}
}

func TestEmptyScheduleMeansNoRetries(t *testing.T) {
	c := newTestClient(t, Config{Schedule: []time.Duration{}})
	if _, ok := c.nextDelay(1); ok {
		t.Fatal("want no retries")
	}
}
