//go:build linux || darwin

package execwrap_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autarquico/codeflock/internal/support/redact"
)

// The tests give the wrapper secrets through a file of the run directory: the
// wrapper only sees a resolver and a retention, whoever composes them.

var errCorruptDelivery = errors.New("the delivery is corrupt")

func deliveryFile(dir string) string { return filepath.Join(dir, "secrets", "delivery.json") }

func deliver(dir string, needles []redact.Needle) error {
	values := map[string][]byte{}
	for _, n := range needles {
		values[n.Ref] = n.Value
	}
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(deliveryFile(dir)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(deliveryFile(dir), b, 0o600)
}

func loadDelivery(dir string) ([]redact.Needle, error) {
	b, err := os.ReadFile(deliveryFile(dir)) //nolint:gosec // a test directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("no secrets delivered")
	}
	if err != nil {
		return nil, err
	}
	var values map[string][]byte
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, errCorruptDelivery
	}
	var out []redact.Needle
	for ref, v := range values {
		out = append(out, redact.Needle{Ref: ref, Value: v})
	}
	return out, nil
}

func resolverOf(dir string) func(string) (string, error) {
	return func(ref string) (string, error) {
		needles, err := loadDelivery(dir)
		if err != nil {
			return "", err
		}
		for _, n := range needles {
			if n.Ref == ref {
				return string(n.Value), nil
			}
		}
		return "", fmt.Errorf("the secret %q is not available to the consumer", ref)
	}
}

// revoke takes ref away from the resolver, as a revocation does; the retention keeps it.
func revoke(dir, ref string) error {
	needles, err := loadDelivery(dir)
	if err != nil {
		return err
	}
	kept := needles[:0]
	for _, n := range needles {
		if n.Ref != ref {
			kept = append(kept, n)
		}
	}
	return deliver(dir, kept)
}
