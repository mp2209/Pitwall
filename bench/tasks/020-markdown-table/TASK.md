`Table` in mdtable.go renders rows as a Markdown table. Every column is
padded to its widest cell, counted in characters, not bytes ("Zürich" is 6
wide), and is at least 3 wide so the separator always has three dashes. A
row with fewer cells than the header gets empty cells. A "|" inside a cell
is written as "\|" (and counts as two characters). Fix it so the tests
pass. Do not change the tests.
