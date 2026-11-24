package inventory

import (
	"errors"
	"fmt"
)

// Line is one product and quantity in an order.
type Line struct {
	SKU string
	Qty int
}

// Status is where an order is in its life.
type Status int

// Order statuses.
const (
	Placed Status = iota + 1
	Cancelled
	Shipped
)

// Order is a placed order.
type Order struct {
	ID     string
	Lines  []Line
	Status Status
}

// ErrNotPlaced is returned when an order is not in the Placed state.
var ErrNotPlaced = errors.New("order is not placed")

// Service places, cancels and ships orders against a Stock.
type Service struct {
	stock  *Stock
	orders map[string]*Order
}

// NewService returns a service using stock.
func NewService(stock *Stock) *Service {
	return &Service{stock: stock, orders: map[string]*Order{}}
}

// Place reserves every line of an order and records it.
func (s *Service) Place(id string, lines []Line) error {
	if _, ok := s.orders[id]; ok {
		return fmt.Errorf("order %s already exists", id)
	}
	for i, l := range lines {
		if err := s.stock.Reserve(l.SKU, l.Qty); err != nil {
			for _, done := range lines[:i] {
				s.stock.Release(done.SKU, done.Qty)
			}
			return err
		}
	}
	s.orders[id] = &Order{ID: id, Lines: lines, Status: Placed}
	return nil
}

// Cancel cancels a placed order.
func (s *Service) Cancel(id string) error {
	o, ok := s.orders[id]
	if !ok {
		return fmt.Errorf("no order %s", id)
	}
	if o.Status != Placed {
		return ErrNotPlaced
	}
	for _, l := range o.Lines {
		s.stock.Release(l.SKU, l.Qty)
	}
	o.Status = Cancelled
	return nil
}

// Ship ships a placed order.
func (s *Service) Ship(id string) error {
	o, ok := s.orders[id]
	if !ok {
		return fmt.Errorf("no order %s", id)
	}
	if o.Status != Placed {
		return ErrNotPlaced
	}
	for _, l := range o.Lines {
		s.stock.Ship(l.SKU, l.Qty)
	}
	o.Status = Shipped
	return nil
}

// StatusOf returns an order's status and whether the order exists.
func (s *Service) StatusOf(id string) (Status, bool) {
	o, ok := s.orders[id]
	if !ok {
		return 0, false
	}
	return o.Status, true
}
