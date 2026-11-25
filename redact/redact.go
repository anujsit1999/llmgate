// Package redact scrubs PII and secrets from prompt text before it is sent
// to an upstream provider.
//
// Regex-based redaction is best-effort defence in depth, not a guarantee:
// it catches common, well-structured identifiers (emails, card numbers,
// cloud keys) and will miss free-form PII such as names or addresses.
package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// Type names a class of sensitive data.
type Type string

// Supported redaction types, applied in this order (most specific first).
const (
	Secret     Type = "secret"
	Email      Type = "email"
	CreditCard Type = "credit_card"
	SSN        Type = "ssn"
	Phone      Type = "phone"
)

// All lists every supported type in application order.
var All = []Type{Secret, Email, CreditCard, SSN, Phone}

type rule struct {
	typ   Type
	re    *regexp.Regexp
	check func(string) bool // optional extra validation (e.g. Luhn)
}

var rules = map[Type]rule{
	Secret: {typ: Secret, re: regexp.MustCompile(
		`\b(?:sk-ant-[A-Za-z0-9_\-]{20,}|sk-[A-Za-z0-9_\-]{20,}|lg_[A-Za-z0-9_\-]{40,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AIza[0-9A-Za-z_\-]{35})`)},
	Email:      {typ: Email, re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	CreditCard: {typ: CreditCard, re: regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`), check: luhn},
	SSN:        {typ: SSN, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	Phone:      {typ: Phone, re: regexp.MustCompile(`(?:\+\d{1,3}[\s.\-]?)?\(?\b\d{3}\)?[\s.\-]\d{3}[\s.\-]\d{4}\b`)},
}

// Redactor applies a configured set of rules.
type Redactor struct {
	rules []rule
}

// New builds a Redactor for the given type names. An empty list means all types.
func New(types []string) (*Redactor, error) {
	if len(types) == 0 {
		return &Redactor{rules: rulesFor(All)}, nil
	}
	want := map[Type]bool{}
	for _, t := range types {
		if _, ok := rules[Type(t)]; !ok {
			return nil, fmt.Errorf("unknown redaction type %q", t)
		}
		want[Type(t)] = true
	}
	var ordered []Type
	for _, t := range All {
		if want[t] {
			ordered = append(ordered, t)
		}
	}
	return &Redactor{rules: rulesFor(ordered)}, nil
}

func rulesFor(types []Type) []rule {
	out := make([]rule, 0, len(types))
	for _, t := range types {
		out = append(out, rules[t])
	}
	return out
}

// Redact returns text with matches replaced by [REDACTED_<TYPE>] and adds
// per-type match counts to counts (which may be nil).
func (r *Redactor) Redact(text string, counts map[Type]int) string {
	for _, rl := range r.rules {
		label := "[REDACTED_" + strings.ToUpper(string(rl.typ)) + "]"
		text = rl.re.ReplaceAllStringFunc(text, func(m string) string {
			if rl.check != nil && !rl.check(m) {
				return m
			}
			if counts != nil {
				counts[rl.typ]++
			}
			return label
		})
	}
	return text
}

// luhn validates a card number candidate (digits with optional spaces/dashes).
func luhn(s string) bool {
	sum, n, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '-' {
			continue
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		n++
	}
	return n >= 13 && sum%10 == 0
}
