package ratelimit

// Limiter is a placeholder; Task 16 implements the token-bucket logic.
type Limiter struct{}

func New() *Limiter { return &Limiter{} }

// Allow currently always returns true. Task 16 wires the per-key token bucket.
func (l *Limiter) Allow(keyID string) bool { return true }
