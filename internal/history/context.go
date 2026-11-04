package history

import (
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Context ranking multiplies a command's frecency by explainable factors:
//
//	sequence   1 + w × P(command | session's previous command), from the window
//	directory  1 + w × share of frecency from the current directory (0.5 for
//	           an ancestor or descendant directory)
//	branch     1 + w × share of windowed runs on the current Git branch
//	env        1 + w × share of windowed runs with identical context.env values
//	session    1 + w × share of windowed runs in the current session
//	failure    1 − w × share of windowed runs that exited nonzero
//
// The window is the most recent Context.Window events in the query's scope,
// so cost stays bounded and deletions need no derived tables.
type contextSignals struct {
	seq      map[string]float64
	seqTotal float64
	total    map[string]float64
	branch   map[string]float64
	env      map[string]float64
	session  map[string]float64
	failed   map[string]float64
}

type windowEvent struct {
	command, session, branch, env string
	seq, started                  int64
	failed                        bool
	weight                        float64
}

func decay(started, now int64) float64 {
	return math.Exp2(math.Min(0, (float64(started)-float64(now))/halfLifeMS))
}

func loadContextSignals(tx *sql.Tx, q Query, now int64) (*contextSignals, error) {
	c := q.Context
	sig := &contextSignals{seq: map[string]float64{}, total: map[string]float64{}, branch: map[string]float64{},
		env: map[string]float64{}, session: map[string]float64{}, failed: map[string]float64{}}
	window := c.Window
	if window < 1 {
		window = 2000
	}
	where, args := queryFilter(Query{Scope: q.Scope, CWD: q.CWD, Hostname: q.Hostname}, "")
	rows, err := tx.Query(`SELECT command,session_id,seq,started_at,COALESCE(exit_status,0)<>0,COALESCE(branch,''),COALESCE(env,'')
 FROM history WHERE `+where+` ORDER BY started_at DESC,id DESC LIMIT ?`, append(args, window)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := map[string][]windowEvent{}
	for rows.Next() {
		var e windowEvent
		if err = rows.Scan(&e.command, &e.session, &e.seq, &e.started, &e.failed, &e.branch, &e.env); err != nil {
			return nil, err
		}
		e.weight = decay(e.started, now)
		sig.total[e.command] += e.weight
		if c.Branch != "" && e.branch == c.Branch {
			sig.branch[e.command] += e.weight
		}
		if c.Env != "" && e.env == c.Env {
			sig.env[e.command] += e.weight
		}
		if c.Session != "" && e.session == c.Session {
			sig.session[e.command] += e.weight
		}
		if e.failed {
			sig.failed[e.command] += e.weight
		}
		// Imported sequence numbers are hashes, not execution order.
		if !strings.HasPrefix(e.session, "import:") {
			sessions[e.session] = append(sessions[e.session], e)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if c.Session == "" {
		return sig, nil
	}
	var prev string
	err = tx.QueryRow("SELECT command FROM history WHERE session_id=? ORDER BY seq DESC LIMIT 1", c.Session).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return sig, nil
	}
	if err != nil {
		return nil, err
	}
	for _, events := range sessions {
		sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })
		for i := 1; i < len(events); i++ {
			if events[i-1].command == prev {
				sig.seq[events[i].command] += events[i].weight
				sig.seqTotal += events[i].weight
			}
		}
	}
	return sig, nil
}

// proximity is 1 for the same directory, 0.5 for an ancestor or descendant.
func proximity(dir, current string) float64 {
	if current == "" {
		return 0
	}
	if dir == current {
		return 1
	}
	within := func(child, parent string) bool {
		return strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
	}
	if within(dir, current) || within(current, dir) {
		return 0.5
	}
	return 0
}

// apply returns the boosted score and the factors that differ from 1.
func (sig *contextSignals) apply(w Context, g *candidate) (float64, map[string]float64) {
	factors := map[string]float64{}
	set := func(name string, f float64) {
		if f != 1 {
			factors[name] = f
		}
	}
	cmd := g.command
	if sig.seqTotal > 0 {
		set("sequence", 1+w.Weights.Sequence*sig.seq[cmd]/sig.seqTotal)
	}
	if g.score > 0 {
		set("directory", 1+w.Weights.Directory*g.dirScore/g.score)
	}
	if total := sig.total[cmd]; total > 0 {
		set("branch", 1+w.Weights.Branch*sig.branch[cmd]/total)
		set("env", 1+w.Weights.Env*sig.env[cmd]/total)
		set("session", 1+w.Weights.Session*sig.session[cmd]/total)
		set("failure", 1-w.Weights.Failure*sig.failed[cmd]/total)
	}
	score := g.score
	for _, f := range factors {
		score *= f
	}
	if len(factors) == 0 {
		factors = nil
	}
	return score, factors
}
