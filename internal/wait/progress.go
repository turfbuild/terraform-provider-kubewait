package wait

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxFieldLen caps one rendered progress field.
const maxFieldLen = 240

// FieldPath is a parsed progress_fields entry: map keys (string) and list
// indices (int).
type FieldPath struct {
	Source string
	elems  []any
}

// ParseFieldPath parses a dot path. Keys containing dots are bracketed and
// quoted, and list elements are indexed:
//
//	status.conditions
//	metadata.labels["app.kubernetes.io/name"]
//	spec.containers[0].image
func ParseFieldPath(src string) (FieldPath, error) {
	fp := FieldPath{Source: src}
	bad := func(why string) (FieldPath, error) {
		return FieldPath{}, fmt.Errorf("invalid field path %q: %s", src, why)
	}
	s := src
	if s == "" {
		return bad("empty")
	}
	expectKey := true
	for len(s) > 0 {
		switch {
		case s[0] == '[':
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return bad("unclosed [")
			}
			inner := s[1:end]
			if strings.HasPrefix(inner, `"`) {
				k, err := strconv.Unquote(inner)
				if err != nil {
					return bad("bad quoted key " + inner)
				}
				fp.elems = append(fp.elems, k)
			} else {
				i, err := strconv.Atoi(inner)
				if err != nil || i < 0 {
					return bad("index must be a quoted key or a non-negative integer, got " + inner)
				}
				fp.elems = append(fp.elems, i)
			}
			s = s[end+1:]
			expectKey = false
		case s[0] == '.':
			if expectKey {
				return bad("empty segment")
			}
			s = s[1:]
			expectKey = true
			if s == "" {
				return bad("trailing .")
			}
		default:
			if !expectKey {
				return bad("expected . or [ after " + fmt.Sprint(fp.elems[len(fp.elems)-1]))
			}
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			fp.elems = append(fp.elems, s[:end])
			s = s[end:]
			expectKey = false
		}
	}
	return fp, nil
}

// Lookup resolves the path in obj.
func (fp FieldPath) Lookup(obj map[string]any) (any, bool) {
	var cur any = obj
	for _, e := range fp.elems {
		switch k := e.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[k]; !ok {
				return nil, false
			}
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				return nil, false
			}
			cur = l[k]
		}
	}
	return cur, true
}

// Snapshot is what a progress message reports.
type Snapshot struct {
	Outcome   Outcome
	Elapsed   time.Duration
	Remaining time.Duration
	// Settling, when non-zero, is how long an unsettled success or failure
	// has held, against Settle.
	Settling time.Duration
	Settle   time.Duration
	// Note is an extra status such as a retried API error or an unserved kind.
	Note string
}

// FormatProgress renders a progress message. Line 1 carries the verdict,
// the reason and the budget; one line per observed object follows, with its
// progress_fields.
func FormatProgress(s *Spec, snap Snapshot) string {
	var b strings.Builder
	b.WriteString(snap.Outcome.Verdict.String())
	b.WriteString(": ")
	b.WriteString(snap.Outcome.Reason)
	if snap.Note != "" {
		b.WriteString(" [" + snap.Note + "]")
	}
	fmt.Fprintf(&b, " · %s elapsed, %s left", FormatDuration(snap.Elapsed), FormatDuration(snap.Remaining))
	if snap.Settling > 0 || (snap.Outcome.Verdict != Pending && snap.Settle > 0) {
		fmt.Fprintf(&b, " · %s held %s of %s", snap.Outcome.Verdict, FormatDuration(snap.Settling), FormatDuration(snap.Settle))
	}
	objs := snap.Outcome.Objects
	for i, o := range objs {
		if i == maxListed {
			fmt.Fprintf(&b, "\n  and %d more", len(objs)-maxListed)
			break
		}
		b.WriteString("\n  ")
		b.WriteString(RefOf(o).String())
		for _, f := range s.ProgressFields {
			v, ok := f.Lookup(o)
			if !ok {
				fmt.Fprintf(&b, " %s=<none>", f.Source)
				continue
			}
			fmt.Fprintf(&b, " %s=%s", f.Source, renderValue(v))
		}
	}
	return b.String()
}

// renderValue renders a field compactly. A list of conditions renders as
// Type=Status(Reason); anything else as compact JSON. Long values are cut.
func renderValue(v any) string {
	var s string
	if conds, ok := conditionList(v); ok {
		s = conds
	} else if str, ok := v.(string); ok {
		s = str
	} else {
		raw, err := json.Marshal(v)
		if err != nil {
			s = fmt.Sprint(v)
		} else {
			s = string(raw)
		}
	}
	if len(s) > maxFieldLen {
		s = s[:maxFieldLen] + "…"
	}
	return s
}

func conditionList(v any) (string, bool) {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return "", false
		}
		t, ok1 := m["type"].(string)
		st, ok2 := m["status"].(string)
		if !ok1 || !ok2 {
			return "", false
		}
		p := t + "=" + st
		if r, _ := m["reason"].(string); r != "" {
			p += "(" + r + ")"
		}
		parts = append(parts, p)
	}
	return "[" + strings.Join(parts, " ") + "]", true
}

// FormatDuration renders a duration to the second: 1h2m, 12m, 40s, 0s.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h, m, s := d/time.Hour, (d%time.Hour)/time.Minute, (d%time.Minute)/time.Second
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
