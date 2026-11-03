package history

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
	"strings"

	"github.com/chr33s/shistory/internal/config"
	"github.com/chr33s/shistory/internal/db"
	"github.com/chr33s/shistory/internal/scope"
)

// ImportResult counts parsed entries and rows that were not already present.
type ImportResult struct {
	Parsed   int `json:"parsed"`
	Imported int `json:"imported"`
}

// ParseHistory reads a zsh (plain or EXTENDED_HISTORY) or bash (optionally
// HISTTIMEFORMAT-stamped) history file. Entries without a timestamp get 0.
func ParseHistory(r io.Reader, shell string) ([]Event, error) {
	if shell != "zsh" && shell != "bash" {
		return nil, fmt.Errorf("import shell must be zsh or bash")
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var events []Event
	var pending *Event
	var stamp int64
	for sc.Scan() {
		line := sc.Text()
		if shell == "zsh" {
			line = unmetafy(line)
			// Continuation of a multi-line command ends with a backslash.
			if pending != nil {
				pending.Command += "\n" + line
			} else {
				e := Event{Shell: shell, Command: line}
				if strings.HasPrefix(line, ": ") {
					if meta, cmd, ok := strings.Cut(line[2:], ";"); ok {
						start, dur, _ := strings.Cut(meta, ":")
						if t, err := strconv.ParseInt(start, 10, 64); err == nil && t >= 0 {
							e.StartedAt, e.Command = t*1000, cmd
							if d, err := strconv.ParseInt(dur, 10, 64); err == nil && d >= 0 {
								ms := d * 1000
								e.Duration = &ms
							}
						}
					}
				}
				pending = &e
			}
			if strings.HasSuffix(pending.Command, "\\") {
				pending.Command = strings.TrimSuffix(pending.Command, "\\")
				continue
			}
			events = append(events, *pending)
			pending = nil
			continue
		}
		if len(line) > 1 && line[0] == '#' {
			if t, err := strconv.ParseInt(line[1:], 10, 64); err == nil && t >= 0 {
				stamp = t * 1000
				continue
			}
		}
		events = append(events, Event{Shell: shell, Command: line, StartedAt: stamp})
	}
	if pending != nil {
		events = append(events, *pending)
	}
	out := events[:0]
	for _, e := range events {
		if strings.TrimSpace(e.Command) != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// zsh stores bytes 0x83..0xA2 as 0x83 followed by the byte XOR 32.
func unmetafy(s string) string {
	if strings.IndexByte(s, 0x83) < 0 {
		return s
	}
	b := []byte(s)
	var out bytes.Buffer
	for i := 0; i < len(b); i++ {
		if b[i] == 0x83 && i+1 < len(b) {
			i++
			out.WriteByte(b[i] ^ 32)
		} else {
			out.WriteByte(b[i])
		}
	}
	return out.String()
}

// Import attributes events to cwd's scope; history files record no directory.
// Rows are keyed by source, timestamp and command, so re-importing a file that
// zsh has rewritten (for example with HIST_SAVE_NO_DUPS) adds no duplicates.
func Import(s *db.Store, c config.Config, source, cwd, hostname string, events []Event) (ImportResult, error) {
	res := ImportResult{Parsed: len(events)}
	sc, err := scope.Detect(c, cwd)
	if err != nil {
		return res, err
	}
	tx, err := s.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO history(session_id,seq,started_at,duration_ms,command,exit_status,cwd,scope_type,scope_path,shell,hostname,tty)
		VALUES(?,?,?,?,?,NULL,?,?,?,?,?,'') ON CONFLICT(session_id,seq) DO NOTHING`)
	if err != nil {
		return res, err
	}
	defer stmt.Close()
	session := "import:" + source
	for _, e := range events {
		if c.Ignore(e.Command, nil) {
			continue
		}
		h := fnv.New64a()
		fmt.Fprintf(h, "%s\x00%d\x00%s", sc.Path, e.StartedAt, e.Command)
		seq := int64(h.Sum64()>>1) | 1
		r, err := stmt.Exec(session, seq, e.StartedAt, e.Duration, e.Command, sc.CWD, sc.Type, sc.Path, e.Shell, hostname)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Imported++
		}
	}
	return res, tx.Commit()
}
