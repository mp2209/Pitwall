package config

// Layered merges layers in order: later layers win.
func Layered(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		out = Merge(out, l)
	}
	return out
}
