package ratelimit

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestStartsFull(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	b := New(3, 1, c.now)
	for i := 0; i < 3; i++ {
		if !b.Allow(1) {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if b.Allow(1) {
		t.Error("the fourth call should be refused")
	}
}

func TestPartialSecondsRefill(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	b := New(2, 2, c.now)
	b.Allow(2)
	c.advance(250 * time.Millisecond)
	if b.Allow(1) {
		t.Error("a quarter second at 2/s is half a token")
	}
	c.advance(250 * time.Millisecond)
	if !b.Allow(1) {
		t.Error("half a second at 2/s is one token")
	}
	if b.Allow(1) {
		t.Error("only one token should have been added")
	}
}

func TestNeverOverfills(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	b := New(3, 10, c.now)
	b.Allow(3)
	c.advance(time.Hour)
	if !b.Allow(3) {
		t.Fatal("a full bucket allows its capacity")
	}
	if b.Allow(1) {
		t.Error("the bucket must not hold more than its capacity")
	}
}

func TestMoreThanCapacity(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	b := New(2, 1, c.now)
	c.advance(time.Minute)
	if b.Allow(3) {
		t.Error("asking for more than the capacity never succeeds")
	}
}
