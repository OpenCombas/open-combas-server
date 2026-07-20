package server

import (
	"sync"
	"time"
)

// Tracks users who have just been added to a squad by a 182 join and are therefore about to perform the
// applicant-teardown -> member-reconnect dance that races the host's ~10s stale-entry retirement.
//
// Only those users need their 184 login reply stalled. A member signing in normally (cold boot, no join in
// progress) has no stale applicant entry on any host and must not pay the latency.
//
// Entries are consumed on first use, so the stall applies once per join rather than to every subsequent
// login. The TTL only bounds leakage from joins whose 184 never arrives (joiner quit at the wrong moment);
// it is deliberately much longer than the real join -> login gap, which is sub-second.
const pendingReconnectTTL = 2 * time.Minute

type pendingReconnectSet struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newPendingReconnectSet() *pendingReconnectSet {
	return &pendingReconnectSet{m: make(map[string]time.Time)}
}

// pendingMemberReconnects is process-wide because the 182 (join) and 184 (login) servers are separate
// listeners that share no other state.
var pendingMemberReconnects = newPendingReconnectSet()

// Mark records that xuid has just joined a squad and will reconnect as a member shortly.
func (p *pendingReconnectSet) Mark(xuid string) {
	if xuid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked()
	p.m[xuid] = time.Now()
}

// Consume reports whether xuid has a live pending-reconnect marker, removing it. False for an unknown or
// expired xuid, so a normal login is never stalled.
func (p *pendingReconnectSet) Consume(xuid string) bool {
	if xuid == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked()
	if _, ok := p.m[xuid]; !ok {
		return false
	}
	delete(p.m, xuid)
	return true
}

func (p *pendingReconnectSet) pruneLocked() {
	cutoff := time.Now().Add(-pendingReconnectTTL)
	for k, t := range p.m {
		if t.Before(cutoff) {
			delete(p.m, k)
		}
	}
}
