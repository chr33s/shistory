package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/chr33s/shistory/internal/config"
	"github.com/chr33s/shistory/internal/db"
	"github.com/chr33s/shistory/internal/history"
	"github.com/chr33s/shistory/internal/scope"
)

func maintenanceFlags(fs *flag.FlagSet, c config.Config, name string, out io.Writer) func(*db.Store) error {
	id := fs.Int64("id", 0, "history row ID")
	path := fs.String("scope", "", "scope to delete/prune")
	before := fs.Int64("before", 0, "exclusive cutoff in Unix milliseconds")
	days := fs.Int("days", 0, "retention days")
	return func(s *db.Store) error {
		where, args := "", []any{}
		if name == "delete" {
			if (*id <= 0 && *path == "") || (*id != 0 && *path != "") || *id < 0 || *before != 0 || *days != 0 {
				return fmt.Errorf("delete requires --id ID or --scope DIR")
			}
			if *id > 0 {
				where, args = "id = ?", []any{*id}
			}
		} else {
			if *id != 0 || *before < 0 || *days < 0 || (*before == 0 && *days == 0) || (*before > 0 && *days > 0) {
				return fmt.Errorf("prune requires --before UNIX_MS or --days N")
			}
			if *days > 0 {
				if *days > 365000 {
					return fmt.Errorf("days too large")
				}
				*before = time.Now().AddDate(0, 0, -*days).UnixMilli()
			}
			where, args = "started_at < ?", []any{*before}
		}
		if *path != "" {
			sc, err := scope.Detect(c, *path)
			if err != nil {
				return err
			}
			if where != "" {
				where += " AND "
			}
			where += "scope_path = ?"
			args = append(args, sc.Path)
		}
		tx, err := s.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.Exec("DELETE FROM history WHERE "+where, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if err = history.RefreshStats(tx); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, n)
		return err
	}
}

func doctor(s *db.Store, c config.Config, out io.Writer) error {
	var integrity, mode string
	var version, timeout int
	if err := s.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if err := s.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if err := s.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := s.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return err
	}
	info, err := os.Stat(c.Database.Path)
	if err != nil {
		return err
	}
	dir, err := os.Stat(filepath.Dir(c.Database.Path))
	if err != nil {
		return err
	}
	if integrity != "ok" || mode != "wal" || version != db.SchemaVersion || timeout != 1000 || info.Mode().Perm() != 0600 {
		return fmt.Errorf("database health check failed: integrity=%s journal=%s schema=%d timeout=%d permissions=%o", integrity, mode, version, timeout, info.Mode().Perm())
	}
	_, err = fmt.Fprintf(out, "database: %s\nintegrity: %s\nschema: %d\njournal: %s\nbusy_timeout: %d ms\npermissions: %04o (directory %04o)\n", c.Database.Path, integrity, version, mode, timeout, info.Mode().Perm(), dir.Mode().Perm())
	return err
}
