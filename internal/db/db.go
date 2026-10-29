package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed 001_init.sql
var migration string

//go:embed 002_command_stats.sql
var commandStatsMigration string

const SchemaVersion = 2

type Store struct{ *sql.DB }

func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Tighten only our dedicated directory, never a user-supplied shared parent.
	if filepath.Base(dir) == "shistory" {
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, err
		}
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("database must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(1000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()
	d, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	s := &Store{d}
	if err := s.initialize(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	if err := s.enableWAL(); err != nil {
		return err
	}
	var version int
	if err := s.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == SchemaVersion {
		return nil
	}
	if version > SchemaVersion {
		return fmt.Errorf("unsupported database schema %d", version)
	}
	// Serialize first-run migration and recheck after acquiring the write lock.
	conn, err := s.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if err = conn.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		if _, err = conn.ExecContext(context.Background(), migration); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = conn.ExecContext(context.Background(), commandStatsMigration); err != nil {
			return err
		}
	}
	if version > SchemaVersion {
		return fmt.Errorf("unsupported database schema %d", version)
	}
	_, err = conn.ExecContext(context.Background(), "COMMIT")
	return err
}

func (s *Store) enableWAL() error {
	// The rollback-to-WAL transition can return BUSY without invoking SQLite's
	// busy handler. Retry across processes, within the same one-second budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		var mode string
		err := s.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("enable WAL: SQLite selected journal mode %q", mode)
			}
			return nil
		}
		var sqliteErr *sqlite.Error
		// Extended result codes retain the primary code in the low byte.
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			return fmt.Errorf("enable WAL: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("enable WAL: %w", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
