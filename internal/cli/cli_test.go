package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chr33s/shistory/internal/history"
)

func setup(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SHISTORY_CONFIG", "")
	t.Setenv("SHISTORY_DB", filepath.Join(root, "shistory", "history.db"))
	t.Setenv("SHISTORY_IGNORE", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	return root
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if err := Run(args, &out, &stderr); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, stderr.String())
	}
	return out.String()
}

func events(t *testing.T, args ...string) []history.Event {
	t.Helper()
	args = append(args, "--format", "json")
	var result []history.Event
	if err := json.Unmarshal([]byte(run(t, args...)), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func record(t *testing.T, cwd, cmd string, seq, time int) {
	t.Helper()
	run(t, "record", "--session", "test", "--seq", fmt.Sprint(seq), "--shell", "zsh", "--cwd", cwd, "--command", cmd, "--started-at", fmt.Sprint(time), "--status", "0", "--duration-ms", "15")
}

func TestScopedHistory(t *testing.T) {
	root := setup(t)
	project, other := filepath.Join(root, "project"), filepath.Join(root, "other")
	for _, dir := range []string{filepath.Join(project, "api"), filepath.Join(project, "web"), other} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	api, web := filepath.Join(project, "api"), filepath.Join(project, "web")
	command := "printf '%s\\n' 'it\"s fine'\nsecond line\n"
	record(t, api, command, 1, 1000)
	record(t, web, "npm test", 2, 1001)
	record(t, other, "private scope", 3, 1002)
	record(t, api, "ignored retry", 1, 9999)
	rows := events(t, "list", "--cwd", api)
	if len(rows) != 2 || rows[0].Command != "npm test" || rows[1].Command != command || rows[1].ScopeType != "project" {
		t.Fatalf("rows: %+v", rows)
	}
	if len(events(t, "list", "--cwd", other)) != 1 {
		t.Fatal("directory fallback")
	}
	if len(events(t, "list", "--cwd", web, "--directory")) != 1 {
		t.Fatal("exact cwd")
	}
	if len(events(t, "list", "--global")) != 3 {
		t.Fatal("global")
	}
	if len(events(t, "search", "fine", "--cwd", web)) != 1 {
		t.Fatal("search")
	}
	if got := run(t, "suggest", "--scope", rows[1].ScopePath, "--prefix", "printf"); got != command+"\n" {
		t.Fatalf("suggest changed text: %q", got)
	}
	if len(events(t, "search", "--cwd", web, "%")) != 1 {
		t.Fatal("literal percent")
	}
	if got := run(t, "list", "--cwd", api, "--format", "nul"); !strings.HasSuffix(got, command+"\x00") {
		t.Fatalf("nul: %q", got)
	}
	if got := run(t, "doctor"); !strings.Contains(got, "integrity: ok") {
		t.Fatal(got)
	}
	info, err := os.Stat(os.Getenv("SHISTORY_DB"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("database permissions")
	}
	info, err = os.Stat(filepath.Dir(os.Getenv("SHISTORY_DB")))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("directory permissions")
	}
}

func TestLiteralPrefixDedupeAndStableNavigation(t *testing.T) {
	root := setup(t)
	for i, cmd := range []string{"git older", "git middle", "git newer", "git newer", "git %_literal"} {
		record(t, root, cmd, i+1, 1000+i)
	}
	if got := run(t, "suggest", "--cwd", root, "--prefix", "git %_"); got != "git %_literal\n" {
		t.Fatal(got)
	}
	if got := run(t, "suggest", "--cwd", root, "--prefix", "Git"); got != "" {
		t.Fatal("prefix should be case-sensitive")
	}
	if len(events(t, "list", "--cwd", root)) != 5 || len(events(t, "search", "--cwd", root, "git")) != 4 {
		t.Fatal("display-only dedupe")
	}
	rows := events(t, "navigate", "--cwd", root)
	id := rows[0].ID
	// A concurrent new insert must not shift an older cursor.
	record(t, root, "new insert", 6, 5000)
	older := events(t, "navigate", "--cwd", root, "--before-id", fmt.Sprint(id))
	if len(older) != 1 || older[0].Command != "git newer" {
		t.Fatal(older)
	}
	newer := events(t, "navigate", "--cwd", root, "--after-id", fmt.Sprint(older[0].ID), "--direction", "newer")
	if len(newer) != 1 || newer[0].ID != id {
		t.Fatal(newer)
	}
	// Timestamp order, rather than insertion order, defines older/newer.
	record(t, root, "imported old", 7, 1)
	rows = events(t, "navigate", "--cwd", root, "--before-id", fmt.Sprint(older[0].ID))
	if rows[0].Command != "git newer" {
		t.Fatal(rows)
	}
}

func TestFiltersAndMaintenance(t *testing.T) {
	root := setup(t)
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte("[history]\nignore = ['^ls$']\nstore_failed = false\n[root]\ngit = false\nmarkers = ['go.mod']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHISTORY_CONFIG", path)
	t.Setenv("SHISTORY_IGNORE", "^pwd$")
	for i, cmd := range []string{" secret", "ls", "pwd", "", "keep"} {
		record(t, root, cmd, i+1, 1000+i)
	}
	run(t, "record", "--session", "failed", "--seq", "1", "--shell", "bash", "--cwd", root, "--command", "fail", "--status", "1")
	rows := events(t, "list", "--cwd", root)
	if len(rows) != 1 || rows[0].Command != "keep" {
		t.Fatal(rows)
	}
	if got := run(t, "delete", "--id", fmt.Sprint(rows[0].ID)); got != "1\n" {
		t.Fatal(got)
	}
	record(t, root, "old", 7, 1)
	record(t, root, "new", 8, 2000)
	if got := run(t, "prune", "--before", "1000"); got != "1\n" {
		t.Fatal(got)
	}
	if got := run(t, "delete", "--scope", root); got != "1\n" {
		t.Fatal(got)
	}
	run(t, "vacuum")
}

func TestInvalidInput(t *testing.T) {
	root := setup(t)
	for _, args := range [][]string{
		{"record"}, {"delete"}, {"prune"}, {"scope", "unexpected"},
		{"list", "--limit", "0"}, {"list", "--global", "--directory"},
		{"navigate", "--direction", "bad"}, {"search", "--format", "bad"},
		{"record", "--session", "x", "--seq", "1", "--shell", "bash", "--cwd", root, "--command", "x", "--status", "256"},
	} {
		var b bytes.Buffer
		if err := Run(args, &b, &b); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSearchRankingConfiguration(t *testing.T) {
	root := setup(t)
	for i, command := range []string{"git status", "git diff", "git status", "git log"} {
		record(t, root, command, i+1, 1000+i)
	}
	rows := events(t, "search", "--cwd", root, "git")
	if len(rows) != 3 || rows[0].Command != "git status" || rows[0].Executions != 2 || rows[0].LastUsed != 1002 || rows[0].Score <= 0 {
		t.Fatal(rows)
	}
	if got := run(t, "suggest", "--cwd", root, "--prefix", "git"); got != "git status\n" {
		t.Fatal(got)
	}
	if got := run(t, "search", "--cwd", root, "--match", "fuzzy", "--format", "raw", "gts"); got != "git status\n" {
		t.Fatal(got)
	}
	if got := run(t, "search", "--cwd", root, "--format", "fzf", "--limit", "1", "g"); got != "3\tgit status\x00" {
		t.Fatalf("fzf: %q", got)
	}
	if got := run(t, "search", "--cwd", root, "--limit", "1"); !strings.Contains(got, "2x") {
		t.Fatal(got)
	}
	path := filepath.Join(root, "rank.toml")
	if err := os.WriteFile(path, []byte("[search]\nrank='recent'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHISTORY_CONFIG", path)
	rows = events(t, "search", "--cwd", root)
	if rows[0].Command != "git log" {
		t.Fatal(rows)
	}
	if got := run(t, "suggest", "--cwd", root, "--prefix", "git"); got != "git log\n" {
		t.Fatal(got)
	}
	rows = events(t, "search", "--cwd", root, "--rank", "frecent")
	if rows[0].Command != "git status" {
		t.Fatal(rows)
	}
	rows = events(t, "search", "--cwd", root, "--dedupe=false")
	if len(rows) != 4 || rows[0].Command != "git log" {
		t.Fatal(rows)
	}
	// Maintenance must update ranking and statistics as well as the event log.
	run(t, "delete", "--id", "3")
	rows = events(t, "search", "--cwd", root, "status")
	if len(rows) != 1 || rows[0].Executions != 1 || rows[0].ID != 1 {
		t.Fatal(rows)
	}
	run(t, "prune", "--before", "1002")
	rows = events(t, "search", "--cwd", root)
	if len(rows) != 1 || rows[0].Command != "git log" {
		t.Fatal(rows)
	}
	for _, args := range [][]string{{"search", "--rank", "bad"}, {"suggest", "--rank", "bad"}, {"search", "--match", "bad"}, {"navigate", "--rank", "frecent"}} {
		var out bytes.Buffer
		if err := Run(args, &out, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
