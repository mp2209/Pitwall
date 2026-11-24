// Package topo orders items by their dependencies.
package topo

import (
	"errors"
	"sort"
)

// Order returns the items of deps so that each comes after its
// dependencies.
func Order(deps map[string][]string) ([]string, error) {
	indegree := map[string]int{}
	users := map[string][]string{}
	for item, ds := range deps {
		if _, ok := indegree[item]; !ok {
			indegree[item] = 0
		}
		for _, d := range ds {
			if _, ok := indegree[d]; !ok {
				indegree[d] = 0
			}
			indegree[item]++
			users[d] = append(users[d], item)
		}
	}
	var ready []string
	for item, n := range indegree {
		if n == 0 {
			ready = append(ready, item)
		}
	}
	var out []string
	for len(ready) > 0 {
		sort.Strings(ready)
		item := ready[0]
		ready = ready[1:]
		out = append(out, item)
		for _, u := range users[item] {
			indegree[u]--
			if indegree[u] == 0 {
				ready = append(ready, u)
			}
		}
	}
	if len(out) != len(indegree) {
		return nil, errors.New("dependency cycle")
	}
	return out, nil
}
