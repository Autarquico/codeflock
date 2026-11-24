package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codeflock/internal/config"
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
	// Run runs a tool of the service manager (systemctl, launchctl) and returns
	// its output. An error that has an ExitCode() int is the tool refusing; any
	// other is the tool not running (permission, cancellation).
	Run func(ctx context.Context, name string, args ...string) (string, error)
}

const (
	retiredName = "legacy"
	// The codes `launchctl print` exits with when the agent is not there: the
	// domain does not exist or has no such service. Any other failure is not
	// proof that it is stopped.
	launchctlNoDomain  = 112
	launchctlNoService = 113
	// ourBinaryMark is in every binary built from this repository (its module path).
	ourBinaryMark = "github.com/Autarquico/code"
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
			// A unit that is still running must keep its file: it is what
			// the person needs to stop it by hand.
			if err := r.stop(ctx, l); err != nil {
				results[l.name] = fmt.Errorf("no se pudo parar: %w", err)
				continue
			}
			removedUnit = true
		}
		results[l.name] = removeFile(l.path)
	}
	if removedUnit && r.GOOS == "linux" {
		_, _ = r.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return r.report(found, results)
}

// stop stops the unit, and says why it did not. A Worker is only stopped when
// that leaves its Runs alive: they are the user's work.
func (r Retirement) stop(ctx context.Context, l leftover) error {
	if r.GOOS == "darwin" {
		return r.bootout(ctx, l)
	}
	if config.KindOf(l.unit) == "worker" {
		if err := r.checkKillMode(ctx, l.unit); err != nil {
			return err
		}
	}
	_, err := r.Run(ctx, "systemctl", "--user", "disable", "--now", l.unit+".service")
	return err
}

// checkKillMode refuses a running Worker unit whose KillMode is not process
// (a drop-in can set it): stopping it would kill the Runs it started.
func (r Retirement) checkKillMode(ctx context.Context, unit string) error {
	out, err := r.Run(ctx, "systemctl", "--user", "show", "-p", "KillMode", "-p", "ActiveState", unit+".service")
	if err != nil {
		return fmt.Errorf("no se pudo leer %s.service: %w", unit, err)
	}
	state := map[string]string{}
	for line := range strings.Lines(out) {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			state[key] = value
		}
	}
	if slices.Contains([]string{"inactive", "failed", ""}, state["ActiveState"]) || state["KillMode"] == "process" {
		return nil
	}
	return fmt.Errorf("el KillMode de %s.service es %q, no process: pararlo mataría los Runs que lanzó; "+
		"quita el override o sigue docs/runbooks/retire-controller.md", unit, state["KillMode"])
}

// bootout boots the agent out of the two domains it may be loaded in, the
// graphical session and the background one of a Mac with nobody logged in, and
// confirms it is gone from both: launchd refuses to boot out a job that is not
// loaded, which is the job already stopped, but only an answer of "not found"
// proves it. A Worker that does not abandon its process group would take its
// Runs with it.
func (r Retirement) bootout(ctx context.Context, l leftover) error {
	if config.KindOf(l.unit) == "worker" {
		plist, err := os.ReadFile(l.path) //nolint:gosec // the path comes from the fixed names of OldUnits
		if err == nil && !bytes.Contains(plist, []byte("AbandonProcessGroup")) {
			return fmt.Errorf("%s no abandona su grupo de procesos: pararlo mataría los Runs que lanzó; "+
				"sigue docs/runbooks/retire-controller.md", l.unit)
		}
	}
	for _, domain := range []string{fmt.Sprintf("gui/%d", r.UID), fmt.Sprintf("user/%d", r.UID)} {
		target := domain + "/" + l.unit
		_, bootErr := r.Run(ctx, "launchctl", "bootout", target)
		_, err := r.Run(ctx, "launchctl", "print", target)
		switch {
		case err == nil:
			still := fmt.Errorf("sigue cargado en %s tras launchctl bootout", domain)
			if bootErr != nil {
				return fmt.Errorf("%w: %w", still, bootErr)
			}
			return still
		case !notLoaded(err):
			return fmt.Errorf("no se pudo comprobar %s: %w", target, err)
		}
	}
	return nil
}

// notLoaded says whether err is launchctl saying the agent is not there.
func notLoaded(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && (exit.ExitCode() == launchctlNoDomain || exit.ExitCode() == launchctlNoService)
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
	err := results[l.name]
	switch {
	case results == nil:
		return Component{Name: l.name, Status: ComponentWarn, Message: l.label + " sigue instalado: " + l.path, Action: "cflk doctor --fix"}
	case err == nil:
		return ok(l.name, l.label+" retirado: "+l.path)
	default:
		return Component{
			Name: l.name, Status: ComponentFail, Message: "no se pudo retirar " + l.path + ": " + err.Error(),
			Action: l.manualFix(r.GOOS),
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
	var out []leftover
	for _, u := range config.OldUnits(r.Home, r.GOOS) {
		out = append(out, leftover{
			name: "legacy:unit:" + u.Name, label: "servicio del " + titleOf(u.Kind), path: u.Path, system: u.System, unit: u.Name,
		})
	}
	return out
}

func titleOf(kind string) string {
	if kind == "controller" {
		return "Controller"
	}
	return "Worker"
}

// manualFix is what to type when --fix could not do it.
func (l leftover) manualFix(goos string) string {
	switch {
	case l.unit != "" && goos == "darwin":
		return fmt.Sprintf("launchctl bootout gui/$(id -u)/%s && rm %s", l.unit, l.path)
	case l.unit != "":
		return fmt.Sprintf("systemctl --user disable --now %s.service && rm %s", l.unit, l.path)
	}
	return "rm " + l.path
}

// shims are the 0.4 binaries next to cflk. A file called cf that is not ours
// (Cloud Foundry has one) is left alone.
func (r Retirement) shims() []leftover {
	var out []leftover
	for _, name := range []string{"cf", "cf.prev"} {
		path := filepath.Join(r.Home, ".local", "bin", name)
		if r.isOurs(path) {
			out = append(out, leftover{name: "legacy:bin:" + name, label: "binario " + name, path: path})
		}
	}
	return out
}

// ourShims are the names the 0.4 install gave its binaries, and what a link of ours points to.
var ourShims = []string{"cf", "cf.prev", "cflk"}

// isOurs says whether path is a binary of this repository. A regular file must
// carry its mark. A link must point, by one of our names, into the install
// directory: a `cf` that goes anywhere else (Cloud Foundry's) is not ours, and
// one that goes there must also carry the mark if its target can be read.
func (r Retirement) isOurs(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		target = filepath.Clean(target)
		if filepath.Dir(target) != filepath.Join(r.Home, ".local", "bin") || !slices.Contains(ourShims, filepath.Base(target)) {
			return false
		}
		content, err := os.ReadFile(path)                                   //nolint:gosec // the path is a fixed name under the home
		return err != nil || bytes.Contains(content, []byte(ourBinaryMark)) // a dangling link of ours has nothing to read
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
