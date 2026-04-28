package discovery

import "strings"

func ModelSegments(id string) []string {
	id = strings.ToLower(id)
	return strings.Split(id, "/")
}
