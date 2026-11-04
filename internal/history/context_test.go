package history

import (
	"math"
	"testing"

	"github.com/chr33s/shistory/internal/config"
)

var testWeights = config.Weights{Sequence: 4, Directory: 1, Branch: 1, Env: 1, Session: 0.5, Failure: 0.5}

func ptr(s string) *string { return &s }

func TestContextSequence(t *testing.T) {
	s := testStore(t)
	add := func(cmd, session string, at int64) int64 {
		return insertEvent(t, s, cmd, at, "/p", "/p", session, "host", 0)
	}
	// "make test" is the more frecent command, but "git push" follows "git commit".
	for i := int64(0); i < 4; i++ {
		add("make test", "a", testNow-10*day+i)
	}
	add("git commit -m x", "a", testNow-5*day)
	add("git push", "a", testNow-5*day+1)
	add("git commit -m y", "b", testNow-day)
	add("git commit -m x", "live", testNow-1)
	frecent := query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "frecent", Prefix: ptr("")})
	if frecent[0].Command != "make test" {
		t.Fatalf("frecent: %+v", frecent)
	}
	ctx := Context{Session: "live", Window: 100, Weights: testWeights}
	rows := query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: ctx})
	if rows[0].Command != "git push" || rows[0].Context["sequence"] != 5 {
		t.Fatalf("context: %+v", rows)
	}
	// Without a session there is no previous command, so frecency order holds.
	ctx.Session = ""
	rows = query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: ctx})
	if rows[0].Command != "make test" || rows[0].Context != nil {
		t.Fatalf("no session: %+v", rows)
	}
	// Deleting the transition's evidence removes the boost, with no derived state.
	if _, err := s.Exec("DELETE FROM history WHERE command='git push'"); err != nil {
		t.Fatal(err)
	}
	ctx.Session = "live"
	rows = query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: ctx})
	for _, r := range rows {
		if r.Context["sequence"] != 0 {
			t.Fatalf("stale transition: %+v", r)
		}
	}
}

func TestContextImportedSequenceIgnored(t *testing.T) {
	s := testStore(t)
	insertEvent(t, s, "a", testNow-3, "/p", "/p", "import:/h", "host", 0)
	insertEvent(t, s, "b", testNow-2, "/p", "/p", "import:/h", "host", 0)
	insertEvent(t, s, "a", testNow-1, "/p", "/p", "live", "host", 0)
	rows := query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: Context{Session: "live", Weights: testWeights}})
	for _, r := range rows {
		if _, ok := r.Context["sequence"]; ok {
			t.Fatalf("imported transition used: %+v", rows)
		}
	}
}

func TestContextDirectoryBranchEnvFailure(t *testing.T) {
	s := testStore(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Exec(sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Equal frecency: one run each, same timestamp.
	ids := map[string]int64{}
	for _, c := range []struct{ cmd, cwd string }{{"root", "/p"}, {"here", "/p/api"}, {"deeper", "/p/api/v1"}, {"other", "/p/web"}} {
		ids[c.cmd] = insertEvent(t, s, c.cmd, testNow-day, "/p", c.cwd, "s", "host", 0)
	}
	rows := query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: Context{CWD: "/p/api", Weights: testWeights}})
	order := []string{rows[0].Command, rows[1].Command, rows[2].Command, rows[3].Command}
	if order[0] != "here" || rows[0].Context["directory"] != 2 || rows[1].Context["directory"] != 1.5 || rows[2].Context["directory"] != 1.5 || order[3] != "other" {
		t.Fatalf("directory: %v %+v", order, rows)
	}

	exec("UPDATE history SET branch='feature' WHERE id=?", ids["other"])
	exec(`UPDATE history SET env='{"AWS_PROFILE":"dev"}' WHERE id=?`, ids["root"])
	exec("UPDATE history SET exit_status=1 WHERE id=?", ids["here"])
	ctx := Context{CWD: "/elsewhere", Branch: "feature", Env: `{"AWS_PROFILE":"dev"}`, Weights: testWeights}
	rows = query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Context: ctx})
	got := map[string]Event{}
	for _, r := range rows {
		got[r.Command] = r
	}
	base := math.Exp2(-1.0 / 30)
	if got["other"].Context["branch"] != 2 || got["root"].Context["env"] != 2 || got["here"].Context["failure"] != 0.5 {
		t.Fatalf("factors: %+v", rows)
	}
	if math.Abs(got["here"].Score-base*0.5) > 1e-12 || rows[len(rows)-1].Command != "here" {
		t.Fatalf("failure penalty: %+v", rows)
	}
	if string(got["root"].Env) != `{"AWS_PROFILE":"dev"}` || got["other"].Branch != "feature" {
		t.Fatalf("event context fields: %+v", rows)
	}
}

func TestContextSessionAndNavigation(t *testing.T) {
	s := testStore(t)
	insertEvent(t, s, "old favourite", testNow-day, "/p", "/p", "a", "host", 0)
	insertEvent(t, s, "in session", testNow-day, "/p", "/p", "live", "host", 0)
	insertEvent(t, s, "unrelated", testNow-2, "/p", "/p", "b", "host", 0)
	insertEvent(t, s, "unrelated", testNow-1, "/p", "/p", "live", "host", 0)
	rows := query(t, s, Query{Scope: "/p", Dedupe: true, Rank: "context", Text: "o", Context: Context{Session: "live", Weights: testWeights}})
	if rows[0].Command != "in session" || rows[0].Context["session"] != 1.5 {
		t.Fatalf("session: %+v", rows)
	}
	if _, err := List(s, Query{Scope: "/p", Limit: 1, Rank: "context", BeforeID: 1}); err == nil {
		t.Fatal("context ranking accepted a navigation cursor")
	}
}

func TestProximity(t *testing.T) {
	for _, c := range []struct {
		dir, cur string
		want     float64
	}{{"/a", "/a", 1}, {"/a/b", "/a", 0.5}, {"/a", "/a/b", 0.5}, {"/ab", "/a", 0}, {"/a", "", 0}, {"/x", "/", 0.5}} {
		if got := proximity(c.dir, c.cur); got != c.want {
			t.Errorf("proximity(%q,%q)=%g want %g", c.dir, c.cur, got, c.want)
		}
	}
}
