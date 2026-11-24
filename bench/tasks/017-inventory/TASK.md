This package reserves stock for orders. stock.go tracks units on hand and
units reserved; order.go places, cancels and ships orders. Three rules are
broken: a reservation must not take units another order already reserved;
placing an order is all or nothing (if any line can't be reserved, nothing
stays reserved and the order isn't recorded); cancelling a placed order
releases its units. Fix it so the tests pass. Do not change the tests.
