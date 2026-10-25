package httpapi

import (
	"strings"
	"testing"
)

func TestMCPLogExcerptRedactsCredentialsAndBoundsOutput(t *testing.T) {
	log := strings.Join([]string{
		"building module core",
		"PASSWORD=secret-value",
		"Authorization: Bearer 123456789",
		"token:",
		"second-line-secret",
		"download https://user:password@example.org/pkg?token=abc",
		"export API_KEY=top-secret",
		"build 0123456789012345678901234567890123456789",
		"step finished successfully",
	}, "\n")
	got, truncated := mcpLogExcerpt(log)
	if truncated || !strings.Contains(got, "building module core") || !strings.Contains(got, "step finished successfully") {
		t.Fatalf("safe lines missing or unexpectedly truncated: %q, %v", got, truncated)
	}
	for _, value := range []string{"secret-value", "123456789", "second-line-secret", "example.org", "top-secret", "0123456789"} {
		if strings.Contains(got, value) {
			t.Fatalf("log excerpt exposed %q: %q", value, got)
		}
	}
	manyLines := strings.Repeat("normal build output\n", 200)
	got, truncated = mcpLogExcerpt(manyLines)
	if !truncated || len(strings.Split(got, "\n")) > mcpLogMaxLines {
		t.Fatalf("line bound missing: lines=%d truncated=%v", len(strings.Split(got, "\n")), truncated)
	}
	got, truncated = mcpLogExcerpt(strings.Repeat("x", mcpLogMaxRunes+100))
	if !truncated || len([]rune(got)) > mcpLogMaxRunes {
		t.Fatalf("character bound missing: chars=%d truncated=%v", len([]rune(got)), truncated)
	}
}
