package sessionid

import (
	"regexp"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"  ses_abc  ", "ses_abc"},
		{"ses\r\n_abc", "ses_abc"},
		{"ses\x00_abc", "ses_abc"},
		{"ses_abc\tdef", "ses_abcdef"},
		{"sés_abc", "ss_abc"},
	}
	for _, tc := range tests {
		if got := Sanitize(tc.in); got != tc.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	long := strings.Repeat("x", MaxLength+20)
	if got := Sanitize(long); len(got) != MaxLength {
		t.Errorf("length = %d, want %d", len(got), MaxLength)
	}
}

func TestFingerprintIsStableAndCanonical(t *testing.T) {
	canonical := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9a-zA-Z]{14}$`)
	first := Fingerprint("conversation one")
	second := Fingerprint("conversation one")
	other := Fingerprint("conversation two")

	if first != second {
		t.Errorf("same seed produced %q and %q", first, second)
	}
	if first == other {
		t.Errorf("different seeds produced the same id %q", first)
	}
	if !canonical.MatchString(first) {
		t.Errorf("id = %q, want the canonical ses_ shape", first)
	}
}
