// Package dotenv loads KEY=VALUE pairs from a project-local .env file into
// the process environment. Real environment variables always win: a key
// already set in the shell is never overwritten by the file, so e2e
// scripts and one-off overrides keep working unchanged.
package dotenv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Apply loads path into the process environment using set-default
// semantics (existing variables win). An empty path is a no-op.
func Apply(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("dotenv: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()
		if ln == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // tolerate Notepad's UTF-8 BOM
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r")) // CRLF files
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("dotenv: %s:%d: expected KEY=VALUE, got %q", path, ln, line)
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return fmt.Errorf("dotenv: %s:%d: invalid key %q", path, ln, key)
		}
		if _, set := os.LookupEnv(key); set {
			continue // real environment wins over the file
		}
		os.Setenv(key, cleanValue(val)) //nolint:errcheck // set only fails on absurd keys, already validated
	}
	return sc.Err()
}

// ResolvePath decides which .env applies at startup: the explicit -env
// path (must exist), else ./.env, else one next to the config file.
// It returns "" when no file exists anywhere — .env is optional.
func ResolvePath(explicit, configPath string) (string, error) {
	if explicit != "" {
		if fileExists(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("dotenv: -env file not found: %s", explicit)
	}
	if fileExists(".env") {
		return ".env", nil
	}
	if dir := filepath.Dir(configPath); dir != "" {
		if p := filepath.Join(dir, ".env"); dir != "." && fileExists(p) {
			return p, nil
		}
	}
	return "", nil
}

// cleanValue trims spaces, unwraps a matching pair of quotes (contents kept
// literally, so "a # b" survives), and otherwise drops trailing comments.
func cleanValue(val string) string {
	val = strings.TrimSpace(val)
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		return val[1 : len(val)-1]
	}
	if i := strings.Index(val, " #"); i >= 0 {
		val = val[:i]
	}
	return strings.TrimSpace(val)
}

func validKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
