package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	t.Setenv("SHISTORY_CONFIG", path)
	t.Setenv("SHISTORY_DB", "")
	t.Setenv("SHISTORY_IGNORE", "")
	for _, text := range []string{"unknown = true", "[history]\nignore = ['[']", "[root]\nmarkers = ['../x']", "[history]\nmax_results = 0", "[search]\nscope = 'bad'", "[search]\nrank = 'bad'"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	if err := os.WriteFile(path, []byte("[root]\nvcs = false\n[database]\npath = '~/custom.db'"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	c, err := Load()
	if err != nil || c.Root.Git || c.Database.Path != filepath.Join(root, "custom.db") {
		t.Fatal(c, err)
	}
}
