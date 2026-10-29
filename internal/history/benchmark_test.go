package history

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/chr33s/shistory/internal/db"
)

// Keep the dataset fixed so before/after measurements are comparable: 20
// projects, 1,000 unique commands per project, and repeated runs over 90 days.
func benchmarkStore(b *testing.B, size int) *db.Store {
	b.Helper()
	s, err := db.Open(filepath.Join(b.TempDir(), "history.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	tx, err := s.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell) VALUES('bench',?,?,?,?,'project',?,'zsh')`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < size; i++ {
		project := fmt.Sprintf("/project/%02d", i%20)
		command := fmt.Sprintf("kubectl --context cluster-%03d get deployment-%03d", (i/20)%1000, (i/20)%1000)
		if _, err := stmt.Exec(i+1, int64(1793232000000)-int64(size-i)*7776000000/int64(size), command, project, project); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkQueries(b *testing.B) {
	binary := filepath.Join(b.TempDir(), "shistory")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/shistory").CombinedOutput(); err != nil {
		b.Fatalf("build: %v %s", err, out)
	}
	for _, size := range []int{100000, 1000000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s := benchmarkStore(b, size)
			prefix := "kubectl --context cluster-999"
			cases := []struct {
				name string
				q    Query
			}{
				{"scoped_search", Query{Scope: "/project/00", Text: "deployment", Limit: 100, Dedupe: true, Rank: "frecent"}},
				{"scoped_fuzzy", Query{Scope: "/project/00", Text: "kctlgtdep9", Match: "fuzzy", Limit: 100, Dedupe: true, Rank: "frecent"}},
				{"global_search", Query{Text: "deployment", Limit: 100, Dedupe: true, Rank: "frecent"}},
				{"scoped_rare", Query{Scope: "/project/00", Text: "deployment-999", Limit: 100, Dedupe: true, Rank: "frecent"}},
				{"scoped_suggest", Query{Scope: "/project/00", Prefix: &prefix, Limit: 1, Dedupe: true, Rank: "frecent"}},
				{"global_suggest", Query{Prefix: &prefix, Limit: 1, Dedupe: true, Rank: "frecent"}},
			}
			for _, tc := range cases {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := List(s, tc.q); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			b.Run("process_suggest", func(b *testing.B) {
				var path string
				var seq int
				var name string
				if err := s.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
					b.Fatal(err)
				}
				home := b.TempDir()
				invoke := func() {
					cmd := exec.Command(binary, "suggest", "--scope", "/project/00", "--prefix", prefix)
					cmd.Env = append(os.Environ(), "HOME="+home, "SHISTORY_DB="+path, "SHISTORY_CONFIG=", "SHISTORY_IGNORE=", "XDG_CONFIG_HOME="+home)
					if out, err := cmd.CombinedOutput(); err != nil {
						b.Fatalf("suggest: %v %s", err, out)
					}
				}
				// Exclude the OS's first-execution validation of the new binary.
				// Every measured iteration still starts a fresh CLI process.
				invoke()
				b.ReportAllocs()
				for b.Loop() {
					invoke()
				}
			})
		})
	}
}
