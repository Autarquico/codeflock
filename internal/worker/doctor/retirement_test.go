package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

// fakeManager records the calls to the service manager.
type fakeManager struct{ calls []string }

func (f *fakeManager) run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return nil
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
	m := &fakeManager{}
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
	r, m, home := retirementOn(t, "darwin")
	agent := filepath.Join(home, "Library", "LaunchAgents", "com.codeflock.worker.plist")
	write(t, agent, "")

	r.Fix(context.Background())

	if exists(agent) || !slices.Equal(m.calls, []string{"launchctl bootout gui/501/com.codeflock.worker"}) {
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
