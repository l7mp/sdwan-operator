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
		if legacy, _ := legacyListName("default", name); got == legacy {
			t.Fatalf("%s: %q is the legacy name", name, got)
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
