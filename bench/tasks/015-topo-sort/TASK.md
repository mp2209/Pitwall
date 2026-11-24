`Order` in topo.go puts items in dependency order: deps[x] lists what x
depends on, and every item must come after all of its dependencies. Items
that only appear as dependencies are included too. Whenever several items
are ready, take the alphabetically first one, so the result is always the
same. A dependency cycle is an error whose message contains "cycle". Fix it
so the tests pass. Do not change the tests.
