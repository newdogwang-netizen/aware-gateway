// Decision cache with TTL and LRU eviction.
package smartrouter

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// CachedDecision is a cached routing decision.
type CachedDecision struct {
	Model  string
	Pool   string
	Reason string
}

// cacheEntry holds a decision and its expiry time.
type cacheEntry struct {
	decision CachedDecision
	expiresAt time.Time
	// LRU tracking
	lastAccess time.Time
}

// DecisionCache is a thread-safe LRU cache with TTL.
type DecisionCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	maxSize int
	ttl     time.Duration
}

// NewDecisionCache creates a DecisionCache.
func NewDecisionCache(maxSize int, ttl time.Duration) *DecisionCache {
	return &DecisionCache{
		entries: make(map[string]*cacheEntry),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Key generates a cache key from the model menu, message count, and latest user message.
func (c *DecisionCache) Key(menu []ModelEntry, msgCount int, latestMsg string) string {
	// Sort model IDs for stable key
	ids := make([]string, len(menu))
	for i, m := range menu {
		ids[i] = m.Name
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	// Message count as a factor — different conversation depths may need different models
	countBytes := []byte{byte(msgCount >> 24), byte(msgCount >> 16), byte(msgCount >> 8), byte(msgCount)}
	h.Write(countBytes)
	// First 500 chars of latest user message
	if len(latestMsg) > 500 {
		latestMsg = latestMsg[:500]
	}
	h.Write([]byte(latestMsg))

	return hex.EncodeToString(h.Sum(nil))
}

// Get returns a cached decision if present and not expired.
func (c *DecisionCache) Get(key string) (CachedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return CachedDecision{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return CachedDecision{}, false
	}
	entry.lastAccess = time.Now()
	return entry.decision, true
}

// Set stores a decision in the cache.
func (c *DecisionCache) Set(key string, decision CachedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict if at capacity
	if len(c.entries) >= c.maxSize {
		c.evictOldest()
	}

	c.entries[key] = &cacheEntry{
		decision:   decision,
		expiresAt:  time.Now().Add(c.ttl),
		lastAccess: time.Now(),
	}
}

// evictOldest removes the least recently accessed entry.
// Caller must hold the lock.
func (c *DecisionCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for k, v := range c.entries {
		if oldestKey == "" || v.lastAccess.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.lastAccess
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// Size returns the current number of cached entries.
func (c *DecisionCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
