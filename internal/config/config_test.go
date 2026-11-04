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
	for _, text := range []string{"unknown = true", "[history]\nignore = ['[']", "[root]\nmarkers = ['../x']", "[history]\nmax_results = 0", "[search]\nscope = 'bad'", "[search]\nrank = 'bad'",
		"[context]\nwindow = 0", "[context]\nenv = ['A-B']", "[context.weights]\nfailure = 2", "[context.weights]\nsequence = -1"} {
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
	if err := os.WriteFile(path, []byte("[search]\nrank = 'context'\n[context]\nenv = ['B', 'A']\n[context.weights]\nsequence = 2"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil || c.Context.Weights.Sequence != 2 || c.Context.Weights.Directory != 1 || c.Context.Window != 2000 {
		t.Fatal(c, err)
	}
	env := c.ContextEnv(func(k string) (string, bool) { return map[string]string{"A": "1", "B": ""}[k], k != "C" })
	if env != `{"A":"1","B":""}` {
		t.Fatalf("env %s", env)
	}
	var empty Config
	if empty.ContextEnv(os.LookupEnv) != "" {
		t.Fatal("unconfigured env encoded")
	}
}
