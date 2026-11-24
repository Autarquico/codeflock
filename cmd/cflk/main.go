// Command cflk is the Code Flock CLI.
package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/command"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

func main() { os.Exit(run()) }

// run composes the binary: the CLI with the checks of this machine.
func run() int {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	local := newLocalJobs(exe)
	return cli.ExecuteWith(cli.Options{
		Doctor:     checkThisMachine,
		Jobs:       newFleet(local),
		Wrapper:    runWrapper,
		Notify:     runner.HandleNotify,
		PeerServe:  local.servePeer,
		PeerAttach: local.attachPeer,
	})
}

// checkThisMachine is `cflk doctor`: the checks of the doctor over the harness
// adapters of this binary and the home of the user. A machine without a home
// directory fails when the checks run, not when cflk starts.
func checkThisMachine(ctx context.Context, fix bool) ([]doctor.Component, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	host, err := localhost.New(localhost.Options{Home: home, Env: os.Environ()})
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	d := doctor.New(doctor.Options{
		Host:     host,
		Adapters: []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}},
		Worker:   config.MachineName(hostname, "esta máquina"),
		DataDir:  paths.DataDir,
	})
	retirement := doctor.Retirement{Home: home, GOOS: runtime.GOOS, UID: os.Getuid(), Run: runTool}
	components := d.Check(ctx)
	if fix {
		return append(components, retirement.Fix(ctx)...), nil
	}
	return append(components, retirement.Check()...), nil
}

// runTool runs a tool of the service manager (systemctl, launchctl).
func runTool(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run() //nolint:gosec // the tool is a constant of the doctor, never user input
}
