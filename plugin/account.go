package plugin

import "strings"

func isPlausibleEmail(s string) bool {
	s = strings.TrimSpace(s)
	at := strings.Index(s, "@")
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	dot := strings.LastIndex(s[at+1:], ".")
	return dot >= 0 && dot < len(s[at+1:])-1
}
