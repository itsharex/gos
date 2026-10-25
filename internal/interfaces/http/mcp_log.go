package httpapi

import (
	"regexp"
	"strings"
)

const (
	mcpLogMaxLines = 120
	mcpLogMaxRunes = 8000
)

var (
	mcpLogSensitiveLine = regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|authorization|cookie|api[_-]?key|private[_-]?key|credential|bearer|client[_-]?secret|access[_-]?key|BEGIN [A-Z ]*PRIVATE KEY|jdbc:|dsn\b)`)
	mcpLogURL           = regexp.MustCompile(`(?i)(?:https?|ftp|ssh|git|s3)://\S+`)
	mcpLogAssignment    = regexp.MustCompile(`(?i)(?:^|\s)(?:export\s+)?[a-z_][a-z0-9_]*\s*[:=]\s*\S+`)
	mcpLogLongValue     = regexp.MustCompile(`[A-Za-z0-9_+/=-]{40,}`)
)

// mcpLogExcerpt bounds the tail before returning it to an external client.
// Logs are freeform, so lines that resemble credentials or assignments are
// withheld in full; this deliberately sacrifices context for safer output.
func mcpLogExcerpt(raw string) (string, bool) {
	lines := strings.Split(raw, "\n")
	truncated := len(lines) > mcpLogMaxLines || len([]rune(raw)) > mcpLogMaxRunes
	if len(lines) > mcpLogMaxLines {
		lines = lines[len(lines)-mcpLogMaxLines:]
	}
	maskNext := false
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		sensitive := mcpLogSensitiveLine.MatchString(line) || mcpLogURL.MatchString(line) ||
			mcpLogAssignment.MatchString(line) || mcpLogLongValue.MatchString(line)
		if sensitive || maskNext {
			lines[i] = "[redacted]"
		} else {
			lines[i] = line
		}
		maskNext = sensitive && (strings.HasSuffix(strings.TrimSpace(line), ":") || strings.HasSuffix(strings.TrimSpace(line), "="))
	}
	output := []rune(strings.Join(lines, "\n"))
	if len(output) > mcpLogMaxRunes {
		output = output[len(output)-mcpLogMaxRunes:]
		truncated = true
	}
	return string(output), truncated
}
