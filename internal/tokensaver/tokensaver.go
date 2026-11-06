// Package tokensaver shrinks tool results before they are sent back to a model.
//
// Agent loops spend most of their input tokens on tool output: a full git diff,
// a grep listing, JSON from an API. Most of that detail is never used. The saver
// only touches tool results (never a user or assistant message), always keeps the
// head and tail of a large payload, and adds a marker so a second pass is a no-op.
package tokensaver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lgllm/internal/translate"
)

// Marker is appended to every rewritten tool result. Its presence means the
// content was already processed (or was produced by another lgllm hop), so the
// saver leaves it alone.
const Marker = "[lgllm:token-saver]"

// Options are the guards that decide whether and how much to compress.
type Options struct {
	// MinBytes skips small results: rewriting a 200-byte output costs more in
	// lost detail than it saves.
	MinBytes int
	// MaxBytes caps the result after compression. Zero means "only the
	// head/tail default".
	MaxBytes int
	// Prompt is an optional extra system instruction ("answer like caveman").
	// Empty leaves the system prompt untouched.
	Prompt string
}

// Stats reports what the saver did for one request.
type Stats struct {
	Before  int      // bytes of tool-result text seen
	After   int      // bytes after compression
	Saved   int      // Before - After
	Filters []string // filter names that fired, in order of first use
	Applied bool
	// SystemChanged is set by the caller when the optional prompt was added, so
	// a same-dialect request knows to re-encode the system message.
	SystemChanged bool
}

// Result returns the saving as a fraction, for logs and headers.
func (s Stats) Result() string {
	if s.Before == 0 {
		return "0/0"
	}
	return fmt.Sprintf("%d/%d", s.Saved, s.Before)
}

// Apply compresses every eligible tool result in the pivot request.
//
// It mutates req in place and is idempotent: content that carries Marker is
// skipped, so a client that retries through lgllm cannot be compressed twice.
func Apply(req *translate.Request, opts Options) Stats {
	var stats Stats
	if req == nil {
		return stats
	}
	if opts.Prompt != "" && !strings.Contains(req.System, opts.Prompt) {
		if req.System != "" {
			req.System += "\n\n"
		}
		req.System += opts.Prompt
	}
	seen := make(map[string]bool, 4)

	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "tool" || m.ToolError {
			continue
		}
		text, ok := toolText(m)
		if !ok {
			continue
		}
		stats.Before += len(text)
		if len(text) < opts.MinBytes {
			stats.After += len(text)
			continue
		}
		if strings.Contains(text, Marker) {
			stats.After += len(text)
			continue
		}

		out, filter := compress(text, opts.MaxBytes)
		if filter == "" || len(out) >= len(text) {
			stats.After += len(text)
			continue
		}
		out += "\n" + Marker
		stats.After += len(out)
		stats.Saved += len(text) - len(out)
		stats.Applied = true
		if !seen[filter] {
			seen[filter] = true
			stats.Filters = append(stats.Filters, filter)
		}
		m.Parts = []translate.Part{{Type: "text", Text: out}}
	}
	return stats
}

