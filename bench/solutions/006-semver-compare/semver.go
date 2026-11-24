// Package semver orders semantic versions.
package semver

import (
	"strconv"
	"strings"
)

// Compare returns -1, 0 or 1 as version a is lower than, equal to, or
// higher than version b.
func Compare(a, b string) int {
	av, apre := split(a)
	bv, bpre := split(b)
	for i := range av {
		if c := cmpInt(av[i], bv[i]); c != 0 {
			return c
		}
	}
	switch {
	case apre == "" && bpre == "":
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	ai, bi := strings.Split(apre, "."), strings.Split(bpre, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		an, aerr := strconv.Atoi(ai[i])
		bn, berr := strconv.Atoi(bi[i])
		switch {
		case aerr == nil && berr == nil:
			if c := cmpInt(an, bn); c != 0 {
				return c
			}
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(ai[i], bi[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(ai), len(bi))
}

func split(v string) ([3]int, string) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	var out [3]int
	for i, p := range strings.SplitN(core, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out, pre
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
