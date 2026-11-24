`Compare` in semver.go should order semantic versions the way the SemVer
spec does: major, minor and patch compare as numbers (1.10.0 is after
1.9.0), a leading "v" is ignored, and a pre-release ("1.0.0-beta.2") comes
before its release. Pre-release identifiers compare one by one: numbers
numerically, other identifiers as text, numbers before text, and when all
shared identifiers are equal the shorter list comes first. Today it compares
plain strings. Fix it so the tests pass. Do not change the tests.
