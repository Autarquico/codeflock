package doctor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

// fakeManager records the calls to the service manager.
type fakeManager struct {
	home  string
	calls []string
}

// exitError is a tool that ran and refused, with its exit code.
type exitError int

func (e exitError) Error() string { return "exit " + strconv.Itoa(int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// run answers like a healthy machine: a Worker unit that keeps its processes,
// and an agent that is gone from launchd as soon as it is asked.
func (f *fakeManager) run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case strings.HasPrefix(call, "systemctl --user show"):
		unit := args[len(args)-1]
		return "FragmentPath=" + filepath.Join(f.home, ".config", "systemd", "user", unit) + "\nKillMode=process\nActiveState=active\n", nil
	case strings.HasPrefix(call, "launchctl print"):
		return "", exitError(113)
	}
	return "", nil
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func statuses(cs []doctor.Component) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name+"="+c.Status)
	}
	return out
}

func retirementOn(t *testing.T, goos string) (doctor.Retirement, *fakeManager, string) {
	t.Helper()
	home := t.TempDir()
	m := &fakeManager{home: home}
	return doctor.Retirement{Home: home, GOOS: goos, UID: 501, Run: m.run}, m, home
}

func TestRetirementOfALinuxMachine(t *testing.T) {
	r, m, home := retirementOn(t, "linux")
	units := filepath.Join(home, ".config", "systemd", "user")
	write(t, filepath.Join(units, "codeflock-controller.service"), "")
	write(t, filepath.Join(units, "cf-worker.service"), "")
	write(t, filepath.Join(units, "other.service"), "")
	write(t, filepath.Join(home, ".local", "bin", "cf"), "ELF github.com/Autarquico/codeflock")
	write(t, filepath.Join(home, ".local", "bin", "cf.prev"), "ELF github.com/Autarquico/codefabric")
	write(t, filepath.Join(home, ".local", "share", "codeflock", "controller.db"), "")

	before := r.Check()
	if len(m.calls) != 0 || !exists(filepath.Join(units, "cf-worker.service")) {
		t.Fatalf("Check changed something: %v", m.calls)
	}
	for _, c := range before {
		if c.Status != doctor.ComponentWarn || c.Action == "" {
			t.Errorf("not a warning with a fix: %+v", c)
		}
	}
	if len(before) != 5 {
		t.Fatalf("found %v", statuses(before))
	}

	after := r.Fix(context.Background())

	for _, gone := range []string{
		filepath.Join(units, "codeflock-controller.service"), filepath.Join(units, "cf-worker.service"),
		filepath.Join(home, ".local", "bin", "cf"), filepath.Join(home, ".local", "bin", "cf.prev"),
	} {
		if exists(gone) {
			t.Errorf("%s was not removed", gone)
		}
	}
	for _, kept := range []string{filepath.Join(units, "other.service"), filepath.Join(home, ".local", "share", "codeflock", "controller.db")} {
		if !exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	for _, want := range []string{"systemctl --user disable --now codeflock-controller.service", "systemctl --user disable --now cf-worker.service", "systemctl --user daemon-reload"} {
		if !slices.Contains(m.calls, want) {
			t.Errorf("missing call %q in %v", want, m.calls)
		}
	}
	if doctor.Failing(after) != 0 || len(after) != 5 {
		t.Errorf("fix: %v", statuses(after))
	}
}

func TestRetirementOfAMacMachineBootsOutTheAgents(t *testing.T) {
	r, m, agent := macAgent(t)

	r.Fix(context.Background())

	want := []string{
		"launchctl print gui/501/com.codeflock.worker",
		"plutil -extract AbandonProcessGroup raw -o - " + agent,
		"launchctl bootout gui/501/com.codeflock.worker", "launchctl print gui/501/com.codeflock.worker",
		"launchctl print user/501/com.codeflock.worker",
	}
	if exists(agent) || !slices.Equal(m.calls, want) {
		t.Errorf("agent exists=%v calls=%v", exists(agent), m.calls)
	}
}

func TestRetirementIsIdempotent(t *testing.T) {
	r, m, home := retirementOn(t, "linux")
	write(t, filepath.Join(home, ".config", "systemd", "user", "cf-worker.service"), "")
	r.Fix(context.Background())
	m.calls = nil

	for _, got := range [][]doctor.Component{r.Fix(context.Background()), r.Check()} {
		if len(got) != 1 || got[0].Status != doctor.ComponentOK {
			t.Errorf("second pass: %v", statuses(got))
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("a clean machine ran %v", m.calls)
	}
}

func TestRetirementLeavesACfThatIsNotOurs(t *testing.T) {
	r, _, home := retirementOn(t, "linux")
	cf := filepath.Join(home, ".local", "bin", "cf")
	write(t, cf, "Cloud Foundry")

	r.Fix(context.Background())

	if !exists(cf) {
		t.Error("removed a cf that is not ours")
	}
}

func TestRetirementReportsWhatItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove it")
	}
	r, _, home := retirementOn(t, "linux")
	units := filepath.Join(home, ".config", "systemd", "user")
	write(t, filepath.Join(units, "cf-worker.service"), "")
	if err := os.Chmod(units, 0o500); err != nil { //nolint:gosec // a directory that cannot be written
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(units, 0o700) }) //nolint:gosec // a directory

	got := r.Fix(context.Background())

	if doctor.Failing(got) != 1 {
		t.Errorf("fix: %v", statuses(got))
	}
}

