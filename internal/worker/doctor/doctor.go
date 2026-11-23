// Package doctor is the verdict on this machine: one Component per thing a Job
// needs, from the runtime to the login of each harness. Every dependency is
// injected, so a check is a function of what the machine looks like and the
// package never reads a secret: it only asks whether a login is there.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/worker/sysinfo"
)

// Status of a Component.
const (
	ComponentOK   = "ok"
	ComponentWarn = "warn"
	ComponentFail = "fail"
)

// Component is the verdict on one thing of the machine: Name is "runtime",
// "git", "disk", or "harness:<id>", "auth:<id>". Action is what a person does
// to fix it, never a secret.
type Component struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Action  string `json:"action,omitempty"`
}

// ClaudeKeychainItem is where Claude Code keeps its login on macOS.
const ClaudeKeychainItem = "Claude Code-credentials"

// loginCommands is what a person runs, on the Worker, to log in to a harness.
var loginCommands = map[string]string{claude.ID: "claude login", "codex": "codex login"}

// globalAuth is a harness that keeps its login in a file of the host.
type globalAuth interface {
	GlobalAuth(host harness.Host) string
}

// Options parameterise New.
type Options struct {
	Host     harness.Host
	Adapters []harness.Adapter
	// Worker names the machine in the actions ("claude login en <worker>").
	Worker string
	// DataDir is the data directory of the Worker: its disk is the one measured,
	// and its baselines say what was applied.
	DataDir string
	MinFree Threshold // zero is DefaultMinFree
	// Git is the git binary; empty is the one in the PATH.
	Git string

	// Runtime checks the session runtime; nil checks tmux.
	Runtime func(ctx context.Context) error
	// RuntimeName names the runtime in the verdict; empty is tmux.
	RuntimeName string
	// Disk measures the filesystem of dir; nil is sysinfo.DiskOf.
	Disk func(dir string) (sysinfo.Disk, error)
	// GOOS is the platform, for the checks that depend on it; empty is this one.
	GOOS string
	// RuntimeIsolated says the runtime confines the Runs (a container, a VM).
	// The zero value is the host itself, which is what tmux and a process are.
	RuntimeIsolated bool
}

// Doctor runs the checks. It remembers whether the disk was failing, which is
// all the state of the hysteresis.
type Doctor struct {
	o Options

	mu          sync.Mutex
	diskFailing bool
}

