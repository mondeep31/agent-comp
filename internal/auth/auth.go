// Package auth checks bearer API keys against a file of SHA-256 digests.
//
// The keys file holds one "name:sha256hex" entry per line; blank lines and
// lines starting with # are ignored. Only digests are stored, so the file is
// not a secret on its own, but it should still be root-only.
package auth

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// TokenPrefix marks agent-comp API keys so they are easy to spot in leaks.
const TokenPrefix = "ac_"

type entry struct {
	name   string
	digest [sha256.Size]byte
}

// Keys is a reloadable set of API key digests.
type Keys struct {
	path    string
	mu      sync.RWMutex
	entries []entry
}

// Load reads the keys file. A missing path is an error: running without auth
// is never a silent default.
func Load(path string) (*Keys, error) {
	k := &Keys{path: path}
	if err := k.Reload(); err != nil {
		return nil, err
	}
	return k, nil
}

// Reload re-reads the file, keeping the old keys on error.
func (k *Keys) Reload() error {
	if k.path == "" {
		return errors.New("auth: api_keys_file is not set")
	}
	f, err := os.Open(k.path)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	defer f.Close()
	entries, err := parse(bufio.NewScanner(f))
	if err != nil {
		return fmt.Errorf("auth: %s: %w", k.path, err)
	}
	k.mu.Lock()
	k.entries = entries
	k.mu.Unlock()
	return nil
}

func parse(sc *bufio.Scanner) ([]entry, error) {
	var out []entry
	names := map[string]bool{}
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, hexDigest, ok := strings.Cut(text, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d: want name:sha256hex", line)
		}
		raw, err := hex.DecodeString(hexDigest)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("line %d: digest must be 64 hex characters", line)
		}
		if names[name] {
			return nil, fmt.Errorf("line %d: duplicate name %q", line, name)
		}
		names[name] = true
		e := entry{name: name}
		copy(e.digest[:], raw)
		out = append(out, e)
	}
	return out, sc.Err()
}

// Authenticate returns the key's name. It compares against every entry in
// constant time so timing does not reveal which digest is closest.
func (k *Keys) Authenticate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	k.mu.RLock()
	defer k.mu.RUnlock()
	match := ""
	for _, e := range k.entries {
		if subtle.ConstantTimeCompare(sum[:], e.digest[:]) == 1 {
			match = e.name
		}
	}
	return match, match != ""
}

// Len reports how many keys are loaded.
func (k *Keys) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.entries)
}

// Generate returns a new random token and the keys-file line for it.
func Generate(name string) (token, line string, err error) {
	if name == "" || strings.ContainsAny(name, ": \t\n#") {
		return "", "", errors.New("auth: name must be non-empty without ':', '#' or whitespace")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, name + ":" + hex.EncodeToString(sum[:]), nil
}
