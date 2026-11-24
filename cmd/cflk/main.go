// Command cflk is the Code Flock CLI.
package main

import (
	"context"
	"os"

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
func checkThisMachine(ctx context.Context) ([]doctor.Component, error) {
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
	return d.Check(ctx), nil
}
