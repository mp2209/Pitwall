package lru

import "testing"

func TestGetCountsAsUse(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")
	c.Put("c", 3) // b is now the least recently used
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("a = %d, %v", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Errorf("c = %d, %v", v, ok)
	}
}

func TestPutCountsAsUse(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 10) // a is now the most recently used
	c.Put("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted")
	}
	if v, _ := c.Get("a"); v != 10 {
		t.Errorf("a = %d, want 10", v)
	}
	if c.Len() != 2 {
		t.Errorf("len = %d", c.Len())
	}
}

func TestMissing(t *testing.T) {
	if _, ok := New(1).Get("x"); ok {
		t.Error("empty cache returned a value")
	}
}
