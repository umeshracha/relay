package routing

import "strings"

type Route struct {
	Method string
	Path   string
}

// Segments rejects ambiguous paths before checking an imported route. Upstreams
// can otherwise normalize traversal or separators after the gateway check.
func Segments(path string) ([]string, bool) {
	if path == "/" {
		return nil, true
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%\\?#; ") {
		return nil, false
	}
	for _, char := range path {
		if char < 32 || char == 127 {
			return nil, false
		}
	}
	parts := strings.Split(path[1:], "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func Parameter(segment string) bool {
	name, ok := strings.CutPrefix(segment, "{")
	if !ok || !strings.HasSuffix(name, "}") {
		return false
	}
	name = strings.TrimSuffix(name, "}")
	return name != "" && !strings.ContainsAny(name, "{} ")
}

func (r Route) Matches(method string, parts []string) bool {
	if r.Method != method {
		return false
	}
	pattern, ok := Segments(r.Path)
	if !ok || len(pattern) != len(parts) {
		return false
	}
	for index, segment := range pattern {
		if segment != parts[index] && !Parameter(segment) {
			return false
		}
	}
	return true
}
