// Package skills holds the skill that `cflk mcp install` writes where Claude
// Code or Codex read skills.
package skills

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
)

//go:embed codeflock/SKILL.md
var codeflock []byte

const (
	versionVar = "{{cflk-version}}"
	// Marker starts the line that tells a SKILL.md was written by cflk.
	Marker = "<!-- cflk-version: "
)

var versionRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// Codeflock is the SKILL.md that the cflk of this version writes.
func Codeflock(version string) ([]byte, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q cannot be written into the skill", version)
	}
	return bytes.ReplaceAll(codeflock, []byte(versionVar), []byte(version)), nil
}
