package doctor

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxText is the longest Name, Message or Action, in bytes. A text of the
// machine (a file name, a list of components) can be anything.
const MaxText = 256

const ellipsis = "…"

// Clean makes s fit a heartbeat and a terminal: control and format characters
// become '?', and a text over MaxText bytes is cut, on a rune boundary, with an
// ellipsis.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
	if len(s) <= MaxText {
		return s
	}
	cut := MaxText - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// CleanComponent applies Clean to every text of c.
func CleanComponent(c Component) Component {
	c.Name, c.Message, c.Action = Clean(c.Name), Clean(c.Message), Clean(c.Action)
	return c
}