func toolText(m *translate.Message) (string, bool) {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// compress picks a filter by content shape and returns the result plus the
// filter name. An empty name means "nothing safe to do".
func compress(text string, maxBytes int) (string, string) {
	trimmed := strings.TrimSpace(text)

	out, filter := "", ""
	switch {
	case looksLikeDiff(trimmed):
		out, filter = compressDiff(trimmed), "diff"
	case looksLikeJSON(trimmed):
		if compact, ok := compressJSON(trimmed); ok {
			out, filter = compact, "json"
		}
	case looksLikeStatus(trimmed):
		out, filter = dedupeLines(trimmed), "status"
	case looksLikeGrep(trimmed):
		out, filter = dedupeLines(trimmed), "grep"
	default:
		out, filter = dedupeLines(trimmed), "log"
	}
	if filter == "" {
		return "", ""
	}
	if out == "" {
		return "", ""
	}
	return headTail(out, targetSize(maxBytes), filter), filter
}

const defaultTarget = 8 << 10

func targetSize(maxBytes int) int {
	if maxBytes > 0 && maxBytes < defaultTarget {
		return maxBytes
	}
	return defaultTarget
}

// headTail keeps both ends: a model needs the start (what happened) and the end
// (the final error, the summary) far more than the middle of a long dump.
func headTail(s string, target int, filter string) string {
	if len(s) <= target {
		return s
	}
	head := target * 2 / 3
	tail := target - head
	cut := len(s) - head - tail
	return s[:head] +
		fmt.Sprintf("\n... [lgllm: %d bytes of %s elided] ...\n", cut, filter) +
		s[len(s)-tail:]
}

func looksLikeDiff(s string) bool {
	return strings.HasPrefix(s, "diff --git ") ||
		strings.HasPrefix(s, "--- a/") ||
		strings.Contains(s, "\ndiff --git ")
}

func looksLikeJSON(s string) bool {
	if s == "" {
		return false
	}
	if s[0] != '{' && s[0] != '[' {
		return false
	}
	return json.Valid([]byte(s))
}

func looksLikeStatus(s string) bool {
	return strings.HasPrefix(s, "On branch ") ||
		strings.HasPrefix(s, "Your branch is ") ||
		strings.Contains(s, "\nnothing to commit")
}

func looksLikeGrep(s string) bool {
	// path:line:text is the shape of grep/rg output. A couple of lines is not
	// enough evidence; require at least three.
	lines := strings.SplitN(s, "\n", 4)
	if len(lines) < 3 {
		return false
	}
	hits := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		rest := line[colon+1:]
		if len(rest) == 0 || rest[0] < '0' || rest[0] > '9' {
			continue
		}
		for len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
			rest = rest[1:]
		}
		if strings.HasPrefix(rest, ":") {
			hits++
		}
	}
	return hits >= 2
}

// compressDiff keeps file headers, hunk headers and changed lines, drops most
// context, and omits diff metadata that a model does not act on.
func compressDiff(s string) string {
	var out []string
	keptContext := 0
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "--- "),
			strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "@@"):
			out = append(out, line)
			keptContext = 0
		case strings.HasPrefix(line, "index "), strings.HasPrefix(line, "old mode"),
			strings.HasPrefix(line, "new mode"), strings.HasPrefix(line, "similarity index"),
			strings.HasPrefix(line, "rename "):
			// metadata: no value for the model
		case strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-"):
			out = append(out, line)
			keptContext = 0
		case keptContext < 2 && strings.TrimSpace(line) != "":
			out = append(out, line)
			keptContext++
		}
	}
	return strings.Join(out, "\n")
}

var verboseKeys = map[string]bool{
	"avatar_url":  true,
	"_links":      true,
	"node_id":     true,
	"etag":        true,
	"created_at":  true,
	"updated_at":  true,
	"permissions": true,
	"metadata":    true,
	"trace_id":    true,
	"request_id":  true,
	"sha":         true,
	"md5":         true,
}

// compressJSON minifies and removes fields that are almost never needed. If the
// value cannot be parsed the content is left alone rather than risk corrupting
// it.
func compressJSON(s string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", false
	}
	v = stripVerbose(v)
	raw, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func stripVerbose(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, inner := range val {
			if verboseKeys[strings.ToLower(k)] {
				continue
			}
			out[k] = stripVerbose(inner)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, inner := range val {
			out[i] = stripVerbose(inner)
		}
		return out
	default:
		return v
	}
}

// dedupeLines collapses runs of identical lines and repeated occurrences, which
// is what makes long logs and grep output cheap without hiding what they say.
func dedupeLines(s string) string {
	lines := strings.Split(s, "\n")

	// First pass: collapse adjacent duplicates.
	var collapsed []string
	var counts []int
	for _, line := range lines {
		if n := len(collapsed); n > 0 && collapsed[n-1] == line {
			counts[n-1]++
			continue
		}
		collapsed = append(collapsed, line)
		counts = append(counts, 1)
	}

	// Second pass: keep the first occurrence of a line that repeats a lot far
	// apart (timestamps aside, a repeated stack frame says nothing new).
	var (
		out      []string
		seen     = make(map[string]int, len(collapsed))
		reported = make(map[string]bool, 4)
	)
	for i, line := range collapsed {
		count := counts[i]
		key := strings.TrimSpace(line)
		if key != "" {
			seen[key]++
		}
		if count > 1 {
			out = append(out, fmt.Sprintf("%s ... [lgllm: repeated %d times]", line, count))
			continue
		}
		if key != "" && seen[key] > 1 && len(key) > 12 {
			if !reported[key] {
				reported[key] = true
				out = append(out, line)
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// SummarizeFilters returns a stable, comma-separated list for logs.
func SummarizeFilters(filters []string) string {
	if len(filters) == 0 {
		return ""
	}
	sorted := append([]string(nil), filters...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}
