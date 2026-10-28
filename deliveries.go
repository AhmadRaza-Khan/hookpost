package hookpost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MessageRecord is a stored message, as returned by ListMessages.
type MessageRecord struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"` // the exact body sent to receivers
	CreatedAt time.Time       `json:"created_at"`
}

// Delivery is the state of one message for one endpoint.
type Delivery struct {
	ID             int64     `json:"id"`
	MessageID      string    `json:"message_id"`
	EndpointID     string    `json:"endpoint_id"`
	Status         string    `json:"status"` // pending, succeeded or dead
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	LastStatusCode *int      `json:"last_status_code"`
	LastError      *string   `json:"last_error"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Attempt is one logged HTTP attempt of a delivery.
type Attempt struct {
	ID           int64     `json:"id"`
	Attempt      int       `json:"attempt"`
	StatusCode   *int      `json:"status_code"`
	Error        *string   `json:"error"`
	ResponseBody *string   `json:"response_body"`
	DurationMS   int       `json:"duration_ms"`
	CreatedAt    time.Time `json:"created_at"`
}

// MessageQuery filters ListMessages.
type MessageQuery struct {
	EventType string // optional exact match
	// Before is a message ID; only messages older than it are returned.
	// Pass the last ID of the previous page to get the next one.
	Before string
	Limit  int // default 50, max 250
}

// ListMessages returns ownerID's messages, newest first, using keyset
// pagination on (created_at, id).
func (c *Client) ListMessages(ctx context.Context, ownerID string, q MessageQuery) ([]MessageRecord, error) {
	q.Limit = clampLimit(q.Limit)
	rows, err := c.pool.Query(ctx, `
		SELECT id, event_type, payload, created_at FROM hookpost_messages
		WHERE owner_id = $1
		  AND ($2 = '' OR event_type = $2)
		  AND ($3 = '' OR (created_at, id) < (
		        SELECT created_at, id FROM hookpost_messages WHERE owner_id = $1 AND id = $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, ownerID, q.EventType, q.Before, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageRecord{}
	for rows.Next() {
		var m MessageRecord
		var payload []byte
		if err := rows.Scan(&m.ID, &m.EventType, &payload, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Payload = payload
		out = append(out, m)
	}
	return out, rows.Err()
}

// clampLimit applies MessageQuery.Limit's default and maximum.
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 250:
		return 250
	}
	return n
}

const deliveryCols = `d.id, d.message_id, d.endpoint_id, d.status, d.attempts,
	d.next_attempt_at, d.last_status_code, d.last_error, d.created_at, d.updated_at`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.MessageID, &d.EndpointID, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// GetDelivery returns one of ownerID's deliveries.
func (c *Client) GetDelivery(ctx context.Context, ownerID string, id int64) (Delivery, error) {
	return scanDelivery(c.pool.QueryRow(ctx, `
		SELECT `+deliveryCols+` FROM hookpost_deliveries d
		JOIN hookpost_endpoints e ON e.id = d.endpoint_id
		WHERE e.owner_id = $1 AND d.id = $2`, ownerID, id))
}

// ListDeliveries returns the per-endpoint deliveries of one of ownerID's
// messages, or ErrNotFound if the message doesn't exist.
func (c *Client) ListDeliveries(ctx context.Context, ownerID, messageID string) ([]Delivery, error) {
	var exists bool
	if err := c.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM hookpost_messages WHERE owner_id = $1 AND id = $2)`,
		ownerID, messageID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := c.pool.Query(ctx, `
		SELECT `+deliveryCols+` FROM hookpost_deliveries d
		WHERE d.message_id = $1
		ORDER BY d.id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListAttempts returns the attempt log of one of ownerID's deliveries, or
// ErrNotFound if the delivery doesn't exist.
func (c *Client) ListAttempts(ctx context.Context, ownerID string, deliveryID int64) ([]Attempt, error) {
	if _, err := c.GetDelivery(ctx, ownerID, deliveryID); err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, attempt, status_code, error, response_body, duration_ms, created_at
		FROM hookpost_attempts
		WHERE delivery_id = $1
		ORDER BY id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.ID, &a.Attempt, &a.StatusCode, &a.Error,
			&a.ResponseBody, &a.DurationMS, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RetryDelivery re-queues a delivery for immediate sending, including dead
// ones. The attempt counter is kept, so a dead delivery gets exactly one
// more try rather than a whole new retry schedule; use RecoverEndpoint or
// ResendMessage for a fresh schedule.
func (c *Client) RetryDelivery(ctx context.Context, ownerID string, deliveryID int64) error {
	tag, err := c.pool.Exec(ctx, `
		UPDATE hookpost_deliveries d
		SET status = 'pending', next_attempt_at = now(), updated_at = now()
		FROM hookpost_endpoints e
		WHERE d.id = $2 AND d.endpoint_id = e.id AND e.owner_id = $1`, ownerID, deliveryID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return notify(ctx, c.pool)
}

// RecoverEndpoint re-queues every dead delivery of an endpoint created at or
// after since (zero = all), each with a fresh retry schedule, e.g. after the
// customer fixed an outage. It returns how many deliveries were re-queued.
func (c *Client) RecoverEndpoint(ctx context.Context, ownerID, endpointID string, since time.Time) (int64, error) {
	if _, err := c.GetEndpoint(ctx, ownerID, endpointID); err != nil {
		return 0, err
	}
	tag, err := c.pool.Exec(ctx, `
		UPDATE hookpost_deliveries
		SET status = 'pending', next_attempt_at = now(), attempts = 0, updated_at = now()
		WHERE endpoint_id = $1 AND status = 'dead' AND created_at >= $2`, endpointID, since)
	if err != nil {
		return 0, err
	}
	if n := tag.RowsAffected(); n > 0 {
		return n, notify(ctx, c.pool)
	}
	return 0, nil
}

// ResendMessage (re)queues a message to one endpoint with a fresh retry
// schedule, e.g. to replay a past event to a newly added endpoint. Both must
// belong to ownerID.
func (c *Client) ResendMessage(ctx context.Context, ownerID, messageID, endpointID string) (Delivery, error) {
	d, err := scanDelivery(c.pool.QueryRow(ctx, `
		INSERT INTO hookpost_deliveries AS d (message_id, endpoint_id)
		SELECT m.id, e.id FROM hookpost_messages m, hookpost_endpoints e
		WHERE m.id = $2 AND e.id = $3 AND m.owner_id = $1 AND e.owner_id = $1
		ON CONFLICT (message_id, endpoint_id) DO UPDATE
		SET status = 'pending', next_attempt_at = now(), attempts = 0, updated_at = now()
		RETURNING `+deliveryCols, ownerID, messageID, endpointID))
	if err != nil {
		return Delivery{}, err
	}
	return d, notify(ctx, c.pool)
}
