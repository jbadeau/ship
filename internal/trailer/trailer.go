// Package trailer parses "Key: value" trailer lines from commit messages and MR descriptions.
package trailer

import "strings"

// All returns every value of key, in order.
func All(text, key string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if ok && k == key {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// First returns the first value of key, or "".
func First(text, key string) string {
	if v := All(text, key); len(v) > 0 {
		return v[0]
	}
	return ""
}