func TestRetirementLeavesALinkToAnotherCf(t *testing.T) {
	r, _, home := retirementOn(t, "linux")
	foreign := filepath.Join(t.TempDir(), "cf")
	write(t, foreign, "Cloud Foundry")
	inHome := filepath.Join(home, ".local", "bin", "cf")
	if err := os.MkdirAll(filepath.Dir(inHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, inHome); err != nil {
		t.Fatal(err)
	}
	// the same name in our directory, but not a binary of ours
	write(t, filepath.Join(home, ".local", "bin", "cf.prev"), "Cloud Foundry")
	prev := filepath.Join(home, ".local", "bin", "cf.prev")

	r.Fix(context.Background())

	if !exists(inHome) || !exists(prev) {
		t.Error("removed a cf that is not ours")
	}
}

func TestRetirementRemovesALinkToOurBinary(t *testing.T) {
	r, _, home := retirementOn(t, "linux")
	bin := filepath.Join(home, ".local", "bin")
	write(t, filepath.Join(bin, "cflk"), "ELF github.com/Autarquico/codeflock")
	if err := os.Symlink(filepath.Join(bin, "cflk"), filepath.Join(bin, "cf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bin, "gone"), filepath.Join(bin, "cf.prev")); err != nil {
		t.Fatal(err)
	}

	r.Fix(context.Background())

	if exists(filepath.Join(bin, "cf")) || !exists(filepath.Join(bin, "cflk")) {
		t.Error("the link of ours stayed, or cflk went")
	}
	if !exists(filepath.Join(bin, "cf.prev")) {
		t.Error("a dangling link to a name that is not ours was removed")
	}
}

func TestRetirementKeepsAUnitItCouldNotStop(t *testing.T) {
	r, _, home := retirementOn(t, "linux")
	unit := filepath.Join(home, ".config", "systemd", "user", "cf-worker.service")
	write(t, unit, "")
	r.Run = func(_ context.Context, _ string, args ...string) (string, error) {
		if slices.Contains(args, "show") {
			return "FragmentPath=" + unit + "\nKillMode=process\nActiveState=active\n", nil
		}
		return "", errors.New("no user bus")
	}

	got := r.Fix(context.Background())

	if !exists(unit) || doctor.Failing(got) != 1 || !strings.Contains(got[0].Action, "systemctl --user disable --now cf-worker.service") {
		t.Errorf("unit exists=%v: %+v", exists(unit), got)
	}
}

func TestRetirementRefusesToStopAWorkerThatWouldTakeItsRunsWithIt(t *testing.T) {
	r, m, home := retirementOn(t, "linux")
	unit := filepath.Join(home, ".config", "systemd", "user", "cf-worker.service")
	write(t, unit, "")
	run := r.Run
	r.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if slices.Contains(args, "show") {
			return "FragmentPath=" + unit + "\nKillMode=control-group\nActiveState=active\n", nil
		}
		return run(ctx, name, args...)
	}

	got := r.Fix(context.Background())

	if !exists(unit) || doctor.Failing(got) != 1 || !strings.Contains(got[0].Message, "KillMode") || slices.Contains(m.calls, "systemctl --user disable --now cf-worker.service") {
		t.Errorf("unit exists=%v calls=%v: %+v", exists(unit), m.calls, got)
	}
}

func TestRetirementRefusesAUnitSystemdLoadsFromAnotherFile(t *testing.T) {
	for _, name := range []string{"cf-worker", "codeflock-controller"} {
		r, m, home := retirementOn(t, "linux")
		unit := filepath.Join(home, ".config", "systemd", "user", name+".service")
		write(t, unit, "")
		r.Run = func(_ context.Context, _ string, args ...string) (string, error) {
			m.calls = append(m.calls, strings.Join(args, " "))
			return "FragmentPath=/etc/systemd/user/" + name + ".service\nKillMode=process\nActiveState=active\n", nil
		}

		got := r.Fix(context.Background())

		if !exists(unit) || doctor.Failing(got) != 1 || slices.Contains(m.calls, "--user disable --now "+name+".service") {
			t.Errorf("%s: unit exists=%v calls=%v: %+v", name, exists(unit), m.calls, got)
		}
	}
}

func TestRetirementIgnoresAUnitThatOnlyStartsLikeOurs(t *testing.T) {
	r, _, home := retirementOn(t, "linux")
	write(t, filepath.Join(home, ".config", "systemd", "user", "cf-worker-backup.service"), "")

	got := r.Check()

	if len(got) != 1 || got[0].Status != doctor.ComponentOK {
		t.Errorf("%v", statuses(got))
	}
}

// launchd is a Mac with an agent loaded in one domain from plist, until it is
// booted out. abandons is what plutil reads from the plist; plutilErr, that it cannot.
type launchd struct {
	*fakeManager
	domain, plist, abandons string
	plutilErr               error
	loaded                  bool
}

func (l *launchd) run(ctx context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	switch {
	case name == "plutil":
		l.calls = append(l.calls, call)
		return l.abandons + "\n", l.plutilErr
	case strings.HasPrefix(call, "launchctl bootout "+l.domain):
		l.loaded = false
	case strings.HasPrefix(call, "launchctl print "+l.domain) && l.loaded:
		l.calls = append(l.calls, call)
		return "com.codeflock.worker = {\n\tpath = " + l.plist + "\n}\n", nil
	}
	return l.fakeManager.run(ctx, name, args...)
}

func macAgent(t *testing.T) (doctor.Retirement, *launchd, string) {
	t.Helper()
	r, m, home := retirementOn(t, "darwin")
	agent := filepath.Join(home, "Library", "LaunchAgents", "com.codeflock.worker.plist")
	write(t, agent, "")
	l := &launchd{fakeManager: m, domain: "gui/501", plist: agent, abandons: "true", loaded: true}
	r.Run = l.run
	return r, l, agent
}

func TestRetirementBootsOutAnAgentLoadedInTheUserDomain(t *testing.T) {
	r, l, agent := macAgent(t)
	l.domain = "user/501"

	got := r.Fix(context.Background())

	if exists(agent) || doctor.Failing(got) != 0 || !slices.Contains(l.calls, "launchctl bootout user/501/com.codeflock.worker") {
		t.Errorf("agent exists=%v calls=%v: %+v", exists(agent), l.calls, got)
	}
}

func TestRetirementKeepsAnAgentWhoseProbeFailsForAnyOtherReason(t *testing.T) {
	r, _, agent := macAgent(t)
	r.Run = func(context.Context, string, ...string) (string, error) { return "", context.Canceled }

	got := r.Fix(context.Background())

	if !exists(agent) || doctor.Failing(got) != 1 {
		t.Errorf("agent exists=%v: %+v", exists(agent), got)
	}
}

func TestRetirementKeepsAnAgentThatIsStillLoaded(t *testing.T) {
	r, l, agent := macAgent(t)
	run := r.Run
	r.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := run(ctx, name, args...)
		l.loaded = true // bootout does nothing
		return out, err
	}

	got := r.Fix(context.Background())

	if !exists(agent) || doctor.Failing(got) != 1 {
		t.Errorf("agent exists=%v: %+v", exists(agent), got)
	}
}

