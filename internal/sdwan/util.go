package sdwan

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	nameSeparator = "___"
	// maxListNameLen is the length of a hashed list name: names this long or
	// longer are cut short and end in a hash of the full name.
	maxListNameLen = 20
	nameHashLen    = 8
	// vManageMaxNameLen is the longest list name vManage keeps as given. It
	// stores a longer name as a prefix of it, "-" and the Java hashCode of the
	// full name, e.g. "default___llamacpp-backends-TCP-50052" as
	// "default___llamacpp-ba-1140021977" (vManage 20.15).
	vManageMaxNameLen = 32
)

// convertK8sNameToSdWan returns the vManage list name of a Kubernetes object.
// A name of maxListNameLen or more characters is cut short and ends in a hash
// of the full name, so that vManage keeps it whole and it stays unique. Even a
// name of exactly maxListNameLen characters is hashed, so that it cannot be
// taken for the legacy name of another object (see isLegacyListName).
func convertK8sNameToSdWan(namespace, name string) string {
	n := namespace + nameSeparator + name
	if len(n) < maxListNameLen {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	return n[:maxListNameLen-nameHashLen-1] + "_" + hex.EncodeToString(sum[:])[:nameHashLen]
}

// isLegacyListName tells if stored is the name vManage gave the list of a
// Kubernetes object before names were hashed. The operator sent the full name,
// which vManage kept if it was short enough, and otherwise stored as a prefix
// of it, "-" and its Java hashCode. Only names with a positive hashCode of ten
// digits after a 21-character prefix were seen, so neither the prefix length
// nor the sign of the hash is assumed. Shorter names have not changed and
// have no legacy list.
func isLegacyListName(stored, namespace, name string) bool {
	n := namespace + nameSeparator + name
	if len(n) < maxListNameLen {
		return false
	}
	if len(n) <= vManageMaxNameLen {
		return stored == n
	}
	if len(stored) > vManageMaxNameLen {
		return false
	}
	h := int64(javaStringHash(n))
	abs := h
	if abs < 0 {
		abs = -abs
	}
	for _, suffix := range []string{"-" + strconv.FormatInt(abs, 10), "-" + strconv.FormatInt(h, 10)} {
		if prefix, ok := strings.CutSuffix(stored, suffix); ok && prefix != "" && strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// javaStringHash returns Java's String.hashCode of s, which vManage uses in
// the names it stores for long list names.
func javaStringHash(s string) int32 {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = 31*h + int32(u)
	}
	return h
}
