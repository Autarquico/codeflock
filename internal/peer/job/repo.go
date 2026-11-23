package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrAmbiguousRepo is returned for an origin URL that has no single reading.
var ErrAmbiguousRepo = errors.New("job: ambiguous repository URL")

var defaultPorts = map[string]string{"ssh": "22", "git+ssh": "22", "ssh+git": "22", "https": "443", "http": "80", "git": "9418"}

// NormalizeRepo reduces an origin URL to host/owner/repo: the host in
// lowercase with its port only when it is not the scheme's default, no user,
// no ".git" and no trailing "/". ssh://, https://, git:// and the scp form
// git@host:owner/repo give the same result for the same repository.
// Local paths, file:// URLs, queries, fragments, empty or dotted path segments
// and a numeric first segment in the scp form (host:22/owner/repo) are refused.
func NormalizeRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\\?#") {
		return "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	host, path, err := splitRepo(raw)
	if err != nil {
		return "", err
	}
	segs := strings.Split(strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git"), "/")
	if host == "" || len(segs) < 2 {
		return "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
		}
	}
	return host + "/" + strings.Join(segs, "/"), nil
}

// RepoHash is the identity of a repository: the SHA-256 of its normalized form.
func RepoHash(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func splitRepo(raw string) (host, path string, err error) {
	if strings.Contains(raw, "://") {
		return splitURL(raw)
	}
	return splitSCP(raw)
}

func splitURL(raw string) (host, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	def, ok := defaultPorts[strings.ToLower(u.Scheme)]
	if !ok || u.Hostname() == "" {
		return "", "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	host = strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && p != def {
		host += ":" + p
	}
	return host, strings.TrimPrefix(u.Path, "/"), nil
}

func splitSCP(raw string) (host, path string, err error) {
	left, path, ok := strings.Cut(raw, ":")
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	if _, h, found := strings.Cut(left, "@"); found {
		left = h
	}
	path = strings.TrimPrefix(path, "/")
	first, _, _ := strings.Cut(path, "/")
	if len(left) < 2 || strings.ContainsAny(left, "/@") || isDigits(first) {
		return "", "", fmt.Errorf("%w: %q", ErrAmbiguousRepo, raw)
	}
	return strings.ToLower(left), path, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
