package prometheus

import "net/url"

// RedactURL drops credentials and the query string from a URL so it can be
// printed or written to a report.
func RedactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(unparseable url)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
