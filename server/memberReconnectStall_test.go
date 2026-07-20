package server

import (
	"testing"
	"time"
)

const (
	testXUIDA = "00090000AAAA0001"
	testXUIDB = "00090000BBBB0002"
)

func TestPendingReconnectConsumedOnce(t *testing.T) {
	p := newPendingReconnectSet()
	p.Mark(testXUIDA)

	if !p.Consume(testXUIDA) {
		t.Fatal("first Consume after Mark = false, want true")
	}
	// The stall must apply once per join, not to every later login by the same user.
	if p.Consume(testXUIDA) {
		t.Error("second Consume = true, want false (marker should be consumed)")
	}
}

func TestPendingReconnectUnmarkedUserNotStalled(t *testing.T) {
	p := newPendingReconnectSet()
	p.Mark(testXUIDA)

	// A member signing in normally has no marker and must not be delayed.
	if p.Consume(testXUIDB) {
		t.Error("Consume of unmarked xuid = true, want false")
	}
	if p.Consume("") {
		t.Error("Consume of empty xuid = true, want false")
	}
}

func TestPendingReconnectExpires(t *testing.T) {
	p := newPendingReconnectSet()
	// Backdate past the TTL: a join whose 184 never arrived must not stall a much later login.
	p.m[testXUIDA] = time.Now().Add(-pendingReconnectTTL - time.Second)

	if p.Consume(testXUIDA) {
		t.Error("Consume of expired marker = true, want false")
	}
	if len(p.m) != 0 {
		t.Errorf("expired marker not pruned, %d entr(ies) remain", len(p.m))
	}
}

func TestPendingReconnectMarkEmptyIsNoop(t *testing.T) {
	p := newPendingReconnectSet()
	p.Mark("")
	if len(p.m) != 0 {
		t.Errorf("Mark(\"\") stored an entry, %d present", len(p.m))
	}
}
