// Package config merges layered configuration.
package config

// Merge returns base with override applied on top.
func Merge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = clone(v)
	}
	for k, v := range override {
		if v == nil {
			delete(out, k)
			continue
		}
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = Merge(bm, om)
				continue
			}
		}
		out[k] = clone(v)
	}
	return out
}

func clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return Merge(t, nil)
	case []any:
		c := make([]any, len(t))
		for i, x := range t {
			c[i] = clone(x)
		}
		return c
	}
	return v
}
