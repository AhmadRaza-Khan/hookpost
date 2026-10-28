package hookpost

import (
	"math/rand/v2"
	"time"
)

// DefaultSchedule is the delay after attempt 1, 2, ... giving 8 attempts over
// about 1.5 days. Slice it to build a shorter schedule:
//
//	cfg.Schedule = hookpost.DefaultSchedule[:3] // 4 attempts total
var DefaultSchedule = []time.Duration{
	5 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	5 * time.Hour,
	10 * time.Hour,
	10 * time.Hour,
}

// nextDelay returns the delay before the next attempt, or ok=false if
// attempts are exhausted. attempt is the number already made (1-based).
func (c *Client) nextDelay(attempt int) (time.Duration, bool) {
	if attempt < 1 || attempt > len(c.cfg.Schedule) {
		return 0, false
	}
	d := c.cfg.Schedule[attempt-1]
	// ±10% jitter so a dead endpoint coming back isn't stampeded
	j := time.Duration(float64(d) * (rand.Float64()*0.2 - 0.1)) //nolint:gosec // jitter needs no crypto
	return d + j, true
}
