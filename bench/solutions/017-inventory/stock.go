// Package inventory reserves and ships stock for orders.
package inventory

import "fmt"

// Stock tracks units on hand and units reserved for placed orders.
type Stock struct {
	onHand   map[string]int
	reserved map[string]int
}

// NewStock returns empty stock.
func NewStock() *Stock {
	return &Stock{onHand: map[string]int{}, reserved: map[string]int{}}
}

// Add puts qty units of sku on hand.
func (s *Stock) Add(sku string, qty int) { s.onHand[sku] += qty }

// Available is the number of units of sku that can still be reserved.
func (s *Stock) Available(sku string) int { return s.onHand[sku] - s.reserved[sku] }

// Reserve sets qty units of sku aside, or fails if not enough are available.
func (s *Stock) Reserve(sku string, qty int) error {
	if qty <= 0 {
		return fmt.Errorf("bad quantity %d", qty)
	}
	if s.Available(sku) < qty {
		return fmt.Errorf("%s: only %d available", sku, s.Available(sku))
	}
	s.reserved[sku] += qty
	return nil
}

// Release returns reserved units to available.
func (s *Stock) Release(sku string, qty int) { s.reserved[sku] -= qty }

// Ship removes reserved units from stock.
func (s *Stock) Ship(sku string, qty int) {
	s.reserved[sku] -= qty
	s.onHand[sku] -= qty
}
