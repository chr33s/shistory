package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestRejectFutureSchemaAndSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("accepted future schema")
	}
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(alias); err == nil {
		s.Close()
		t.Fatal("accepted a database symlink")
	}
}

func TestConcurrentFirstRunAndWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shistory", "history.db")
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		ready.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			s, err := Open(path)
			if err != nil {
				errors <- err
				return
			}
			defer s.Close()
			for n := 1; n <= 10; n++ {
				_, err = s.Exec(`INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell) VALUES(?,?,1,'test','/tmp','directory','/tmp','zsh')`, fmt.Sprint(i), n)
				if err != nil {
					errors <- err
					return
				}
			}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.QueryRow("SELECT count(*) FROM history").Scan(&n); err != nil || n != 120 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestWALTransitionContention(t *testing.T) {
	for _, release := range []bool{true, false} {
		t.Run(fmt.Sprintf("release=%t", release), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			holder, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if _, err := holder.Exec("CREATE TABLE blocker (value INTEGER); INSERT INTO blocker VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			conn, err := holder.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
				t.Fatal(err)
			}
			defer conn.ExecContext(context.Background(), "ROLLBACK")
			var value int
			if err := conn.QueryRowContext(context.Background(), "SELECT value FROM blocker").Scan(&value); err != nil {
				t.Fatal(err)
			}
			// A zero busy timeout ensures success requires application-level retry.
			target, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			target.SetMaxOpenConns(1)
			var mode string
			err = target.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
			var sqliteErr *sqlite.Error
			if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_BUSY {
				t.Fatalf("expected WAL contention, got %v", err)
			}
			if release {
				released := make(chan error, 1)
				go func() {
					time.Sleep(50 * time.Millisecond)
					_, err := conn.ExecContext(context.Background(), "ROLLBACK")
					released <- err
				}()
				err := (&Store{target}).initialize()
				if unlockErr := <-released; unlockErr != nil {
					t.Fatal(unlockErr)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := target.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
					t.Fatalf("mode=%q err=%v", mode, err)
				}
				var version int
				if err := target.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
					t.Fatalf("version=%d err=%v", version, err)
				}
			} else {
				started := time.Now()
				err := (&Store{target}).enableWAL()
				if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_BUSY) {
					t.Fatalf("expected bounded contention error, got %v", err)
				}
				if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
					t.Fatalf("retry did not respect its one-second budget: %s", elapsed)
				}
			}
		})
	}
}

func TestUpgradeVersionOnePreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(migration); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(`INSERT INTO history(session_id,seq,started_at,command,cwd,scope_type,scope_path,shell)
 VALUES('old',1,100,'echo preserved','/p','project','/p','zsh'),('old',2,200,'echo preserved','/p','project','/p','zsh')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count, dirty int
	if err = s.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = s.QueryRow("SELECT count(*) FROM history WHERE command='echo preserved'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err = s.QueryRow("SELECT count(*) FROM dirty_commands").Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || count != 2 || dirty != 1 {
		t.Fatalf("version=%d events=%d dirty=%d", version, count, dirty)
	}
}
