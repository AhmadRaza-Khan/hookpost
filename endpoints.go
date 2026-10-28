package hookpost

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ahmadraza-khan/hookpost/internal/ids"
	"github.com/ahmadraza-khan/hookpost/signature"
)

// Endpoint is a URL an owner receives webhooks at.
type Endpoint struct {
	ID             string    `json:"id"`
	OwnerID        string    `json:"owner_id"`
	URL            string    `json:"url"`
	Description    string    `json:"description"`
	EventTypes     []string  `json:"event_types"` // empty = all events
	Disabled       bool      `json:"disabled"`
	DisabledReason string    `json:"disabled_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Secret is only populated by CreateEndpoint; use RotateSecret to issue
	// a new one.
	Secret string `json:"secret,omitempty"`
}

// EndpointInput is the input to CreateEndpoint.
type EndpointInput struct {
	OwnerID     string
	URL         string
	Description string
	EventTypes  []string // empty = all events (exact match only)
}

// EndpointUpdate is the input to UpdateEndpoint. Nil fields are unchanged.
type EndpointUpdate struct {
	URL         *string
	Description *string
	EventTypes  *[]string
}

// Errors returned by endpoint operations.
var (
	ErrNotFound   = errors.New("hookpost: not found")
	ErrInvalidURL = errors.New("hookpost: endpoint URL must be absolute https (or http when AllowPrivateNetworks)")
)

// DefaultGrace is how long the old secret keeps signing after a rotation
// when the admin API is not told otherwise.
const DefaultGrace = 24 * time.Hour

const endpointCols = `id, owner_id, url, description, event_types, disabled,
	COALESCE(disabled_reason, ''), created_at, updated_at`

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var ep Endpoint
	err := row.Scan(&ep.ID, &ep.OwnerID, &ep.URL, &ep.Description, &ep.EventTypes,
		&ep.Disabled, &ep.DisabledReason, &ep.CreatedAt, &ep.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ep, ErrNotFound
	}
	return ep, err
}

// validateURL accepts absolute https URLs (and http with
// AllowPrivateNetworks). Where the host resolves is checked at dial time.
func (c *Client) validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return ErrInvalidURL
	}
	if u.Scheme == "https" || (u.Scheme == "http" && c.cfg.AllowPrivateNetworks) {
		return nil
	}
	return ErrInvalidURL
}

// CreateEndpoint registers a new endpoint with a fresh signing secret. The
// returned Endpoint.Secret is the only time the secret is exposed; hand it
// to the customer.
func (c *Client) CreateEndpoint(ctx context.Context, in EndpointInput) (Endpoint, error) {
	if in.OwnerID == "" {
		return Endpoint{}, ErrNoOwner
	}
	if err := c.validateURL(in.URL); err != nil {
		return Endpoint{}, err
	}
	secret, err := signature.NewSecret()
	if err != nil {
		return Endpoint{}, err
	}
	if in.EventTypes == nil {
		in.EventTypes = []string{}
	}
	ep, err := scanEndpoint(c.pool.QueryRow(ctx, `
		INSERT INTO hookpost_endpoints (id, owner_id, url, description, secret, event_types)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+endpointCols,
		ids.New("ep"), in.OwnerID, in.URL, in.Description, secret, in.EventTypes))
	if err != nil {
		return Endpoint{}, fmt.Errorf("hookpost: create endpoint: %w", err)
	}
	ep.Secret = secret
	return ep, nil
}

// GetEndpoint returns one of ownerID's endpoints, without its secret.
func (c *Client) GetEndpoint(ctx context.Context, ownerID, id string) (Endpoint, error) {
	return scanEndpoint(c.pool.QueryRow(ctx, `
		SELECT `+endpointCols+` FROM hookpost_endpoints
		WHERE owner_id = $1 AND id = $2`, ownerID, id))
}

// ListEndpoints returns all of ownerID's endpoints, oldest first, without
// their secrets.
func (c *Client) ListEndpoints(ctx context.Context, ownerID string) ([]Endpoint, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT `+endpointCols+` FROM hookpost_endpoints
		WHERE owner_id = $1 ORDER BY created_at, id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		ep, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// UpdateEndpoint changes the non-nil fields of up.
func (c *Client) UpdateEndpoint(ctx context.Context, ownerID, id string, up EndpointUpdate) (Endpoint, error) {
	if up.URL != nil {
		if err := c.validateURL(*up.URL); err != nil {
			return Endpoint{}, err
		}
	}
	var eventTypes []string // nil = unchanged (NULL)
	if up.EventTypes != nil {
		eventTypes = *up.EventTypes
		if eventTypes == nil {
			eventTypes = []string{}
		}
	}
	return scanEndpoint(c.pool.QueryRow(ctx, `
		UPDATE hookpost_endpoints SET
			url         = COALESCE($3, url),
			description = COALESCE($4, description),
			event_types = COALESCE($5, event_types),
			updated_at  = now()
		WHERE owner_id = $1 AND id = $2
		RETURNING `+endpointCols,
		ownerID, id, up.URL, up.Description, eventTypes))
}

// SetEndpointDisabled disables or re-enables an endpoint. A disabled
// endpoint gets no new deliveries; ones already queued stay pending and are
// sent once it is enabled again.
func (c *Client) SetEndpointDisabled(ctx context.Context, ownerID, id string, disabled bool) error {
	tag, err := c.pool.Exec(ctx, `
		UPDATE hookpost_endpoints
		SET disabled = $3,
		    disabled_reason = CASE WHEN $3 THEN 'disabled manually' END,
		    updated_at = now()
		WHERE owner_id = $1 AND id = $2`, ownerID, id, disabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if !disabled {
		return notify(ctx, c.pool)
	}
	return nil
}

// DeleteEndpoint removes an endpoint along with its deliveries.
func (c *Client) DeleteEndpoint(ctx context.Context, ownerID, id string) error {
	tag, err := c.pool.Exec(ctx,
		`DELETE FROM hookpost_endpoints WHERE owner_id = $1 AND id = $2`, ownerID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateSecret issues and returns a new secret for an endpoint. For the
// grace period, requests are signed with both the new and the old secret,
// so the customer's existing verifier keeps working until they deploy the
// new one.
func (c *Client) RotateSecret(ctx context.Context, ownerID, id string, grace time.Duration) (string, error) {
	if grace < 0 {
		grace = 0
	}
	secret, err := signature.NewSecret()
	if err != nil {
		return "", err
	}
	tag, err := c.pool.Exec(ctx, `
		UPDATE hookpost_endpoints
		SET old_secret = secret,
		    old_secret_expires_at = now() + make_interval(secs => $4),
		    secret = $3,
		    updated_at = now()
		WHERE owner_id = $1 AND id = $2`, ownerID, id, secret, seconds(grace))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return secret, nil
}

// disableEndpoint is the dispatcher's unscoped disable (e.g. on 410 Gone).
func (c *Client) disableEndpoint(ctx context.Context, id, reason string) {
	_, err := c.pool.Exec(ctx, `
		UPDATE hookpost_endpoints
		SET disabled = true, disabled_reason = $2, updated_at = now()
		WHERE id = $1`, id, reason)
	logIfErr(c, "disable endpoint", err)
}