func TestRetirementRefusesAWorkerAgentThatDoesNotAbandonItsProcessGroup(t *testing.T) {
	for name, set := range map[string]func(*launchd){
		"false":      func(l *launchd) { l.abandons = "false" },
		"unreadable": func(l *launchd) { l.plutilErr = errors.New("no such key"); l.abandons = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r, l, agent := macAgent(t)
			set(l)

			got := r.Fix(context.Background())

			if !exists(agent) || doctor.Failing(got) != 1 || !l.loaded || slices.ContainsFunc(l.calls, func(c string) bool { return strings.Contains(c, "bootout") }) {
				t.Errorf("agent exists=%v calls=%v: %+v", exists(agent), l.calls, got)
			}
		})
	}
}

func TestRetirementRefusesAnAgentLoadedFromAnotherPlist(t *testing.T) {
	r, l, agent := macAgent(t)
	l.plist = "/Users/other/Library/LaunchAgents/com.codeflock.worker.plist"

	got := r.Fix(context.Background())

	if !exists(agent) || doctor.Failing(got) != 1 || !l.loaded || slices.ContainsFunc(l.calls, func(c string) bool { return strings.Contains(c, "bootout") }) {
		t.Errorf("agent exists=%v calls=%v: %+v", exists(agent), l.calls, got)
	}
}

func TestRetirementRemovesTheFileOfAnAgentThatWasNotLoaded(t *testing.T) {
	r, l, agent := macAgent(t)
	l.loaded = false
	l.abandons = "false" // never read: nothing is stopped

	got := r.Fix(context.Background())

	if exists(agent) || doctor.Failing(got) != 0 {
		t.Errorf("agent exists=%v calls=%v: %+v", exists(agent), l.calls, got)
	}
}
