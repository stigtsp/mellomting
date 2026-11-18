package topview

import (
	"net/netip"
	"regexp"
	"strconv"
)

// Redactor replaces IP addresses in displayed text with labels that
// stay the same for the session (ip-1, ip-2, …), so rows from one
// client still group together on a shared screen without revealing the
// address. It rewrites every IP literal, wherever it appears: remotes,
// user agents, names.
type Redactor struct {
	labels map[netip.Addr]string
}

// NewRedactor has no labels yet.
func NewRedactor() *Redactor { return &Redactor{labels: map[netip.Addr]string{}} }

var (
	ipv4 = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6 = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:%[0-9A-Za-z._-]+)?`)
)

// Apply returns s with every address replaced by its label.
func (r *Redactor) Apply(s string) string {
	s = ipv4.ReplaceAllStringFunc(s, r.label)
	return ipv6.ReplaceAllStringFunc(s, r.label)
}

func (r *Redactor) label(lit string) string {
	a, err := netip.ParseAddr(lit)
	if err != nil {
		return lit
	}
	a = a.WithZone("").Unmap()
	l, ok := r.labels[a]
	if !ok {
		l = "ip-" + strconv.Itoa(len(r.labels)+1)
		r.labels[a] = l
	}
	return l
}
