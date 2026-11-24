`Render` in tmpl.go fills placeholders like {{name}} in a template from a
map. Spaces inside the braces are allowed ({{ name }}). A placeholder with
no value is an error that names it, and so is a "{{" that is never closed.
Values are inserted as they are: a value that itself contains "{{x}}" is
not expanded again. Single braces are ordinary text. Fix it so the tests
pass. Do not change the tests.
