package api

import (
	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/cacheopt"
)

// cacheOptimizerOn reports whether the gateway adds cache breakpoints for a
// caller: the key's switch, or the config default in dev mode.
func (s *Server) cacheOptimizerOn(key *auth.Key) bool {
	if key != nil {
		return key.CacheOptimizerOn
	}
	return s.cfg.CacheOptimizerDefault
}

// optimizeCache adds prompt-cache breakpoints to an Anthropic-format body
// when the caller has the optimizer on. On any problem it sends the body as
// the client wrote it.
func (s *Server) optimizeCache(ex *exchange, body []byte) []byte {
	if !s.cacheOptimizerOn(ex.key) {
		ex.cacheOutcome = "off"
		return body
	}
	out, res, err := cacheopt.Apply(body, ex.route.Model)
	if err != nil {
		s.log.Warn("cache optimizer skipped a request it could not read", "err", err, "id", ex.id)
		ex.cacheOutcome = "error"
		return body
	}
	ex.breakpointsAdded, ex.cacheOutcome = res.Added, res.Outcome
	return out
}
