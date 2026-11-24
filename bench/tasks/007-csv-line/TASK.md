`ParseLine` in csvline.go should split one CSV record into fields. Fields
may be wrapped in double quotes; inside quotes a comma is part of the field
and two double quotes ("") stand for one. Empty fields are kept. An
unterminated quote is an error. Today it just splits on commas. Fix it so
the tests pass. Do not change the tests.
