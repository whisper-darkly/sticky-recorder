package cookies

import "sync"

// entry holds one cookie string and its SWRR state.
type entry struct {
	cookies       string
	penalty       int
	currentWeight int // SWRR running weight
}

// Pool implements smooth weighted round-robin selection with penalty-based
// deprioritization. Thread-safe via sync.Mutex.
type Pool struct {
	mu      sync.Mutex
	entries []entry
}

// NewPool creates a pool from the given cookie strings, deduplicating entries.
func NewPool(cookieStrings []string) *Pool {
	seen := make(map[string]struct{}, len(cookieStrings))
	var entries []entry
	for _, c := range cookieStrings {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		entries = append(entries, entry{cookies: c})
	}
	return &Pool{entries: entries}
}

// Select picks a cookie string using smooth weighted round-robin based on
// inverse penalty weights. Returns "" if the pool is empty.
func (p *Pool) Select() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.entries) == 0 {
		return ""
	}
	if len(p.entries) == 1 {
		return p.entries[0].cookies
	}

	// Compute effective weights: maxPenalty - penalty + 1
	maxPenalty := 0
	for i := range p.entries {
		if p.entries[i].penalty > maxPenalty {
			maxPenalty = p.entries[i].penalty
		}
	}

	totalWeight := 0
	for i := range p.entries {
		w := maxPenalty - p.entries[i].penalty + 1
		p.entries[i].currentWeight += w
		totalWeight += w
	}

	// Pick entry with highest currentWeight
	best := 0
	for i := 1; i < len(p.entries); i++ {
		if p.entries[i].currentWeight > p.entries[best].currentWeight {
			best = i
		}
	}

	p.entries[best].currentWeight -= totalWeight
	return p.entries[best].cookies
}

// Penalize increases the penalty for the given cookie string and re-roots
// all penalties so the minimum is always 0.
func (p *Pool) Penalize(cookies string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.entries {
		if p.entries[i].cookies == cookies {
			p.entries[i].penalty++
			break
		}
	}
	p.reroot()
}

// Update merges a new set of cookie strings into the pool. New strings are
// added with penalty 0, missing strings are removed, and existing entries
// keep their penalties. Re-roots after merge.
func (p *Pool) Update(newCookies []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Build set of new cookies for fast lookup
	newSet := make(map[string]struct{}, len(newCookies))
	for _, c := range newCookies {
		newSet[c] = struct{}{}
	}

	// Build map of existing penalties
	existing := make(map[string]int, len(p.entries))
	for _, e := range p.entries {
		existing[e.cookies] = e.penalty
	}

	// Rebuild entries: preserve penalties for retained, add new at 0
	seen := make(map[string]struct{}, len(newCookies))
	var entries []entry
	for _, c := range newCookies {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		pen := 0
		if p, ok := existing[c]; ok {
			pen = p
		}
		entries = append(entries, entry{cookies: c, penalty: pen})
	}

	p.entries = entries
	p.reroot()
}

// Count returns the number of entries in the pool.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// reroot subtracts the minimum penalty from all entries so the lowest is 0.
// Must be called with p.mu held.
func (p *Pool) reroot() {
	if len(p.entries) == 0 {
		return
	}
	minPenalty := p.entries[0].penalty
	for _, e := range p.entries[1:] {
		if e.penalty < minPenalty {
			minPenalty = e.penalty
		}
	}
	if minPenalty > 0 {
		for i := range p.entries {
			p.entries[i].penalty -= minPenalty
		}
	}
}
