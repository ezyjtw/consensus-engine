package arb

import "sync"

// Cooldown is an in-memory, per-key debouncer that prevents the engine from
// emitting repeated intents for the same venue pair within a configured window.
type Cooldown struct {
	mu     sync.Mutex
	lastMs map[string]int64
	ttlMs  int64
}

func NewCooldown(ttlMs int64) *Cooldown {
	return &Cooldown{
		lastMs: make(map[string]int64),
		ttlMs:  ttlMs,
	}
}

// IsOnCooldown returns true if the key was last marked within the cooldown window.
func (c *Cooldown) IsOnCooldown(key string, nowMs int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.lastMs[key]
	return ok && nowMs-last < c.ttlMs
}

// IsOnCooldownWithTTL is like IsOnCooldown but uses a caller-supplied TTL
// instead of the default. Used for per-symbol cooldown overrides.
func (c *Cooldown) IsOnCooldownWithTTL(key string, nowMs, ttlMs int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.lastMs[key]
	return ok && nowMs-last < ttlMs
}

// Mark records nowMs as the last-seen time for the given key.
func (c *Cooldown) Mark(key string, nowMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastMs[key] = nowMs
}
