package wait

import "fmt"

// conditionEntry returns the status.conditions entry of the given type.
func conditionEntry(obj map[string]any, typ string) (map[string]any, bool) {
	status, _ := obj["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == typ {
			return m, true
		}
	}
	return nil, false
}

// holds reports whether obj's status.conditions has an entry of c's type
// with c's status and, when c names one, c's reason. Matching is exact and
// case-sensitive. status.phase is not consulted.
func (c Condition) holds(obj map[string]any) bool {
	e, ok := conditionEntry(obj, c.Type)
	if !ok {
		return false
	}
	if s, _ := e["status"].(string); s != c.Status {
		return false
	}
	if c.HasReason {
		if r, _ := e["reason"].(string); r != c.Reason {
			return false
		}
	}
	return true
}

// observed describes what obj actually carries for c's type, for reasons:
// "Ready=False (KubeletNotReady)" or "Ready missing".
func (c Condition) observed(obj map[string]any) string {
	e, ok := conditionEntry(obj, c.Type)
	if !ok {
		return c.Type + " missing"
	}
	s, _ := e["status"].(string)
	if r, _ := e["reason"].(string); r != "" {
		return fmt.Sprintf("%s=%s (%s)", c.Type, s, r)
	}
	return c.Type + "=" + s
}
