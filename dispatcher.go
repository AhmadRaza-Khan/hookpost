package hookpost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ahmadraza-khan/hookpost/signature"
)

// leaseMargin is added to Timeout when leasing, so a lease only expires if
// the worker holding it is really gone.
const leaseMargin = 30 * time.Second

// pruneBatch is how many messages Prune deletes per statement.
const pruneBatch = 1000

type leased struct {
	deliveryID int64
	attempt    int
	msgID      string
	eventType  string
	payload    []byte
	endpointID string
	url        string
	secret     string
	oldSecret  *string
}

// Run delivers webhooks until ctx is cancelled, then waits for in-flight
// requests to finish and be recorded before returning. Call it once per
// process; running it on several instances against one database is safe,
// since deliveries are leased with FOR UPDATE SKIP LOCKED.
//
// Run holds one pool connection for LISTEN (unless Config.DisableListen)
// plus up to Config.Workers connections while recording results.
//
// Delivery is at-least-once: if a process dies mid-request, the delivery is
// sent again once its lease (Timeout + 30s) expires, with the same
// webhook-id. Receivers should dedupe on that header.
func (c *Client) Run(ctx context.Context) error {
	var bg sync.WaitGroup
	defer bg.Wait()

	wake := make(chan struct{}, 1)
	if !c.cfg.DisableListen {
		bg.Add(1)
		go func() { defer bg.Done(); c.listen(ctx, wake) }()
	}
	if c.cfg.Retention > 0 {
		bg.Add(1)
		go func() { defer bg.Done(); c.pruneLoop(ctx) }()
	}

	slots := make(chan struct{}, c.cfg.Workers)
	var wg sync.WaitGroup
	defer wg.Wait() // graceful: finish in-flight requests

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		// Reserve worker slots before leasing, so leased rows never sit in
		// memory waiting for a worker while their lease runs out.
		n := c.reserve(ctx, slots)
		if n == 0 {
			return nil // ctx cancelled
		}
		batch, err := c.lease(ctx, n)
		if err != nil && ctx.Err() == nil {
			c.log.Error("hookpost: lease", "err", err)
		}
		for i := len(batch); i < n; i++ {
			<-slots
		}
		for _, d := range batch {
			wg.Add(1)
			go func(d leased) {
				defer func() { <-slots; wg.Done() }()
				// Let the request and its bookkeeping finish after shutdown starts.
				c.deliver(context.WithoutCancel(ctx), d)
			}(d)
		}
		if len(batch) > 0 && len(batch) == n {
			continue // probably more work waiting; don't sleep
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-wake:
		}
	}
}

// reserve blocks for one free worker slot, then grabs more up to BatchSize.
// It returns 0 if ctx is cancelled first.
func (c *Client) reserve(ctx context.Context, slots chan struct{}) int {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return 0
	}
	n := 1
	for n < c.cfg.BatchSize {
		select {
		case slots <- struct{}{}:
			n++
		default:
			return n
		}
	}
	return n
}

