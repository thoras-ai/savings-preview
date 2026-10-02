package prometheus

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ParseSelector reads label matchers such as `cluster="prod-us"` or
// `{cluster=~"prod-.*",region="us-east-1"}` and returns them in canonical
// form, without braces. An unquoted value is quoted, so `cluster=prod-us`
// works without shell quoting.
func ParseSelector(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return "", nil
	}
	var out []string
	for rest := s; rest != ""; {
		m, r, err := parseMatcher(rest)
		if err != nil {
			return "", fmt.Errorf("selector %q: %w", s, err)
		}
		out = append(out, m)
		rest = strings.TrimSpace(r)
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return "", fmt.Errorf("selector %q: expected a comma before %q", s, rest)
		}
		rest = strings.TrimSpace(rest[1:])
	}
	return strings.Join(out, ","), nil
}

func parseMatcher(s string) (matcher, rest string, err error) {
	i := strings.IndexAny(s, "=!")
	if i <= 0 {
		return "", "", fmt.Errorf("%q is not a label matcher (want name=\"value\")", s)
	}
	name := strings.TrimSpace(s[:i])
	if !labelName.MatchString(name) {
		return "", "", fmt.Errorf("%q is not a label name", name)
	}
	if name == "__name__" {
		return "", "", fmt.Errorf("matching on the metric name is not allowed")
	}
	var op string
	for _, o := range []string{"=~", "!~", "!=", "="} {
		if strings.HasPrefix(s[i:], o) {
			op = o
			break
		}
	}
	if op == "" {
		return "", "", fmt.Errorf("%q: unknown operator", s)
	}
	s = strings.TrimSpace(s[i+len(op):])
	var val string
	if s != "" && (s[0] == '"' || s[0] == '\'' || s[0] == '`') {
		end := closingQuote(s)
		if end < 0 {
			return "", "", fmt.Errorf("unterminated string in %q", s)
		}
		raw := s[:end+1]
		if raw[0] == '\'' {
			raw = `"` + strings.ReplaceAll(strings.ReplaceAll(raw[1:len(raw)-1], `\'`, `'`), `"`, `\"`) + `"`
		}
		if val, err = strconv.Unquote(raw); err != nil {
			return "", "", fmt.Errorf("bad string %s: %w", s[:end+1], err)
		}
		s = s[end+1:]
	} else {
		end := strings.IndexAny(s, ",")
		if end < 0 {
			end = len(s)
		}
		val, s = strings.TrimSpace(s[:end]), s[end:]
		if strings.ContainsAny(val, "\"'`{}") {
			return "", "", fmt.Errorf("value %q must be quoted", val)
		}
	}
	if op == "=~" || op == "!~" {
		if _, err := regexp.Compile("^(?:" + val + ")$"); err != nil {
			return "", "", fmt.Errorf("bad regular expression %q: %w", val, err)
		}
	}
	return name + op + strconv.Quote(val), s, nil
}

// closingQuote returns the index of the quote that closes the string starting
// at s[0], or -1.
func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q != '`':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

// withSelector adds the matchers to every vector selector in q. Every metric
// in the adapter's queries is written with braces, so each "{" outside a
// string opens a selector.
func withSelector(q, matchers string) string {
	if matchers == "" {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8*len(matchers))
	var quote byte
	for i := 0; i < len(q); i++ {
		c := q[i]
		b.WriteByte(c)
		switch {
		case quote != 0:
			if c == '\\' && quote != '`' && i+1 < len(q) {
				i++
				b.WriteByte(q[i])
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '{':
			b.WriteString(matchers)
			if i+1 < len(q) && q[i+1] != '}' {
				b.WriteByte(',')
			}
		}
	}
	return b.String()
}
