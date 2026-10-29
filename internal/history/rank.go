package history

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chr33s/shistory/internal/db"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var errFutureSummary = fmt.Errorf("future summary requires raw events")

const halfLifeMS = 30 * 24 * 60 * 60 * 1000.0

type candidate struct {
	id         int64
	command    string
	lastUsed   int64
	executions int64
	score      float64
}

// Fuzzy search is an ordered subsequence match. Lowercase queries ignore case;
// a query containing an uppercase rune is case-sensitive. All punctuation and
// spaces are literal, with no fzf extended-search operators.
func fuzzyMatcher(query string) func(string) bool {
	sensitive := strings.IndexFunc(query, unicode.IsUpper) >= 0
	if !sensitive {
		query = strings.ToLower(query)
	}
	needle := []rune(query)
	return func(command string) bool {
		if len(needle) == 0 {
			return true
		}
		i := 0
		for _, r := range command {
			if !sensitive {
				r = unicode.ToLower(r)
			}
			if r == needle[i] {
				i++
				if i == len(needle) {
					return true
				}
			}
		}
		return false
	}
}

func ranked(s *db.Store, q Query, where string, args []any) ([]Event, error) {
	// Two first queries after an upgrade can both discover dirty summaries.
	// SQLite cannot upgrade the losing reader's snapshot to a writer; retry the
	// entire snapshot, within the same one-second budget used for other locks.
	deadline := time.Now().Add(time.Second)
	for {
		result, err := rankedSnapshot(s, q, where, args)
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY || !time.Now().Before(deadline) {
			return result, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func rankedSnapshot(s *db.Store, q Query, where string, args []any) ([]Event, error) {
	if q.BeforeID != 0 || q.AfterID != 0 {
		return nil, fmt.Errorf("ranked or deduplicated queries do not support navigation cursors")
	}
	now := q.Now
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	matches := fuzzyMatcher(q.Text)
	groups := map[string]*candidate{}
	var all []candidate
	tx, err := s.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	useStats := q.Dedupe && !q.Failed && q.Session == "" && q.Hostname == ""
	if useStats {
		if err = RefreshStats(tx); err != nil {
			return nil, err
		}
	}
	collect := func(summary bool) error {
		query := "SELECT id,command,started_at,1,1.0 FROM history WHERE " + where
		if summary {
			query = "SELECT last_id,command,last_used,executions,weight FROM command_stats WHERE " + where
		}
		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item candidate
			var weight float64
			if err = rows.Scan(&item.id, &item.command, &item.lastUsed, &item.executions, &weight); err != nil {
				return err
			}
			// Future timestamps need individual clamping; summaries cannot represent
			// that until their newest event is in the past.
			if summary && item.lastUsed > now {
				return errFutureSummary
			}
			if q.Match == "fuzzy" && !matches(item.command) {
				continue
			}
			group := groups[item.command]
			if group == nil {
				group = &candidate{id: item.id, command: item.command, lastUsed: item.lastUsed}
				groups[item.command] = group
			}
			group.executions += item.executions
			group.score += weight * math.Exp2(math.Min(0, (float64(item.lastUsed)-float64(now))/halfLifeMS))
			if item.lastUsed > group.lastUsed || item.lastUsed == group.lastUsed && item.id > group.id {
				group.lastUsed, group.id = item.lastUsed, item.id
			}
			if !q.Dedupe {
				all = append(all, item)
			}
		}
		return rows.Err()
	}
	err = collect(useStats)
	if err == errFutureSummary {
		groups = map[string]*candidate{}
		err = collect(false)
	}
	if err != nil {
		return nil, err
	}
	if q.Dedupe {
		for _, g := range groups {
			all = append(all, *g)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if q.Rank == "frecent" && groups[a.command].score != groups[b.command].score {
			return groups[a.command].score > groups[b.command].score
		}
		if a.lastUsed != b.lastUsed {
			return a.lastUsed > b.lastUsed
		}
		return a.id > b.id
	})
	if len(all) > q.Limit {
		all = all[:q.Limit]
	}
	// Fetch complete events only for displayed results. The scan above avoids
	// copying cwd, session and other metadata for every execution in the history.
	result := make([]Event, 0, len(all))
	for _, item := range all {
		var e Event
		err = tx.QueryRow("SELECT "+columns+" FROM history WHERE id=?", item.id).Scan(&e.ID, &e.Session, &e.Seq, &e.StartedAt, &e.Duration, &e.Command, &e.Status, &e.CWD, &e.ScopeType, &e.ScopePath, &e.Shell, &e.Hostname, &e.TTY)
		if err != nil {
			return nil, err
		}
		g := groups[item.command]
		e.Executions, e.LastUsed, e.Score = g.executions, g.lastUsed, g.score
		result = append(result, e)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
