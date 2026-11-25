package redact

import "testing"

func TestRedact(t *testing.T) {
	r, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, in, want string
		typ            Type
	}{
		{"email", "mail me at jane.doe@example.com now", "mail me at [REDACTED_EMAIL] now", Email},
		{"visa test card", "card 4111 1111 1111 1111 ok", "card [REDACTED_CREDIT_CARD] ok", CreditCard},
		{"ssn", "ssn 123-45-6789", "ssn [REDACTED_SSN]", SSN},
		{"us phone", "call (415) 555-0123 today", "call [REDACTED_PHONE] today", Phone},
		{"anthropic key", "key sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "key [REDACTED_SECRET]", Secret},
		{"aws key", "AKIAIOSFODNN7EXAMPLE leaked", "[REDACTED_SECRET] leaked", Secret},
		{"plain text untouched", "the quick brown fox", "the quick brown fox", ""},
		{"non-luhn digits untouched", "order 1234 5678 9012 3456", "order 1234 5678 9012 3456", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counts := map[Type]int{}
			got := r.Redact(tc.in, counts)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.typ != "" && counts[tc.typ] != 1 {
				t.Fatalf("counts = %v, want one %s", counts, tc.typ)
			}
		})
	}
}

func TestNewSubsetAndUnknown(t *testing.T) {
	r, err := New([]string{"email"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Redact("a@b.co 123-45-6789", nil); got != "[REDACTED_EMAIL] 123-45-6789" {
		t.Fatalf("subset redaction wrong: %q", got)
	}
	if _, err := New([]string{"dna"}); err == nil {
		t.Fatal("expected error for unknown type")
	}
}
