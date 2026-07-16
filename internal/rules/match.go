package rules

import "strings"

// Request is the normalized request the engine passes to the rules engine.
type Request struct {
	Method  string
	Path    string
	Headers map[string]string
}

// Matches reports whether req satisfies all non-empty fields of m.
// Path supports '*' (one segment, no '/') and '**' (zero or more segments).
func (m Match) Matches(req Request) bool {
	if m.Method != "" && !strings.EqualFold(m.Method, req.Method) {
		return false
	}
	if m.Path != "" && !pathMatches(m.Path, req.Path) {
		return false
	}
	for k, v := range m.Headers {
		if req.Headers[k] != v {
			return false
		}
	}
	return true
}

func pathMatches(pattern, actual string) bool {
	p := splitSegments(pattern)
	a := splitSegments(actual)
	return segMatch(p, a)
}

func splitSegments(s string) []string {
	s = strings.Trim(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// segMatch matches pattern segments against actual. '**' spans zero+ segments.
func segMatch(pat, act []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// '**' matches zero or more remaining segments; try all.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(act); i++ {
				if segMatch(pat[1:], act[i:]) {
					return true
				}
			}
			return false
		}
		if len(act) == 0 {
			return false
		}
		if pat[0] != "*" && pat[0] != act[0] {
			return false
		}
		pat, act = pat[1:], act[1:]
	}
	return len(act) == 0
}
