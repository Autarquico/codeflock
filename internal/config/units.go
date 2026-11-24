package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Unit is a service of 0.4 (cf-* or codeflock-*, the CodeFabric and Code Flock
// names) that is installed.
type Unit struct {
	Name   string // as the manager knows it
	Kind   string // "worker" or "controller"
	Path   string // its unit file or property list
	System bool   // a LaunchDaemon of the system, which a user cannot manage
}

// OldUnits are the services of 0.4 installed under the exact names it gave
// them, running or not: the unit files of systemd --user on Linux, the
// LaunchAgents of the user and the LaunchDaemons of the system on macOS. They
// are found by their files, which are what survives a stopped unit. A unit that
// merely starts with the same prefix (cf-worker-backup.service) is the user's.
func OldUnits(home, goos string) []Unit {
	var units []Unit
	add := func(dir, name, suffix string) {
		path := filepath.Join(dir, name+suffix)
		if _, err := os.Stat(path); err == nil {
			units = append(units, Unit{Name: name, Kind: KindOf(name), Path: path, System: dir == launchDaemonsDir})
		}
	}
	switch goos {
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		for _, name := range []string{"cf-worker", "cf-controller", "codeflock-worker", "codeflock-controller"} {
			add(dir, name, ".service")
		}
	case "darwin":
		for _, dir := range []string{filepath.Join(home, "Library", "LaunchAgents"), launchDaemonsDir} {
			for _, name := range []string{"com.codefabric.worker", "com.codefabric.controller", "com.codeflock.worker", "com.codeflock.controller"} {
				add(dir, name, ".plist")
			}
		}
	}
	return units
}

// KindOf is "worker" or "controller": which of the two a service of the old name is.
func KindOf(name string) string {
	if strings.Contains(name, "controller") {
		return "controller"
	}
	return "worker"
}

const launchDaemonsDir = "/Library/LaunchDaemons"
