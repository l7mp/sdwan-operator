package sdwan

import "strings"

const (
	nameSeparator = "___"
)

func convertK8sNameToSdWan(namespace, name string) string {
	return namespace + nameSeparator + name
}

func createPrefix(search string) string {
	prefix := search
	if len(search) > 20 {
		prefix = search[:20]
	}

	return prefix
}

func findByPrefix(m map[string]string, search string) (string, bool) {
	prefix := createPrefix(search)

	for key := range m {
		if strings.HasPrefix(key, prefix) {
			return m[key], true
		}
	}
	return "", false
}

func findKeyByPrefix(m map[string]string, search string) (string, bool) {
	prefix := createPrefix(search)

	for key := range m {
		if strings.HasPrefix(key, prefix) {
			return key, true
		}
	}
	return "", false
}
