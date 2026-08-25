package teamrun

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxCauseSummaryBytes = 1024

const truncatedCauseSuffix = "…[truncated]"

var unsafeModelContent = regexp.MustCompile(`(?i)(raw[\s_-]*)?(model[\s_-]*)?(prompt|completion|messages?|input|output)\s*[:=]`)

var transportCompletionMarker = regexp.MustCompile(`(?i)stream disconnected before completion\s*:`)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(authorization\s*:\s*)(?:bearer|basic)\s+[^\s,;]+`),
	regexp.MustCompile(`(?i)\b(cookie|set-cookie)(\s*:\s*)[^\r\n]+`),
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|access_token|refresh_token|api[_-]?key|secret|client_secret)(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`),
	regexp.MustCompile(`(?i)\b(postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://([^:/@\s]+):([^@\s]+)@`),
	regexp.MustCompile(`\b(?:sk|wv_sk)-[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// SanitizeCauseSummary returns nil when the input may contain model content
// that cannot be reliably separated from safe diagnostics.
func SanitizeCauseSummary(cause error) *string {
	for current := cause; current != nil; current = errors.Unwrap(current) {
		if summary := sanitizeCauseText(current.Error()); summary != nil {
			return summary
		}
	}
	return nil
}

func sanitizeCauseText(text string) *string {
	summary := strings.TrimSpace(text)
	if summary == "" || !utf8.ValidString(summary) {
		return nil
	}
	// Codex uses "stream disconnected before completion:" as a transport
	// diagnostic. Its word "completion" does not introduce model content.
	// Normalize only that exact marker before applying the conservative model
	// content filter; every other prompt/completion/input/output marker keeps
	// failing closed.
	safetyText := transportCompletionMarker.ReplaceAllString(summary, "stream disconnected before response finished -")
	if unsafeModelContent.MatchString(safetyText) {
		return nil
	}

	summary = credentialPatterns[0].ReplaceAllString(summary, `${1}[REDACTED]`)
	summary = credentialPatterns[1].ReplaceAllString(summary, `${1}${2}[REDACTED]`)
	summary = credentialPatterns[2].ReplaceAllString(summary, `${1}${2}[REDACTED]`)
	summary = credentialPatterns[3].ReplaceAllString(summary, `${1}://${2}:[REDACTED]@`)
	for _, pattern := range credentialPatterns[4:] {
		summary = pattern.ReplaceAllString(summary, `[REDACTED]`)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}
	if len(summary) <= maxCauseSummaryBytes {
		return &summary
	}

	limit := maxCauseSummaryBytes - len(truncatedCauseSuffix)
	for limit > 0 && !utf8.RuneStart(summary[limit]) {
		limit--
	}
	summary = summary[:limit] + truncatedCauseSuffix
	return &summary
}