// lease claims up to limit due deliveries: it bumps attempts and pushes
// next_attempt_at past the request timeout in one statement, so no
// transaction is held during HTTP. Deliveries of disabled endpoints stay
// pending and resume when the endpoint is enabled again.
func (c *Client) lease(ctx context.Context, limit int) ([]leased, error) {
	rows, err := c.pool.Query(ctx, `
		WITH due AS (
			SELECT d.id FROM hookpost_deliveries d
			JOIN hookpost_endpoints e ON e.id = d.endpoint_id
			WHERE d.status = 'pending' AND d.next_attempt_at <= now() AND NOT e.disabled
			ORDER BY d.next_attempt_at
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		), upd AS (
			UPDATE hookpost_deliveries d
			SET attempts = d.attempts + 1,
			    next_attempt_at = now() + make_interval(secs => $2),
			    updated_at = now()
			FROM due WHERE d.id = due.id
			RETURNING d.id, d.attempts, d.message_id, d.endpoint_id
		)
		SELECT upd.id, upd.attempts, m.id, m.event_type, m.payload,
		       e.id, e.url, e.secret,
		       CASE WHEN e.old_secret_expires_at > now() THEN e.old_secret END
		FROM upd
		JOIN hookpost_messages  m ON m.id = upd.message_id
		JOIN hookpost_endpoints e ON e.id = upd.endpoint_id`,
		limit, seconds(c.cfg.Timeout+leaseMargin))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []leased
	for rows.Next() {
		var d leased
		if err := rows.Scan(&d.deliveryID, &d.attempt, &d.msgID, &d.eventType,
			&d.payload, &d.endpointID, &d.url, &d.secret, &d.oldSecret); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (c *Client) deliver(ctx context.Context, d leased) {
	start := time.Now()
	code, respBody, err := c.post(ctx, d)
	dur := time.Since(start)

	success := err == nil && code >= 200 && code < 300
	status, next, lastErr := "succeeded", time.Now(), (*string)(nil)
	dead := false
	if !success {
		gone := code == http.StatusGone // receiver says "stop sending"
		if gone {
			c.disableEndpoint(ctx, d.endpointID, "endpoint returned 410 Gone")
		}
		delay, more := c.nextDelay(d.attempt)
		dead = !more || gone
		status, next = "pending", time.Now().Add(delay)
		if dead {
			status, next = "dead", time.Now()
		}
		s := errString(err, code)
		lastErr = &s
	}

	attemptErr := errOrNil(err)

	// The outcome and its attempt log row are written together, so anyone
	// who sees the new status also sees the attempt that caused it.
	err2 := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		// "attempts = $2" guards against a stale worker whose lease expired
		// and was taken over by another instance.
		if _, err := tx.Exec(ctx, `
			UPDATE hookpost_deliveries
			SET status = $3, next_attempt_at = $4, last_status_code = $5,
			    last_error = $6, updated_at = now()
			WHERE id = $1 AND attempts = $2 AND status = 'pending'`,
			d.deliveryID, d.attempt, status, next, nullInt(code), lastErr); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO hookpost_attempts
			  (delivery_id, attempt, status_code, error, response_body, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			d.deliveryID, d.attempt, nullInt(code), attemptErr, respBody, dur.Milliseconds())
		return err
	})
	logIfErr(c, "record attempt", err2)

	if c.cfg.OnAttempt != nil {
		c.cfg.OnAttempt(AttemptInfo{
			DeliveryID: d.deliveryID, MessageID: d.msgID, EndpointID: d.endpointID,
			EventType: d.eventType, Attempt: d.attempt, StatusCode: code,
			Err: err, Duration: dur, Dead: dead,
		})
	}
}

func (c *Client) post(ctx context.Context, d leased) (int, string, error) {
	secrets := []string{d.secret}
	if d.oldSecret != nil {
		secrets = append(secrets, *d.oldSecret) // receiver may still have the old one
	}
	h, err := signature.Headers(d.msgID, time.Now(), d.payload, secrets...)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(d.payload))
	if err != nil {
		return 0, "", err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // allow conn reuse
	// TEXT columns reject invalid UTF-8, and truncation can split a rune
	return resp.StatusCode, string(bytes.ToValidUTF8(body, []byte("�"))), nil
}

// disableEndpoint is the dispatcher's unscoped disable (e.g. on 410 Gone).
func (c *Client) disableEndpoint(ctx context.Context, id, reason string) {
	_, err := c.pool.Exec(ctx, `
		UPDATE hookpost_endpoints
		SET disabled = true, disabled_reason = $2, updated_at = now()
		WHERE id = $1`, id, reason)
	logIfErr(c, "disable endpoint", err)
}

// listen wakes the dispatch loop on NOTIFY, reconnecting on errors.
func (c *Client) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := c.pool.Acquire(ctx)
		if err != nil {
			sleepCtx(ctx, time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err == nil {
			for {
				if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
					break
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
		// The connection may still be LISTENing or broken; don't return it to the pool.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
		sleepCtx(ctx, time.Second)
	}
}

func (c *Client) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := c.Prune(ctx, c.cfg.Retention); err != nil && ctx.Err() == nil {
			c.log.Error("hookpost: prune", "err", err)
		} else if n > 0 {
			c.log.Info("hookpost: pruned messages", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Prune deletes messages older than olderThan that have no pending
// deliveries, along with their deliveries and attempts. Run calls it hourly
// when Config.Retention > 0. It returns the number of messages deleted.
func (c *Client) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	var total int64
	for {
		tag, err := c.pool.Exec(ctx, `
			DELETE FROM hookpost_messages WHERE id IN (
				SELECT m.id FROM hookpost_messages m
				WHERE m.created_at < now() - make_interval(secs => $1)
				  AND NOT EXISTS (
					SELECT 1 FROM hookpost_deliveries d
					WHERE d.message_id = m.id AND d.status = 'pending')
				LIMIT $2)`, seconds(olderThan), pruneBatch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < pruneBatch {
			return total, nil
		}
	}
}

// seconds converts d for use with make_interval(secs => $n).
func seconds(d time.Duration) float64 { return d.Seconds() }

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func nullInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func errString(err error, code int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", code)
}

func errOrNil(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

func logIfErr(c *Client, what string, err error) {
	if err != nil {
		c.log.Error("hookpost: "+what, "err", err)
	}
}
