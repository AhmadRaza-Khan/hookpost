package hookpost

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Version is the library version, used in the default User-Agent.
const Version = "0.1.0"

// notifyChannel is the Postgres LISTEN/NOTIFY channel that wakes dispatchers.
const notifyChannel = "hookpost"

// DBTX is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx, so Send can run
// inside the caller's own transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config controls delivery behaviour. Zero values get sensible defaults.
// Config is copied by New; changing it afterwards has no effect.
type Config struct {
	// Workers is the number of concurrent deliveries per process. Default 10.
	Workers int
	// PollInterval is how often idle workers check for due deliveries. Default 1s.
	PollInterval time.Duration
	// BatchSize is the most deliveries leased per poll. Default = Workers.
	BatchSize int
	// Timeout is the per-request HTTP timeout. Default 15s.
	Timeout time.Duration
	// Schedule is the retry delays after each failed attempt, so
	// len(Schedule)+1 is the maximum number of attempts. Default DefaultSchedule.
	// An empty, non-nil slice means a single attempt with no retries.
	Schedule []time.Duration
	// MaxResponseBytes is how much of each response body is stored. Default 4096.
	MaxResponseBytes int64
	// UserAgent header sent with every request. Default "hookpost/<Version>".
	UserAgent string
	// RawPayload sends the payload as-is instead of wrapping it in the
	// {"type","timestamp","data"} envelope.
	RawPayload bool
	// AllowPrivateNetworks disables the SSRF guard and allows http:// URLs.
	// Only for local development and tests.
	AllowPrivateNetworks bool
	// HTTPClient replaces the built-in client entirely; you then own SSRF safety.
	HTTPClient *http.Client
	// DisableListen turns off LISTEN/NOTIFY wake-ups (e.g. behind PgBouncer in
	// transaction mode). Deliveries are then picked up on the next poll.
	DisableListen bool
	// Retention is how long finished messages are kept before Run prunes
	// them. Default 30 days; negative disables pruning.
	Retention time.Duration
	// Logger for dispatcher errors. Default slog.Default().
	Logger *slog.Logger
	// OnAttempt is called after every delivery attempt (e.g. for metrics).
	// It runs on the delivery goroutine, so keep it fast.
	OnAttempt func(AttemptInfo)
}

// AttemptInfo describes one delivery attempt and is passed to Config.OnAttempt.
type AttemptInfo struct {
	DeliveryID int64
	MessageID  string
	EndpointID string
	EventType  string
	Attempt    int
	StatusCode int // 0 when no response was received
	Err        error
	Duration   time.Duration
	// Dead is true when this attempt failed and no retries remain.
	Dead bool
}

// Client sends webhooks. It is safe for concurrent use; create one per process.
type Client struct {
	pool *pgxpool.Pool
	cfg  Config
	http *http.Client
	log  *slog.Logger
}

// ErrNoPool is returned by New when pool is nil.
var ErrNoPool = errors.New("hookpost: nil pool")

// New builds a Client. It does not start the dispatcher; call Run for that.
// The caller owns pool and is responsible for closing it.
func New(pool *pgxpool.Pool, cfg Config) (*Client, error) {
	if pool == nil {
		return nil, ErrNoPool
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 10
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = cfg.Workers
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Schedule == nil {
		cfg.Schedule = DefaultSchedule
	}
	// own copy, so later edits to the caller's slice (or DefaultSchedule) don't leak in
	cfg.Schedule = append([]time.Duration{}, cfg.Schedule...)
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 4096
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "hookpost/" + Version
	}
	if cfg.Retention == 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = newHTTPClient(cfg.Timeout, cfg.AllowPrivateNetworks)
	}
	return &Client{pool: pool, cfg: cfg, http: hc, log: cfg.Logger}, nil
}

// notify wakes listening dispatchers. Inside a transaction Postgres delivers
// it only on commit, which is exactly when the work becomes visible.
func notify(ctx context.Context, db DBTX) error {
	_, err := db.Exec(ctx, `SELECT pg_notify($1, '')`, notifyChannel)
	return err
}
