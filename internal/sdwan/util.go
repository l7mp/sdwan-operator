package sdwan

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	nameSeparator = "___"
	// maxListNameLen is the longest list name vManage keeps: it truncates
	// longer names.
	maxListNameLen = 20
	nameHashLen    = 8
)

// convertK8sNameToSdWan returns the vManage list name of a Kubernetes object.
// A name longer than maxListNameLen is cut short and ends in a hash of the full
// name, so that vManage keeps it whole and it stays unique.
func convertK8sNameToSdWan(namespace, name string) string {
	n := namespace + nameSeparator + name
	if len(n) <= maxListNameLen {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	return n[:maxListNameLen-nameHashLen-1] + "_" + hex.EncodeToString(sum[:])[:nameHashLen]
}
