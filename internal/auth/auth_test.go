package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeys(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateAndAuthenticate(t *testing.T) {
	tokA, lineA, err := Generate("agent-a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, lineB, _ := Generate("agent-b")
	if !strings.HasPrefix(tokA, TokenPrefix) {
		t.Fatalf("token %q lacks prefix", tokA)
	}
	k, err := Load(writeKeys(t, "# comment", "", lineA, lineB))
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]string{tokA: "agent-a", tokB: "agent-b"} {
		if got, ok := k.Authenticate(tok); !ok || got != want {
			t.Errorf("Authenticate = %q, %v; want %q", got, ok, want)
		}
	}
	for _, bad := range []string{"", "ac_nope", tokA + "x"} {
		if _, ok := k.Authenticate(bad); ok {
			t.Errorf("token %q accepted", bad)
		}
	}
}

func TestLoadRejectsMalformedFiles(t *testing.T) {
	_, line, _ := Generate("a")
	cases := map[string][]string{
		"no separator":   {"abc"},
		"short digest":   {"a:abcd"},
		"duplicate name": {line, line},
	}
	for name, lines := range cases {
		if _, err := Load(writeKeys(t, lines...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(""); err == nil {
		t.Error("empty path accepted")
	}
}

func TestReloadKeepsOldKeysOnError(t *testing.T) {
	tok, line, _ := Generate("a")
	p := writeKeys(t, line)
	k, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := k.Reload(); err == nil {
		t.Fatal("reload of a bad file succeeded")
	}
	if _, ok := k.Authenticate(tok); !ok {
		t.Fatal("old key lost after failed reload")
	}
}

func TestGenerateRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", "a:b", "a b", "#a"} {
		if _, _, err := Generate(n); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
}
