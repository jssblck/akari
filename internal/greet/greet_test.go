package greet

import "testing"

func TestGreeting(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"named":            {in: "Ada", want: "Hello, Ada!"},
		"empty falls back": {in: "", want: "Hello, world!"},
		"trims whitespace": {in: "  Grace  ", want: "Hello, Grace!"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Greeting(tc.in); got != tc.want {
				t.Errorf("Greeting(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
