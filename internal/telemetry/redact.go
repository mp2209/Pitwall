package telemetry

import (
	"regexp"
	"strings"
)

// secretPatterns match credentials with well-known shapes. They run before
// the other patterns so a key is never half-redacted as, say, a phone number.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),                // Anthropic
	regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}`), // OpenAI
	regexp.MustCompile(`\b[ps]k-lf-[A-Za-z0-9-]{8,}`),                // Langfuse
	regexp.MustCompile(`\bpw-[A-Za-z0-9_-]{20,}`),                    // Pitwall
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),              // AWS access key IDs
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),             // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), // Slack
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),      // Google API keys
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
}

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	// 13 to 19 digits, optionally grouped by single spaces or dashes; only
	// numbers that pass the Luhn check are redacted.
	cardPattern = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	// Phone numbers need separators, so plain runs of digits (IDs, counts,
	// timestamps) are left alone.
	phonePattern = regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?(?:\(\d{3}\)\s?|\b\d{3}[ .-])\d{3}[ .-]\d{4}\b`)
)

// Redact replaces credentials, email addresses, card numbers and phone
// numbers with markers such as "[REDACTED:email]". Markers contain no quotes
// or backslashes, so redacting inside JSON text keeps it valid.
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "[REDACTED:secret]")
	}
	s = emailPattern.ReplaceAllString(s, "[REDACTED:email]")
	s = cardPattern.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(m) {
			return "[REDACTED:card]"
		}
		return m
	})
	return phonePattern.ReplaceAllString(s, "[REDACTED:phone]")
}

func luhn(s string) bool {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
