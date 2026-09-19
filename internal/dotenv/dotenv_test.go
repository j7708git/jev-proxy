package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// unset clears a variable so the file can take effect, and restores the
// environment when the test ends.
func unset(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { os.Setenv(key, v) })
		os.Unsetenv(key)
	}
}

func TestApplyParsesFileAndEnvWins(t *testing.T) {
	// CRLF endings, comments, export prefix, quoting, inline comments,
	// an empty value, and an existing variable that must not be clobbered.
	const (
		wins  = "JEV_DOTENV_WIN"
		bare  = "JEV_DOTENV_BARE"
		dq    = "JEV_DOTENV_DQ"
		sq    = "JEV_DOTENV_SQ"
		empty = "JEV_DOTENV_EMPTY"
	)
	t.Setenv(wins, "from-shell") // set before Apply: must survive
	unset(t, bare)
	unset(t, dq)
	unset(t, sq)
	unset(t, empty)

	p := envFile(t, strings.Join([]string{
		"# jev-proxy secrets",
		"",
		wins + "=from-file",
		"export " + bare + " = unquoted value # trailing comment",
		dq + `="keep # hash and  spaces"`,
		sq + `='backslash \n stays literal'`,
		empty + "=",
	}, "\r\n"))

	if err := Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := os.Getenv(wins); got != "from-shell" {
		t.Fatalf("existing env was overwritten: %q", got)
	}
	if got := os.Getenv(bare); got != "unquoted value" {
		t.Fatalf("export/trim/inline-comment: %q", got)
	}
	if got := os.Getenv(dq); got != "keep # hash and  spaces" {
		t.Fatalf("double-quoted value: %q", got)
	}
	if got := os.Getenv(sq); got != `backslash \n stays literal` {
		t.Fatalf("single-quoted value: %q", got)
	}
	if got, ok := os.LookupEnv(empty); !ok || got != "" {
		t.Fatalf("empty value: %q ok=%v", got, ok)
	}
}

func TestApplyRejectsMalformedLines(t *testing.T) {
	unset(t, "JEV_DOTENV_NOEQ")
	if err := Apply(envFile(t, "NOEQ\n")); err == nil {
		t.Fatal("expected error for line without =")
	}
	if err := Apply(envFile(t, "bad-key-1=x\n")); err == nil {
		t.Fatal("expected error for invalid key characters")
	}
	if err := Apply(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("Apply on an explicit missing file must error")
	}
}

func TestApplyEmptyPathNoop(t *testing.T) {
	if err := Apply(""); err != nil {
		t.Fatalf("Apply(\"\") = %v, want nil", err)
	}
}

func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	p := envFile(t, "JEV_DOTENV_RESOLVED=1\n")

	// Explicit path wins, and a missing explicit path is an error.
	if got, err := ResolvePath(p, "config.yaml"); err != nil || got != p {
		t.Fatalf("explicit = %q, %v", got, err)
	}
	if _, err := ResolvePath(filepath.Join(dir, "nope.env"), "config.yaml"); err == nil {
		t.Fatal("missing explicit -env path must error")
	}

	// CWD/.env is found; nothing exists → "" without error.
	old, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(old) })
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chdir(dir)
	if got, err := ResolvePath("", "c.yaml"); err != nil || got != ".env" {
		t.Fatalf("cwd .env = %q, %v", got, err)
	}
	os.Chdir(t.TempDir())
	if got, err := ResolvePath("", "c.yaml"); err != nil || got != "" {
		t.Fatalf("no .env = %q, %v", got, err)
	}
}
