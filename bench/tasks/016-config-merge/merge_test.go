package config

import (
	"reflect"
	"testing"
)

func base() map[string]any {
	return map[string]any{
		"name": "api",
		"db":   map[string]any{"host": "localhost", "port": 5432, "password": "dev"},
		"tags": []any{"a", "b"},
	}
}

func TestDeepMerge(t *testing.T) {
	got := Merge(base(), map[string]any{
		"db":    map[string]any{"host": "db.internal", "password": nil},
		"tags":  []any{"c"},
		"debug": true,
	})
	want := map[string]any{
		"name":  "api",
		"db":    map[string]any{"host": "db.internal", "port": 5432},
		"tags":  []any{"c"},
		"debug": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v\nwant  %v", got, want)
	}
}

func TestNilDeletesTopLevel(t *testing.T) {
	got := Merge(base(), map[string]any{"tags": nil})
	if _, ok := got["tags"]; ok {
		t.Error("a nil override should delete the key")
	}
}

func TestInputsUnchanged(t *testing.T) {
	b := base()
	o := map[string]any{"db": map[string]any{"port": 6543}, "cache": map[string]any{"ttl": 60}}
	got := Merge(b, o)
	if !reflect.DeepEqual(b, base()) {
		t.Errorf("base was modified: %v", b)
	}
	got["db"].(map[string]any)["host"] = "changed"
	got["cache"].(map[string]any)["ttl"] = 1
	got["tags"].([]any)[0] = "changed"
	if !reflect.DeepEqual(b, base()) {
		t.Errorf("changing the result changed base: %v", b)
	}
	if o["cache"].(map[string]any)["ttl"] != 60 {
		t.Errorf("changing the result changed override: %v", o)
	}
}

func TestLayered(t *testing.T) {
	defaults := map[string]any{"log": map[string]any{"level": "info", "json": false}}
	prod := map[string]any{"log": map[string]any{"json": true}}
	local := map[string]any{"log": map[string]any{"level": "debug"}}
	got := Layered(defaults, prod, local)
	want := map[string]any{"log": map[string]any{"level": "debug", "json": true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Layered = %v, want %v", got, want)
	}
	if defaults["log"].(map[string]any)["level"] != "info" {
		t.Error("Layered changed a layer")
	}
}
