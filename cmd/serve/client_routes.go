package main

import "strings"

// Include SDK inference aliases: a different URL must not bypass client policy.
func clientAPIPath(path string) bool {
	for _, prefix := range []string{"/v1", "/v1beta", "/openai/v1", "/backend-api/codex"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