// New builds a Doctor over o.
func New(o Options) *Doctor {
	if o.MinFree == (Threshold{}) {
		o.MinFree, _ = ParseThreshold(DefaultMinFree)
	}
	if o.RuntimeName == "" {
		o.RuntimeName = "tmux"
	}
	if o.Runtime == nil {
		o.Runtime = func(ctx context.Context) error {
			t, err := tmux.New(tmux.Options{Socket: tmux.SocketName(o.DataDir)})
			if err != nil {
				return err
			}
			return t.Check(ctx)
		}
	}
	if o.Disk == nil {
		o.Disk = sysinfo.DiskOf
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	return &Doctor{o: o}
}

// Check runs every check; what it returns is fit for a terminal (Clean).
func (d *Doctor) Check(ctx context.Context) []Component {
	out := []Component{d.runtime(ctx), d.git(ctx), d.disk()}
	for _, a := range d.o.Adapters {
		out = append(out, d.harness(ctx, a)...)
	}
	for i := range out {
		out[i] = CleanComponent(out[i])
	}
	return out
}

// Failing is the number of components that fail.
func Failing(components []Component) int {
	n := 0
	for _, c := range components {
		if c.Status == ComponentFail {
			n++
		}
	}
	return n
}

func ok(name, msg string) Component {
	return Component{Name: name, Status: ComponentOK, Message: msg}
}

func (d *Doctor) runtime(ctx context.Context) Component {
	if err := d.o.Runtime(ctx); err != nil {
		action := "revisa el runtime de sesiones en " + d.o.Worker
		if d.o.RuntimeName == "tmux" {
			action = "instala tmux 3.0 o posterior en " + d.o.Worker
		}
		return Component{
			Name: "runtime", Status: ComponentFail, Message: err.Error(),
			Action: action,
		}
	}
	return ok("runtime", d.o.RuntimeName+" disponible")
}

func (d *Doctor) git(ctx context.Context) Component {
	res, err := d.o.Host.Run(ctx, harness.Cmd{Argv: []string{d.o.Git, "--version"}})
	if err != nil || res.ExitCode != 0 {
		return Component{
			Name: "git", Status: ComponentFail, Message: "git no está disponible",
			Action: "instala git en " + d.o.Worker,
		}
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n")
	return ok("git", version)
}

func (d *Doctor) disk() Component {
	disk, err := d.o.Disk(d.o.DataDir)
	if err != nil {
		return Component{Name: "disk", Status: ComponentWarn, Message: "no se pudo medir el disco: " + err.Error()}
	}
	d.mu.Lock()
	d.diskFailing = d.o.MinFree.Failing(disk.FreeBytes, disk.TotalBytes, d.diskFailing)
	failing := d.diskFailing
	d.mu.Unlock()

	pct := 0.0
	if disk.TotalBytes > 0 {
		pct = 100 * float64(disk.FreeBytes) / float64(disk.TotalBytes)
	}
	msg := fmt.Sprintf("%.0f%% libre (%s); mínimo %s", pct, humanBytes(disk.FreeBytes), d.o.MinFree)
	if failing {
		return Component{
			Name: "disk", Status: ComponentFail, Message: msg,
			Action: "libera espacio en " + d.o.Worker + "",
		}
	}
	return ok("disk", msg)
}

// harness checks the harnesses this machine offers: one that is not installed
// is not offered, and has nothing to report.
func (d *Doctor) harness(ctx context.Context, a harness.Adapter) []Component {
	id := a.ID()
	det, err := a.Detect(ctx, d.o.Host)
	switch {
	case err != nil:
		return []Component{{
			Name: "harness:" + id, Status: ComponentFail, Message: "no se pudo detectar: " + err.Error(),
			Action: "revisa la instalación de " + id + " en " + d.o.Worker,
		}}
	case !det.Installed:
		return nil
	}
	out := []Component{d.config(ctx, a)}
	if c, warn := d.isolation(a); warn {
		out = append(out, c)
	}
	if _, isAuth := a.(globalAuth); isAuth {
		out = append(out, d.auth(ctx, a))
	}
	return out
}

func (d *Doctor) config(ctx context.Context, a harness.Adapter) Component {
	name := "harness:" + a.ID()
	val, err := a.ValidateConfig(ctx, d.o.Host)
	switch {
	case errors.Is(err, harness.ErrUnsupported):
	case err != nil:
		return Component{
			Name: name, Status: ComponentFail, Message: "no se pudo validar la configuración: " + err.Error(),
			Action: "revisa la configuración de " + a.ID() + " en " + d.o.Worker,
		}
	case len(val.Issues) > 0:
		// A harness with ConfigIsolation runs on the configuration materialised
		// for each Run, never on the host's: a problem there is only a warning.
		// Without it, a configuration that does not work takes no Runs.
		status := ComponentWarn
		if val.Blocking() && !a.Capabilities().ConfigIsolation {
			status = ComponentFail
		}
		issue := val.Issues[0]
		msg := issue.Message
		if issue.Component != "" {
			msg = issue.Component + ": " + msg
		}
		return Component{
			Name: name, Status: status, Message: fmt.Sprintf("configuración con %d problema(s): %s", len(val.Issues), msg),
			Action: "revisa la configuración de " + a.ID() + " en " + d.o.Worker,
		}
	}
	return ok(name, "instalado y con configuración válida")
}

// isolation warns of a harness that would run with its permission prompts
// skipped directly on the host: because it launches that way on a runtime that
// does not confine it, or because the host configuration it reads says so.
func (d *Doctor) isolation(a harness.Adapter) (Component, bool) {
	var reasons []string
	if a.Capabilities().PreapprovedPermissions && !d.o.RuntimeIsolated {
		reasons = append(reasons, a.ID()+" se ejecuta con permisos preaprobados directamente en el host (sin aislamiento)")
	}
	if d.hostConfigBypasses(a) {
		reasons = append(reasons, a.ID()+" no aísla su configuración y la del host salta los permisos (bypass)")
	}
	if len(reasons) == 0 {
		return Component{}, false
	}
	return Component{
		Name: "isolation:" + a.ID(), Status: ComponentWarn,
		Message: strings.Join(reasons, "; "),
		Action:  "usa un Worker dedicado o un runtime aislado para " + a.ID() + " en " + d.o.Worker,
	}, true
}

func (d *Doctor) hostConfigBypasses(a harness.Adapter) bool {
	detector, ok := a.(harness.BypassDetector)
	if !ok || a.Capabilities().ConfigIsolation {
		return false
	}
	bypasses, err := detector.BypassesPermissions(d.o.Host)
	return err == nil && bypasses
}

// auth says whether a login is there, never what it is.
func (d *Doctor) auth(ctx context.Context, a harness.Adapter) Component {
	name := "auth:" + a.ID()
	if a.ID() == claude.ID && d.hasKeychainLogin(ctx) {
		return ok(name, "login de Claude Code presente")
	}
	if auth, isAuth := a.(globalAuth); isAuth {
		if _, err := os.Stat(auth.GlobalAuth(d.o.Host)); err == nil {
			return ok(name, "login presente")
		}
	}
	login := loginCommands[a.ID()]
	if login == "" {
		login = "inicia sesión en " + a.ID()
	}
	return Component{
		Name: name, Status: ComponentFail, Message: "sin login de " + a.ID(),
		Action: login + " en " + d.o.Worker,
	}
}

// hasKeychainLogin asks the macOS Keychain whether the item exists. Without -w
// or -g the command prints attributes and never the password; its output is
// dropped anyway.
func (d *Doctor) hasKeychainLogin(ctx context.Context) bool {
	if d.o.GOOS != "darwin" {
		return false
	}
	res, err := d.o.Host.Run(ctx, harness.Cmd{Argv: []string{"security", "find-generic-password", "-s", ClaudeKeychainItem}})
	return err == nil && res.ExitCode == 0
}
