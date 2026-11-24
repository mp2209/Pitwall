package topo

import (
	"reflect"
	"strings"
	"testing"
)

func TestOrder(t *testing.T) {
	cases := []struct {
		deps map[string][]string
		want []string
	}{
		{map[string][]string{"app": {"lib", "log"}, "lib": {"log"}}, []string{"log", "lib", "app"}},
		{map[string][]string{"b": nil, "a": nil, "c": nil}, []string{"a", "b", "c"}},
		{map[string][]string{"web": {"db", "cache"}, "worker": {"db"}}, []string{"cache", "db", "web", "worker"}},
		// After m, both a and z are ready: a comes first.
		{map[string][]string{"z": nil, "b": {"z"}, "a": {"m"}, "m": nil}, []string{"m", "a", "z", "b"}},
	}
	for _, c := range cases {
		got, err := Order(c.deps)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Order(%v) = %v, %v; want %v", c.deps, got, err, c.want)
		}
	}
}

func TestCycles(t *testing.T) {
	for _, deps := range []map[string][]string{
		{"a": {"b"}, "b": {"a"}},
		{"a": {"a"}},
		{"x": nil, "a": {"b"}, "b": {"c"}, "c": {"a"}},
	} {
		_, err := Order(deps)
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Errorf("Order(%v) should report a cycle, got %v", deps, err)
		}
	}
}
