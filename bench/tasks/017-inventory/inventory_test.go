package inventory

import (
	"errors"
	"testing"
)

func TestReservationsDontOverlap(t *testing.T) {
	st := NewStock()
	st.Add("pen", 5)
	svc := NewService(st)
	if err := svc.Place("o1", []Line{{"pen", 3}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Place("o2", []Line{{"pen", 3}}); err == nil {
		t.Error("only 2 pens are left; o2 must fail")
	}
	if got := st.Available("pen"); got != 2 {
		t.Errorf("available pens = %d, want 2", got)
	}
}

func TestFailedPlaceReservesNothing(t *testing.T) {
	st := NewStock()
	st.Add("pen", 5)
	st.Add("ink", 1)
	svc := NewService(st)
	if err := svc.Place("o1", []Line{{"pen", 2}, {"ink", 2}}); err == nil {
		t.Fatal("2 ink can't be reserved; the order must fail")
	}
	if got := st.Available("pen"); got != 5 {
		t.Errorf("available pens after a failed order = %d, want 5", got)
	}
	if _, ok := svc.StatusOf("o1"); ok {
		t.Error("a failed order must not be recorded")
	}
	if err := svc.Place("o1", []Line{{"pen", 2}, {"ink", 1}}); err != nil {
		t.Errorf("the id is free again: %v", err)
	}
}

func TestFailedPlaceSameSKUTwice(t *testing.T) {
	st := NewStock()
	st.Add("pen", 5)
	svc := NewService(st)
	if err := svc.Place("o1", []Line{{"pen", 3}, {"pen", 3}}); err == nil {
		t.Fatal("6 pens can't be reserved from 5")
	}
	if got := st.Available("pen"); got != 5 {
		t.Errorf("available pens = %d, want 5", got)
	}
}

func TestCancelReleases(t *testing.T) {
	st := NewStock()
	st.Add("pen", 5)
	svc := NewService(st)
	if err := svc.Place("o1", []Line{{"pen", 4}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel("o1"); err != nil {
		t.Fatal(err)
	}
	if got := st.Available("pen"); got != 5 {
		t.Errorf("available pens after cancel = %d, want 5", got)
	}
	if err := svc.Cancel("o1"); !errors.Is(err, ErrNotPlaced) {
		t.Errorf("cancelling twice: %v", err)
	}
	if s, _ := svc.StatusOf("o1"); s != Cancelled {
		t.Errorf("status = %v", s)
	}
}

func TestShip(t *testing.T) {
	st := NewStock()
	st.Add("pen", 5)
	svc := NewService(st)
	if err := svc.Place("o1", []Line{{"pen", 2}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ship("o1"); err != nil {
		t.Fatal(err)
	}
	if got := st.Available("pen"); got != 3 {
		t.Errorf("available pens after shipping = %d, want 3", got)
	}
	if err := svc.Cancel("o1"); !errors.Is(err, ErrNotPlaced) {
		t.Errorf("a shipped order can't be cancelled: %v", err)
	}
	if err := svc.Ship("o1"); !errors.Is(err, ErrNotPlaced) {
		t.Errorf("an order ships once: %v", err)
	}
}
