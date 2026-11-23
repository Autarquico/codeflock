package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T) Runner {
	t.Helper()
	raw, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "tailscale" || !reflect.DeepEqual(args, []string{"status", "--json"}) {
			t.Errorf("ran %s %v", name, args)
		}
		return raw, nil
	}
}

func names(ps []Peer) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestOnlyTaggedNodesAreDiscoveredAndTheirIdentityIsTheNodeID(t *testing.T) {
	res, err := Discover(t.Context(), Options{Run: fixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Peers); !reflect.DeepEqual(got, []string{"mac-mini", "omarch01"}) {
		t.Fatalf("peers = %v (untagged, self and a name that is an option must not appear)", got)
	}
	mac := res.Peers[0]
	if mac.ID != "nMAC00000CNTRL" || mac.Host != "mac-mini.tail1234.ts.net" || !mac.Online || mac.Source != Tailscale {
		t.Errorf("mac-mini = %+v", mac)
	}
	if res.Peers[1].Online {
		t.Errorf("omarch01 is offline in the fixture: %+v", res.Peers[1])
	}
	if res.Self.ID != "nSELF0000CNTRL" || res.Self.Name != "laptop" {
		t.Errorf("self = %+v", res.Self)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no usable name") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestPeersOfTheConfigFileJoinAndDuplicatesAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := `peers = ["builder.example.com", "mac-mini", "laptop", "-oProxyCommand=x", "a b"]`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Discover(t.Context(), Options{Run: fixture(t), ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Peers); !reflect.DeepEqual(got, []string{"mac-mini", "omarch01", "builder"}) {
		t.Fatalf("peers = %v", got)
	}
	b := res.Peers[2]
	if b.ID != "config:builder.example.com" || b.Host != "builder.example.com" || b.Source != Config {
		t.Errorf("builder = %+v", b)
	}
	if invalid := strings.Count(strings.Join(res.Warnings, "\n"), "not a valid host name"); invalid != 2 {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestWithoutTailscaleTheConfigFileStillWorks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`peers = ["mac-mini"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	down := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("executable file not found")
	}
	res, err := Discover(t.Context(), Options{Run: down, ConfigPath: path})
	if err != nil || len(res.Peers) != 1 || len(res.Warnings) != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestAMissingConfigFileIsNotAnErrorButABrokenOneIs(t *testing.T) {
	if _, err := ReadConfig(filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Errorf("missing file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("peers = [unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Error("a broken file was read")
	}
}

func TestFindNamesAPeerOnlyAmongTheDiscovered(t *testing.T) {
	res, _ := Discover(t.Context(), Options{Run: fixture(t)})
	if p, ok := res.Find("mac-mini"); !ok || p.Host != "mac-mini.tail1234.ts.net" {
		t.Errorf("find mac-mini = %+v, %v", p, ok)
	}
	if _, ok := res.Find("evil.example.com; rm -rf /"); ok {
		t.Error("found a host nobody discovered")
	}
}
