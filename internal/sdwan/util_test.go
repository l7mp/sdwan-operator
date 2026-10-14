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
	exact := convertK8sNameToSdWan("default", "business-a")
	if exact != "default___business-a" {
		t.Fatalf("name of exactly %d characters changed: %q", maxListNameLen, exact)
	}

	seen := map[string]string{exact: "business-a"}
	for _, name := range []string{"business-app-1", "business-app-10", "business-app-2", "llamacpp-backends"} {
		got := convertK8sNameToSdWan("default", name)
		if len(got) > maxListNameLen {
			t.Fatalf("%s: %q is longer than %d characters", name, got, maxListNameLen)
		}
		if !strings.HasPrefix(got, "default___") {
			t.Fatalf("%s: %q does not start with the namespace", name, got)
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
