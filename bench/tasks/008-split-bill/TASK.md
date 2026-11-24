money.go handles amounts in cents. `Split` divides a total among n people
as evenly as possible: the shares must add up to the total exactly, with the
first people paying the extra cents; a negative total or n below 1 is an
error. `Format` renders cents as dollars, like "$12.34", "$0.05" and "-$0.05".
Both have bugs. Fix them so the tests pass. Do not change the tests.
