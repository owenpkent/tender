package money

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Amount
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"12.50", 12_500_000},
		{"0.000001", 1},
		{"-3.25", -3_250_000},
		{"+3.25", 3_250_000},
		{".5", 500_000},
		{" 7.000000 ", 7_000_000},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2.3", "1.0000001", "1e6", "9223372036854775807"} {
		if _, err := Parse(in); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q): want ErrMalformed, got %v", in, err)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, want := range []Amount{0, 1, -1, 12_500_000, -3_250_000, 999_999_999_999} {
		got, err := Parse(want.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", want.String(), err)
		}
		if got != want {
			t.Errorf("round trip of %d via %q gave %d", want, want.String(), got)
		}
	}
}
