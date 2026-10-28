package hookpost

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Client sends webhooks. It is safe for concurrent use; create one per process.
type Client struct {
	pool *pgxpool.Pool
}

// ErrNoPool is returned by New when pool is nil.
var ErrNoPool = errors.New("hookpost: nil pool")

// New builds a Client. The caller owns pool and is responsible for closing it.
func New(pool *pgxpool.Pool) (*Client, error) {
	if pool == nil {
		return nil, ErrNoPool
	}
	return &Client{pool: pool}, nil
}
