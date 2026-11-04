package scope

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/chr33s/shistory/internal/config"
)

func TestDetection(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	nested := filepath.Join(root, "repo", "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", filepath.Dir(nested))
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var c config.Config
	c.Root.Git = true
	c.Root.Markers = []string{"go.mod"}
	s, err := Detect(c, nested)
	if err != nil || s.Path != filepath.Dir(nested) || s.Type != "project" {
		t.Fatalf("%+v %v", s, err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	s, err = Detect(c, alias)
	if err != nil || s.CWD != nested {
		t.Fatalf("symlink: %+v %v", s, err)
	}
	c.Root.Git = false
	s, err = Detect(c, nested)
	if err != nil || s.Path != nested || s.Type != "project" {
		t.Fatalf("marker %+v %v", s, err)
	}
	c.Root.Markers = nil
	s, err = Detect(c, nested)
	if err != nil || s.Type != "directory" {
		t.Fatal(s, err)
	}
	// Linked worktrees use a .git file and still form their own namespace.
	git("-C", filepath.Dir(nested), "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "init")
	worktree := filepath.Join(root, "worktree")
	git("-C", filepath.Dir(nested), "worktree", "add", "-q", "-b", "linked", worktree)
	c.Root.Git = true
	t.Setenv("GIT_DIR", filepath.Join(root, "nonexistent"))
	s, err = Detect(c, worktree)
	if err != nil || s.Path != worktree || s.Type != "project" {
		t.Fatalf("worktree %+v %v", s, err)
	}
	if s.Branch != "linked" || Branch(worktree) != "linked" {
		t.Fatalf("worktree branch %+v", s)
	}
	os.Unsetenv("GIT_DIR") // Restored by t.Setenv's cleanup.
	git("-C", filepath.Dir(nested), "symbolic-ref", "HEAD", "refs/heads/feature/x")
	if b := Branch(nested); b != "feature/x" {
		t.Fatalf("nested branch %q", b)
	}
	git("-C", worktree, "checkout", "-q", "--detach")
	if b := Branch(worktree); b != "" {
		t.Fatalf("detached branch %q", b)
	}
	if b := Branch(root); b != "" {
		t.Fatalf("non-repository branch %q", b)
	}
}
