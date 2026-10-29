package history

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/chr33s/shistory/internal/db"
)

const testNow int64 = 1793232000000
const day int64 = 86400000

func testStore(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertEvent(t *testing.T, s *db.Store, command string, started int64, scope, cwd, session, host string, status int) int64 {
	t.Helper()
	result, err := s.Exec(`INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell,hostname,exit_status)
 VALUES(?,(SELECT coalesce(max(seq),0)+1 FROM history),?,?,?,'project',?,'zsh',?,?)`, session, started, command, cwd, scope, host, status)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func query(t *testing.T, s *db.Store, q Query) []Event {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 100
	}
	q.Now = testNow
	rows, err := List(s, q)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestFrecencyAndUniqueCommands(t *testing.T) {
	s := testStore(t)
	add := func(cmd string, at int64) int64 {
		return insertEvent(t, s, cmd, at, "/project", "/project", "session", "host", 0)
	}
	add("git status", testNow-day)
	add("git diff", testNow-day/2)
	newestStatus := add("git status", testNow-day)
	add("git status", testNow-30*day) // Out-of-order import contributes half a run.
	recent := add("git log", testNow)
	rows := query(t, s, Query{Scope: "/project", Dedupe: true, Rank: "frecent"})
	if len(rows) != 3 || rows[0].ID != newestStatus || rows[0].Executions != 3 || rows[0].LastUsed != testNow-day {
		t.Fatalf("frecent: %+v", rows)
	}
	want := 2*math.Exp2(-1.0/30) + 0.5
	if math.Abs(rows[0].Score-want) > 1e-12 {
		t.Fatalf("score=%g want %g", rows[0].Score, want)
	}
	rows = query(t, s, Query{Scope: "/project", Dedupe: true, Rank: "recent"})
	if len(rows) != 3 || rows[0].ID != recent {
		t.Fatalf("recent: %+v", rows)
	}
	rows = query(t, s, Query{Scope: "/project", Rank: "frecent"})
	if len(rows) != 5 || rows[0].Command != "git status" || rows[2].Command != "git status" {
		t.Fatalf("all executions: %+v", rows)
	}
	rows = query(t, s, Query{Scope: "/project"})
	if len(rows) != 5 || rows[0].ID != recent {
		t.Fatalf("chronological: %+v", rows)
	}
}

func TestRankingFiltersAndSummaryEquivalence(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 40; i++ {
		insertEvent(t, s, fmt.Sprintf("command %d", i%3), testNow-int64(i)*day, fmt.Sprintf("/project%d", i%2), fmt.Sprintf("/project%d/sub%d", i%2, i%4), "session", fmt.Sprintf("host%d", i%2), i%2)
	}
	for _, q := range []Query{
		{Dedupe: true, Rank: "frecent"},
		{Scope: "/project0", Dedupe: true, Rank: "frecent"},
		{CWD: "/project0/sub0", Dedupe: true, Rank: "recent"},
		{Text: "command 1", Dedupe: true, Rank: "frecent"},
	} {
		summary := query(t, s, q)
		q.Session = "session" // Forces the raw-event fallback, matching the same events.
		raw := query(t, s, q)
		if len(summary) != len(raw) {
			t.Fatalf("summary=%+v raw=%+v", summary, raw)
		}
		for i := range raw {
			a, b := summary[i], raw[i]
			if a.ID != b.ID || a.Executions != b.Executions || a.LastUsed != b.LastUsed || math.Abs(a.Score-b.Score) > 1e-12 {
				t.Fatalf("summary=%+v raw=%+v", a, b)
			}
		}
	}
	for _, q := range []Query{
		{Hostname: "host1", Dedupe: true, Rank: "frecent"},
		{Failed: true, Dedupe: true, Rank: "frecent"},
	} {
		rows := query(t, s, q)
		var total int64
		for _, e := range rows {
			total += e.Executions
			if e.Hostname != "host1" || *e.Status != 1 {
				t.Fatal(e)
			}
		}
		if total != 20 {
			t.Fatalf("filtered count %d", total)
		}
	}
}

func TestSummaryRepairAndFutureTimestamps(t *testing.T) {
	s := testStore(t)
	first := insertEvent(t, s, "repeat", testNow-day, "/p", "/p", "s", "h", 0)
	latest := insertEvent(t, s, "repeat", testNow, "/p", "/p", "s", "h", 0)
	insertEvent(t, s, "other", testNow-day, "/p", "/p", "s", "h", 0)
	if _, err := s.Exec("DELETE FROM history WHERE id=?", latest); err != nil {
		t.Fatal(err)
	}
	rows := query(t, s, Query{Text: "repeat", Dedupe: true, Rank: "frecent"})
	if len(rows) != 1 || rows[0].ID != first || rows[0].Executions != 1 {
		t.Fatal(rows)
	}
	if _, err := s.Exec("UPDATE history SET command='renamed',cwd='/p/sub' WHERE id=?", first); err != nil {
		t.Fatal(err)
	}
	rows = query(t, s, Query{CWD: "/p/sub", Dedupe: true, Rank: "frecent"})
	if len(rows) != 1 || rows[0].Command != "renamed" {
		t.Fatal(rows)
	}
	if got := query(t, s, Query{Text: "repeat", Dedupe: true}); len(got) != 0 {
		t.Fatal(got)
	}
	insertEvent(t, s, "future", testNow+day, "/p", "/p", "s", "h", 0)
	insertEvent(t, s, "future", testNow-day, "/p", "/p", "s", "h", 0)
	rows = query(t, s, Query{Text: "future", Dedupe: true, Rank: "frecent"})
	if len(rows) != 1 || math.Abs(rows[0].Score-(1+math.Exp2(-1.0/30))) > 1e-12 {
		t.Fatal(rows)
	}
	if _, err := s.Exec("DELETE FROM history"); err != nil {
		t.Fatal(err)
	}
	if got := query(t, s, Query{Dedupe: true, Rank: "frecent"}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestFuzzyMatching(t *testing.T) {
	for _, tc := range []struct {
		query, command string
		want           bool
	}{
		{"gts", "git status", true}, {"gts", "git diff", false},
		{"écr", "ÉCHO\nrépertoire", true}, {"Écr", "écho répertoire", false},
		{"GSt", "Git Status", true}, {"GSt", "git status", false},
		{"a%_", "ab%xy_", true}, {"!foo", "foo", false}, {"a b", "ab", false},
		{"'", "echo 'quoted'", true}, {"", "anything", true},
	} {
		if got := fuzzyMatcher(tc.query)(tc.command); got != tc.want {
			t.Errorf("%q / %q = %v", tc.query, tc.command, got)
		}
	}
	s := testStore(t)
	literal := "printf '%_quoted'\nnext\n"
	insertEvent(t, s, literal, testNow-day, "/p", "/p", "s", "h", 0)
	insertEvent(t, s, "unrelated", testNow, "/p", "/p", "s", "h", 0)
	rows := query(t, s, Query{Text: "p%_n", Match: "fuzzy", Rank: "frecent", Dedupe: true, Limit: 1})
	if len(rows) != 1 || rows[0].Command != literal {
		t.Fatal(rows)
	}
	rows = query(t, s, Query{Text: "%_", Dedupe: true})
	if len(rows) != 1 || rows[0].Command != literal {
		t.Fatal(rows)
	}
	prefix := "printf '%_"
	rows = query(t, s, Query{Prefix: &prefix, Rank: "frecent", Dedupe: true})
	if len(rows) != 1 || rows[0].Command != literal {
		t.Fatal(rows)
	}
}

func TestSearchBeforeLimit(t *testing.T) {
	s := testStore(t)
	old := insertEvent(t, s, "kubectl ancient-needle", testNow-90*day, "/p", "/p", "s", "h", 0)
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10001)
 INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell)
 SELECT 'bulk',x,?,'recent command '||x,'/p','project','/p','zsh' FROM n`, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, match := range []string{"literal", "fuzzy"} {
		text := "ancient-needle"
		if match == "fuzzy" {
			text = "kancndl"
		}
		rows := query(t, s, Query{Scope: "/p", Text: text, Match: match, Rank: "frecent", Dedupe: true, Limit: 1})
		if len(rows) != 1 || rows[0].ID != old {
			t.Fatalf("%s: %+v", match, rows)
		}
	}
}

func TestConcurrentSummaryRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000)
 INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell)
 SELECT 'bulk',x,?,'command '||(x%10),'/p','project','/p','zsh' FROM n`, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Exec("DELETE FROM history WHERE seq%2=0"); err != nil {
		t.Fatal(err)
	}
	stores := make([]*db.Store, 8)
	for i := range stores {
		stores[i], err = db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	for _, store := range stores {
		go func() {
			<-start
			rows, err := List(store, Query{Scope: "/p", Dedupe: true, Rank: "frecent", Limit: 100, Now: testNow})
			if err == nil {
				var count int64
				for _, row := range rows {
					count += row.Executions
				}
				if count != 500 {
					err = fmt.Errorf("count=%d", count)
				}
			}
			results <- err
		}()
	}
	close(start)
	for range stores {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
