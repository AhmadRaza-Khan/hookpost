package hookpost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmadraza-khan/hookpost/internal/ids"
)

// Message is one event to deliver to every matching endpoint of OwnerID.
type Message struct {
	// ID is optional. Set it to make Send idempotent (e.g. "order-123-created"):
	// sending the same ID twice is a no-op the second time. It becomes the
	// webhook-id header, so it must be unique across all owners.
	ID string
	// OwnerID selects whose endpoints receive the message. Required.
	OwnerID string
	// EventType, e.g. "order.created". Required.
	EventType string
	// Payload is JSON-encoded unless it is already []byte or json.RawMessage,
	// in which case it must be valid JSON and is sent byte for byte.
	Payload any
}

// Errors returned by Send.
var (
	ErrNoOwner     = errors.New("hookpost: OwnerID is required")
	ErrNoEventType = errors.New("hookpost: EventType is required")
	ErrBadPayload  = errors.New("hookpost: payload is not valid JSON")
)

// SendResult reports what Send did.
type SendResult struct {
	MessageID string
	// Endpoints is how many deliveries were queued.
	Endpoints int
	// Duplicate is true when a message with this ID already existed and
	// nothing new was queued.
	Duplicate bool
}

// envelope is the request body receivers get unless Config.RawPayload is set.
type envelope struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// Send records msg and queues one delivery per matching, enabled endpoint.
// It never does HTTP; the dispatcher started by Run delivers later.
//
// Pass a pgx.Tx to make the webhook part of your transaction: it is only
// sent if the transaction commits. Pass the pool for fire-and-forget.
//
// An endpoint matches when it belongs to OwnerID, is enabled, and lists
// EventType (exact match) or no event types at all. A message with no
// matching endpoints is still stored (Endpoints == 0) so it can be replayed
// to an endpoint added later.
func (c *Client) Send(ctx context.Context, db DBTX, msg Message) (SendResult, error) {
	if msg.OwnerID == "" {
		return SendResult{}, ErrNoOwner
	}
	if msg.EventType == "" {
		return SendResult{}, ErrNoEventType
	}
	body, err := encodePayload(msg.Payload)
	if err != nil {
		return SendResult{}, err
	}
	if !c.cfg.RawPayload {
		// Wrap before storing, so the stored bytes are exactly the request body.
		body, err = json.Marshal(envelope{msg.EventType, time.Now().UTC(), body})
		if err != nil {
			return SendResult{}, fmt.Errorf("hookpost: encode envelope: %w", err)
		}
	}
	id := msg.ID
	if id == "" {
		id = ids.New("msg")
	}

	// One statement, so the message and its deliveries are atomic even when
	// db is the pool rather than a transaction.
	var inserted, queued int
	err = db.QueryRow(ctx, `
		WITH m AS (
			INSERT INTO hookpost_messages (id, owner_id, event_type, payload)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING
			RETURNING id
		), d AS (
			INSERT INTO hookpost_deliveries (message_id, endpoint_id)
			SELECT m.id, e.id
			FROM m, hookpost_endpoints e
			WHERE e.owner_id = $2
			  AND NOT e.disabled
			  AND (cardinality(e.event_types) = 0 OR $3 = ANY(e.event_types))
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM m), (SELECT count(*) FROM d)`,
		id, msg.OwnerID, msg.EventType, body).Scan(&inserted, &queued)
	if err != nil {
		return SendResult{}, fmt.Errorf("hookpost: send: %w", err)
	}
	if inserted == 0 {
		return SendResult{MessageID: id, Duplicate: true}, nil
	}
	if queued > 0 {
		if err := notify(ctx, db); err != nil {
			return SendResult{}, fmt.Errorf("hookpost: notify: %w", err)
		}
	}
	return SendResult{MessageID: id, Endpoints: queued}, nil
}

func encodePayload(p any) ([]byte, error) {
	switch v := p.(type) {
	case nil:
		return []byte("null"), nil
	case []byte:
		if !json.Valid(v) {
			return nil, ErrBadPayload
		}
		return v, nil
	case json.RawMessage:
		if !json.Valid(v) {
			return nil, ErrBadPayload
		}
		return v, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("hookpost: encode payload: %w", err)
		}
		return b, nil
	}
}
