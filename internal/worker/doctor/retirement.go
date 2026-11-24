package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Retirement finds what the Controller and the Worker of 0.4 left on this
// machine and, on Fix, removes the part that is only a service or a shim: the
// units and the cf binaries. Data and credentials are only listed, with the
// command to delete them: they are the user's to delete.
//
// Every dependency is a field, so a test runs it over a temporary home with a
// fake service manager.
type Retirement struct {
	Home string
	GOOS string
	UID  int
	// Run runs a tool of the service manager (systemctl, launchctl). Its error
	// is not fatal: a unit that is not loaded is a unit already stopped.
	Run func(ctx context.Context, name string, args ...string) error
}

const (
	retiredName = "legacy"
	// ourBinaryMark is in every binary built from this repository (its module path).
	ourBinaryMark = "github.com/Autarquico/code"
	launchDaemons = "/Library/LaunchDaemons"
)

type leftover struct {
	name   string // component name
	label  string // what it is, for the message
	path   string
	system bool // a LaunchDaemon: only root can remove it
	data   bool // a directory: listed, never removed
	unit   string
}

// Check reports what is left, changing nothing.
func (r Retirement) Check() []Component {
	return r.report(r.find(), nil)
}

// Fix removes the units and the shims that Check reports, and reports what it
// did. A second Fix finds nothing and says so.
func (r Retirement) Fix(ctx context.Context) []Component {
	found := r.find()
	results := map[string]error{}
	removedUnit := false
	for _, l := range found {
		if l.system || l.data {
			continue
		}
		if l.unit != "" {
			r.stop(ctx, l)
			removedUnit = true
		}
		results[l.name] = removeFile(l.path)
	}
	if removedUnit && r.GOOS == "linux" {
		_ = r.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return r.report(found, results)
}

func (r Retirement) stop(ctx context.Context, l leftover) {
	if r.GOOS == "darwin" {
		_ = r.Run(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", r.UID, l.unit))
		return
	}
	_ = r.Run(ctx, "systemctl", "--user", "disable", "--now", l.unit+".service")
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// report turns the leftovers into components. results is nil for a Check.
func (r Retirement) report(found []leftover, results map[string]error) []Component {
	if len(found) == 0 {
		return []Component{ok(retiredName, "sin restos del Controller ni del Worker")}
	}
	out := make([]Component, 0, len(found))
	for _, l := range found {
		out = append(out, r.component(l, results))
	}
	return out
}

func (r Retirement) component(l leftover, results map[string]error) Component {
	switch {
	case l.system:
		return Component{
			Name: l.name, Status: ComponentWarn, Message: l.label + " del sistema: " + l.path,
			Action: fmt.Sprintf("sudo launchctl bootout system/%s && sudo rm %s", l.unit, l.path),
		}
	case l.data:
		return Component{
			Name: l.name, Status: ComponentWarn, Message: "datos del Controller o del Worker: " + l.path,
			Action: "tras validar la red de peers: rm -rf " + l.path + " (ver docs/runbooks/retire-controller.md)",
		}
	}
	err, done := results[l.name]
	switch {
	case results == nil:
		return Component{Name: l.name, Status: ComponentWarn, Message: l.label + " sigue instalado: " + l.path, Action: "cflk doctor --fix"}
	case !done || err == nil:
		return ok(l.name, l.label+" retirado: "+l.path)
	default:
		return Component{
			Name: l.name, Status: ComponentFail, Message: "no se pudo retirar " + l.path + ": " + err.Error(),
			Action: "rm " + l.path,
		}
	}
}

// find lists the leftovers: units, shims and data, in that order.
func (r Retirement) find() []leftover {
	var out []leftover
	out = append(out, r.units()...)
	out = append(out, r.shims()...)
	out = append(out, r.data()...)
	return out
}

func (r Retirement) units() []leftover {
	type place struct {
		dir, ext string
		prefixes []string
		system   bool
	}
	var places []place
	switch r.GOOS {
	case "linux":
		places = []place{{
			dir: filepath.Join(r.Home, ".config", "systemd", "user"), ext: ".service",
			prefixes: []string{"codeflock-controller", "codeflock-worker", "cf-controller", "cf-worker"},
		}}
	case "darwin":
		prefixes := []string{"com.codeflock.controller", "com.codeflock.worker", "com.codefabric.controller", "com.codefabric.worker"}
		places = []place{
			{dir: filepath.Join(r.Home, "Library", "LaunchAgents"), ext: ".plist", prefixes: prefixes},
			{dir: launchDaemons, ext: ".plist", prefixes: prefixes, system: true},
		}
	}
	var out []leftover
	for _, p := range places {
		for _, prefix := range p.prefixes {
			matches, _ := filepath.Glob(filepath.Join(p.dir, prefix+"*"+p.ext))
			for _, m := range matches {
				unit := strings.TrimSuffix(filepath.Base(m), p.ext)
				out = append(out, leftover{
					name: "legacy:unit:" + unit, label: kindOf(unit), path: m, system: p.system, unit: unit,
				})
			}
		}
	}
	return out
}

func kindOf(unit string) string {
	if strings.Contains(unit, "controller") {
		return "servicio del Controller"
	}
	return "servicio del Worker"
}

// shims are the 0.4 binaries next to cflk. A file called cf that is not ours
// (Cloud Foundry has one) is left alone.
func (r Retirement) shims() []leftover {
	var out []leftover
	for _, name := range []string{"cf", "cf.prev"} {
		path := filepath.Join(r.Home, ".local", "bin", name)
		if isOurs(path) {
			out = append(out, leftover{name: "legacy:bin:" + name, label: "binario " + name, path: path})
		}
	}
	return out
}

func isOurs(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return err == nil && strings.HasPrefix(filepath.Base(target), "cf")
	}
	if !info.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(path) //nolint:gosec // the path is a fixed name under the home
	return err == nil && bytes.Contains(content, []byte(ourBinaryMark))
}

// data lists the directories of the Controller and the Worker that 0.5 does not use.
func (r Retirement) data() []leftover {
	var out []leftover
	for _, base := range [][]string{{".local", "share"}, {".config"}} {
		for _, app := range []string{"codeflock", "codefabric"} {
			path := filepath.Join(append(append([]string{r.Home}, base...), app)...)
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				out = append(out, leftover{name: "legacy:data:" + path, label: "datos", path: path, data: true})
			}
		}
	}
	return out
}
