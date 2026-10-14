package sdwan

import (
	"strings"
	"testing"
)

func TestConvertK8sNameToSdWan(t *testing.T) {
	t.Parallel()
	if got := convertK8sNameToSdWan("default", "app"); got != "default___app" {
		t.Fatalf("short name changed: %q", got)
	}
	if got := convertK8sNameToSdWan("default", "business-"); got != "default___business-" {
		t.Fatalf("name of %d characters changed: %q", maxListNameLen-1, got)
	}

	seen := map[string]string{}
	for _, name := range []string{"business-a", "business-app-1", "business-app-10", "business-app-2", "llamacpp-backends"} {
		got := convertK8sNameToSdWan("default", name)
		if len(got) != maxListNameLen {
			t.Fatalf("%s: %q is not %d characters long", name, got, maxListNameLen)
		}
		if !strings.HasPrefix(got, "default___") {
			t.Fatalf("%s: %q does not start with the namespace", name, got)
		}
		if isLegacyListName(got, "default", name) {
			t.Fatalf("%s: %q is taken for the legacy name", name, got)
		}
		if other, ok := seen[got]; ok {
			t.Fatalf("%s and %s both map to %q", name, other, got)
		}
		seen[got] = name
		if again := convertK8sNameToSdWan("default", name); again != got {
			t.Fatalf("%s: name not stable: %q, then %q", name, got, again)
		}
	}
}

func TestJavaStringHash(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int32{
		"":      0,
		"hello": 99162322,
		// the suffix vManage 20.15 gave this name ("default___llamacpp-ba-1140021977")
		"default___llamacpp-backends-TCP-50052": 1140021977,
		"polygenelubricants":                    -2147483648, // Java's Integer.MIN_VALUE example
	} {
		if got := javaStringHash(s); got != want {
			t.Fatalf("javaStringHash(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestIsLegacyListName(t *testing.T) {
	t.Parallel()
	const service = "llamacpp-backends-TCP-50052" // "default___" + this is 37 characters
	for _, tc := range []struct {
		desc, stored, name string
		want               bool
	}{
		{"stored by vManage 20.15", "default___llamacpp-ba-1140021977", service, true},
		{"other prefix length", "default___llamacpp-b-1140021977", service, true},
		{"signed hash", "default___llamacpp-ba--1140021977", service, false},
		{"hash of another name", "default___llamacpp-ba-1140021978", service, false},
		{"same prefix, other service", "default___llamacpp-ba-1140021977", "llamacpp-backends-TCP-50053", false},
		{"prefix not of the name", "default___llamacpx-ba-1140021977", service, false},
		{"hashed name", convertK8sNameToSdWan("default", service), service, false},
		{"too long to be stored", "default___llamacpp-backends-TCP-5-1140021977", service, false},
		{"20 to 32 characters: kept whole", "default___llamacpp-backends", "llamacpp-backends", true},
		{"20 to 32 characters: not truncated", "default___llamacpp-b", "llamacpp-backends", false},
		{"exactly 20 characters", "default___business-a", "business-a", true},
		{"short name: no legacy list", "default___app", "app", false},
	} {
		if got := isLegacyListName(tc.stored, "default", tc.name); got != tc.want {
			t.Errorf("%s: isLegacyListName(%q, %q) = %v, want %v", tc.desc, tc.stored, tc.name, got, tc.want)
		}
	}

	// A negative hashCode: vManage's exact format is unknown, so both the
	// absolute value and the signed number are accepted.
	const negative = "svc-006-backends-TCP-50052"
	if h := javaStringHash("default___" + negative); h != -322838150 {
		t.Fatalf("javaStringHash = %d, want -322838150", h)
	}
	for _, stored := range []string{"default___svc-006-ba-322838150", "default___svc-006-b--322838150"} {
		if !isLegacyListName(stored, "default", negative) {
			t.Errorf("negative hash: %q not taken for the legacy name of %q", stored, negative)
		}
	}
}
