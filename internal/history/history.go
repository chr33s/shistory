package history

import (
	"fmt"
	"strings"

	"github.com/chr33s/shistory/internal/config"
	"github.com/chr33s/shistory/internal/db"
	"github.com/chr33s/shistory/internal/scope"
)

type Event struct {
	ID         int64   `json:"id"`
	Session    string  `json:"session_id"`
	Seq        int64   `json:"seq"`
	StartedAt  int64   `json:"started_at"`
	Duration   *int64  `json:"duration_ms"`
	Command    string  `json:"command"`
	Status     *int    `json:"exit_status"`
	CWD        string  `json:"cwd"`
	ScopeType  string  `json:"scope_type"`
	ScopePath  string  `json:"scope_path"`
	Shell      string  `json:"shell"`
	Hostname   string  `json:"hostname"`
	TTY        string  `json:"tty"`
	Executions int64   `json:"executions,omitempty"`
	LastUsed   int64   `json:"last_used,omitempty"`
	Score      float64 `json:"score,omitempty"`
}

func Record(s *db.Store, c config.Config, e Event) error {
	if e.Session == "" || e.Seq < 1 || e.StartedAt < 0 || e.Shell == "" {
		return fmt.Errorf("record requires session, positive seq, shell, and nonnegative started-at")
	}
	if e.Status != nil && (*e.Status < 0 || *e.Status > 255) {
		return fmt.Errorf("status must be between 0 and 255")
	}
	if e.Duration != nil && *e.Duration < 0 {
		return fmt.Errorf("duration must be nonnegative")
	}
	if c.Ignore(e.Command, e.Status) {
		return nil
	}
	sc, err := scope.Detect(c, e.CWD)
	if err != nil {
		return err
	}
	_, err = s.Exec(`INSERT INTO history(session_id,seq,started_at,duration_ms,command,exit_status,cwd,scope_type,scope_path,shell,hostname,tty)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id,seq) DO NOTHING`,
		e.Session, e.Seq, e.StartedAt, e.Duration, e.Command, e.Status, sc.CWD, sc.Type, sc.Path, e.Shell, e.Hostname, e.TTY)
	return err
}

type Query struct {
	Scope    string
	CWD      string
	Text     string
	Prefix   *string
	Limit    int
	Dedupe   bool
	Failed   bool
	Session  string
	Hostname string
	BeforeID int64
	AfterID  int64
	Rank     string
	Match    string
	Now      int64 // Optional Unix milliseconds, for reproducible ranking.
}

const columns = `id,session_id,seq,started_at,duration_ms,command,exit_status,cwd,scope_type,scope_path,shell,COALESCE(hostname,''),COALESCE(tty,'')`

func List(s *db.Store, q Query) ([]Event, error) {
	if q.Limit < 1 || q.Limit > 100000 {
		return nil, fmt.Errorf("limit must be between 1 and 100000")
	}
	if q.Rank != "" && q.Rank != "recent" && q.Rank != "frecent" {
		return nil, fmt.Errorf("rank must be recent or frecent")
	}
	if q.Match != "" && q.Match != "literal" && q.Match != "fuzzy" {
		return nil, fmt.Errorf("match must be literal or fuzzy")
	}
	if (q.BeforeID != 0 || q.AfterID != 0) && q.Rank == "frecent" {
		return nil, fmt.Errorf("navigation requires recent ranking")
	}
	where, args := queryFilter(q)
	if q.Dedupe || q.Rank == "frecent" || q.Match == "fuzzy" {
		return ranked(s, q, where, args)
	}
	return chronological(s, q, where, args)
}

func queryFilter(q Query) (string, []any) {
	where, args := []string{"1=1"}, []any{}
	add := func(clause string, values ...any) { where = append(where, clause); args = append(args, values...) }
	if q.Scope != "" {
		add("scope_path = ?", q.Scope)
	}
	if q.CWD != "" {
		add("cwd = ?", q.CWD)
	}
	if q.Text != "" && q.Match != "fuzzy" {
		pattern := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(q.Text)
		add(`command LIKE ? ESCAPE '\'`, "%"+pattern+"%")
	}
	if q.Prefix != nil {
		add("substr(command,1,length(?)) = ?", *q.Prefix, *q.Prefix)
	}
	if q.Failed {
		add("exit_status <> 0")
	}
	if q.Session != "" {
		add("session_id = ?", q.Session)
	}
	if q.Hostname != "" {
		add("hostname = ?", q.Hostname)
	}
	return strings.Join(where, " AND "), args
}

func chronological(s *db.Store, q Query, where string, args []any) ([]Event, error) {
	order := "DESC"
	if q.BeforeID != 0 || q.AfterID != 0 {
		id, op := q.BeforeID, "<"
		if q.AfterID != 0 {
			id, op, order = q.AfterID, ">", "ASC"
		}
		var started int64
		if err := s.QueryRow("SELECT started_at FROM history WHERE id = ?", id).Scan(&started); err != nil {
			return nil, fmt.Errorf("navigation cursor: %w", err)
		}
		where += " AND (started_at,id) " + op + " (?,?)"
		args = append(args, started, id)
	}
	sql := "SELECT " + columns + " FROM history WHERE " + where + " ORDER BY started_at " + order + ", id " + order
	sql += " LIMIT ?"
	args = append(args, q.Limit)
	rows, err := s.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Session, &e.Seq, &e.StartedAt, &e.Duration, &e.Command, &e.Status, &e.CWD, &e.ScopeType, &e.ScopePath, &e.Shell, &e.Hostname, &e.TTY); err != nil {
			return nil, err
		}
		result = append(result, e)
		if len(result) == q.Limit {
			break
		}
	}
	return result, rows.Err()
}
