// Package greet builds greeting messages for the akari command.
package greet

import (
	"fmt"
	"strings"
)

// Greeting returns a greeting for name. It trims surrounding whitespace and
// falls back to "world" when name is empty so callers never produce a dangling
// "Hello, !".
func Greeting(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("Hello, %s!", name)
}
