`Cache` in lru.go is a least-recently-used cache: when it is full, adding a
new key evicts the key used longest ago. Reading a key with Get, or
updating it with Put, counts as using it. Right now it evicts the wrong
keys. Fix it so the tests pass. Do not change the tests.
