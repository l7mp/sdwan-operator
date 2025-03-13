package sdwan

const (
	nameSeparator = "___"
)

func convertK8sNameToSdWan(namespace, name string) string {
	return namespace + nameSeparator + name
}
