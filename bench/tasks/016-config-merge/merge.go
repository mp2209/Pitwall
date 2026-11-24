// Package config merges layered configuration.
package config

// Merge returns base with override applied on top.
func Merge(base, override map[string]any) map[string]any {
	out := base
	for k, v := range override {
		out[k] = v
	}
	return out
}
