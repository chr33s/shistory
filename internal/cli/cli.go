package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chr33s/shistory/internal/config"
	"github.com/chr33s/shistory/internal/db"
	"github.com/chr33s/shistory/internal/history"
	"github.com/chr33s/shistory/internal/scope"
	"github.com/chr33s/shistory/internal/session"
)

const help = `shistory — SQLite history scoped to a project or directory

Commands:
  record    --session ID --seq N --shell SHELL --cwd DIR --command TEXT
            [--started-at UNIX_MS] [--status N] [--duration-ms N]
  list      [--cwd DIR | --scope PATH | --directory | --global]
  search    [query] [same filters as list]
  suggest   --prefix TEXT [--scope PATH | --cwd DIR] [--limit N]
  navigate  [--query TEXT] [--before-id ID | --after-id ID] [--direction older|newer]
  scope     [--cwd DIR] [--json]
  import    [--file PATH] [--shell zsh|bash] [--cwd DIR]  (default cwd: $HOME)
  doctor    Check SQLite integrity, schema, permissions, and configuration
  delete    --id ID | --scope DIR
  prune     --before UNIX_MS | --days N [--scope DIR]
  vacuum    Reclaim database space
  session   Generate a random shell session ID

Query options: --limit N --dedupe[=false] --failed --session ID --hostname NAME
Search/suggest: --rank recent|frecent|context (default from search.rank)
  --context-session ID ranks by that session's previous command (no filter)
Search: --match literal|fuzzy (default literal)
Output: --format table|json|raw|nul|fzf (default table; suggest defaults raw)
Navigation outputs ID<TAB>command, newest occurrence of each command only
(--dedupe=false visits every execution); --format json is also available.
Configuration: SHISTORY_CONFIG, SHISTORY_DB, SHISTORY_IGNORE; see readme.md.
`

func Run(args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, help)
		return err
	}
	if args[0] == "session" {
		if len(args) != 1 {
			return fmt.Errorf("session takes no arguments")
		}
		id, err := session.New()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, id)
		return err
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	name := args[0]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	cwd := fs.String("cwd", "", "command/query directory")
	format := fs.String("format", "table", "table, json, raw, nul, or fzf")
	if name == "scope" {
		asJSON := fs.Bool("json", false, "JSON output")
		if err := parse(fs, args[1:]); err != nil {
			if err == flag.ErrHelp {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("scope takes no positional arguments")
		}
		sc, err := scope.Detect(c, *cwd)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(sc)
		}
		_, err = fmt.Fprintln(out, sc.Path)
		return err
	}
	var action func(*db.Store) error
	switch name {
	case "record":
		action = recordFlags(fs, c, cwd)
	case "list", "search", "suggest", "navigate":
		action = queryFlags(fs, c, name, cwd, format, out)
	case "import":
		action = importFlags(fs, c, cwd, out)
	case "doctor":
		action = func(s *db.Store) error { return doctor(s, c, out) }
	case "delete", "prune":
		action = maintenanceFlags(fs, c, name, out)
	case "vacuum":
		action = func(s *db.Store) error { _, err := s.Exec("VACUUM"); return err }
	default:
		return fmt.Errorf("unknown command %q; run shistory help", name)
	}
	if err := parse(fs, args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if name != "search" && fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	s, err := db.Open(c.Database.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	return action(s)
}

// Permit options before or after a search query without a CLI framework.
func parse(fs *flag.FlagSet, args []string) error {
	options, positionals := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		options = append(options, a)
		key, _, equals := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(key)
		if f != nil && !equals {
			b, ok := f.Value.(interface{ IsBoolFlag() bool })
			if !ok || !b.IsBoolFlag() {
				if i+1 == len(args) {
					return fmt.Errorf("missing value for %s", a)
				}
				i++
				options = append(options, args[i])
			}
		}
	}
	return fs.Parse(append(append(options, "--"), positionals...))
}

func recordFlags(fs *flag.FlagSet, c config.Config, cwd *string) func(*db.Store) error {
	var e history.Event
	fs.StringVar(&e.Session, "session", "", "session ID")
	fs.Int64Var(&e.Seq, "seq", 0, "per-session sequence")
	fs.StringVar(&e.Shell, "shell", "", "shell name")
	fs.StringVar(&e.Command, "command", "", "original command")
	fs.Int64Var(&e.StartedAt, "started-at", time.Now().UnixMilli(), "Unix milliseconds")
	fs.StringVar(&e.Hostname, "hostname", "", "hostname")
	fs.StringVar(&e.TTY, "tty", "", "terminal")
	status := fs.Int("status", 0, "exit status")
	duration := fs.Int64("duration-ms", 0, "duration in milliseconds")
	return func(s *db.Store) error {
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "status" {
				e.Status = status
			}
			if f.Name == "duration-ms" {
				e.Duration = duration
			}
		})
		e.CWD = *cwd
		if e.Hostname == "" {
			e.Hostname, _ = os.Hostname()
		}
		if env := c.ContextEnv(os.LookupEnv); env != "" {
			e.Env = []byte(env)
		}
		return history.Record(s, c, e)
	}
}

func importFlags(fs *flag.FlagSet, c config.Config, cwd *string, out io.Writer) func(*db.Store) error {
	file := fs.String("file", "", "history file (default $HISTFILE, then ~/.zsh_history or ~/.bash_history)")
	shell := fs.String("shell", "", "zsh or bash (default from the file name, then $SHELL)")
	return func(s *db.Store) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if *shell == "" {
			for _, name := range []string{*file, os.Getenv("SHELL")} {
				if base := filepath.Base(name); strings.Contains(base, "zsh") {
					*shell = "zsh"
				} else if strings.Contains(base, "bash") {
					*shell = "bash"
				}
				if *shell != "" {
					break
				}
			}
		}
		if *shell == "" {
			*shell = "zsh"
		}
		if *file == "" {
			*file = os.Getenv("HISTFILE")
		}
		if *file == "" {
			*file = filepath.Join(home, "."+*shell+"_history")
		}
		path, err := config.Expand(*file)
		if err != nil {
			return err
		}
		if path, err = filepath.Abs(path); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		events, err := history.ParseHistory(f, *shell)
		if err != nil {
			return err
		}
		if *cwd == "" {
			*cwd = home
		}
		hostname, _ := os.Hostname()
		res, err := history.Import(s, c, path, *cwd, hostname, events)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "imported %d of %d entries from %s\n", res.Imported, res.Parsed, path)
		return err
	}
}

func queryFlags(fs *flag.FlagSet, c config.Config, name string, cwd, format *string, out io.Writer) func(*db.Store) error {
	var q history.Query
	path := fs.String("scope", "", "cached scope path")
	global := fs.Bool("global", false, "all scopes")
	directory := fs.Bool("directory", false, "exact cwd")
	fs.IntVar(&q.Limit, "limit", c.History.MaxResults, "maximum displayed rows")
	fs.BoolVar(&q.Dedupe, "dedupe", (name == "search" && c.Search.Dedupe) || name == "suggest" || name == "navigate", "show each exact command once")
	fs.BoolVar(&q.Failed, "failed", false, "failed commands")
	fs.StringVar(&q.Session, "session", "", "session filter")
	fs.StringVar(&q.Hostname, "hostname", "", "hostname filter")
	fs.Int64Var(&q.BeforeID, "before-id", 0, "older than row ID")
	fs.Int64Var(&q.AfterID, "after-id", 0, "newer than row ID")
	fs.StringVar(&q.Text, "query", "", "substring")
	if name == "search" || name == "suggest" {
		fs.StringVar(&q.Rank, "rank", c.Search.Rank, "recent, frecent, or context")
		fs.StringVar(&q.Context.Session, "context-session", "", "current session, for context ranking")
	}
	if name == "search" {
		fs.StringVar(&q.Match, "match", "literal", "literal substring or fuzzy subsequence")
	}
	direction := fs.String("direction", "older", "older or newer")
	prefix := fs.String("prefix", "", "literal prefix")
	if name == "suggest" {
		q.Limit = 1
		*format = "raw"
	}
	if name == "navigate" {
		q.Limit = 1
		q.Navigate = true
		*format = "navigate"
	}
	return func(s *db.Store) error {
		if *direction != "older" && *direction != "newer" {
			return fmt.Errorf("direction must be older or newer")
		}
		if q.BeforeID < 0 || q.AfterID < 0 || (q.BeforeID != 0 && q.AfterID != 0) {
			return fmt.Errorf("supply one positive navigation cursor")
		}
		if *global && (*directory || *path != "") || *directory && *path != "" {
			return fmt.Errorf("global, directory, and scope are mutually exclusive")
		}
		if name == "search" && fs.NArg() > 0 {
			if q.Text != "" {
				return fmt.Errorf("supply query once")
			}
			q.Text = strings.Join(fs.Args(), " ")
		}
		if name == "suggest" {
			q.Prefix = prefix
		}
		if err := resolveQuery(c, &q, *cwd, *path, *global, *directory, name == "search"); err != nil {
			return err
		}
		if q.Rank == "context" {
			if err := resolveContext(c, &q, *cwd); err != nil {
				return err
			}
		}
		if name == "navigate" && *direction == "newer" && q.AfterID == 0 {
			return nil
		}
		if !validFormat(*format) && *format != "navigate" {
			return fmt.Errorf("unknown format %q", *format)
		}
		rows, err := history.List(s, q)
		if err != nil {
			return err
		}
		return printEvents(out, *format, rows)
	}
}

func resolveQuery(c config.Config, q *history.Query, cwd, path string, global, directory, useDefault bool) error {
	if useDefault && !global && !directory && path == "" {
		global = c.Search.Scope == "global"
		directory = c.Search.Scope == "directory"
	}
	if global {
		return nil
	}
	if path != "" {
		var err error
		q.Scope, err = config.Expand(path)
		return err
	}
	sc, err := scope.Detect(c, cwd)
	if err != nil {
		return err
	}
	if directory {
		q.CWD = sc.CWD
	} else {
		q.Scope = sc.Path
	}
	return nil
}

// resolveContext captures the querying shell's directory, branch, and
// configured environment. The process inherits the shell's working directory.
func resolveContext(c config.Config, q *history.Query, cwd string) error {
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	dir, err := scope.Canonical(cwd)
	if err != nil {
		return err
	}
	q.Context.CWD = dir
	if c.Root.Git {
		q.Context.Branch = scope.Branch(dir)
	}
	q.Context.Env = c.ContextEnv(os.LookupEnv)
	q.Context.Window = c.Context.Window
	q.Context.Weights = c.Context.Weights
	return nil
}

func validFormat(format string) bool {
	return format == "table" || format == "json" || format == "raw" || format == "nul" || format == "fzf"
}

func printEvents(out io.Writer, format string, events []history.Event) error {
	if format == "json" {
		return json.NewEncoder(out).Encode(events)
	}
	for _, e := range events {
		var err error
		switch format {
		case "raw":
			_, err = io.WriteString(out, e.Command)
			if err == nil {
				_, err = io.WriteString(out, "\n")
			}
		case "nul":
			_, err = io.WriteString(out, e.Command+"\x00")
		case "fzf":
			_, err = fmt.Fprintf(out, "%d\t%s\x00", e.ID, e.Command)
		case "navigate":
			_, err = fmt.Fprintf(out, "%d\t%s", e.ID, e.Command)
		default:
			status := "?"
			if e.Status != nil {
				status = strconv.Itoa(*e.Status)
			}
			// Quoting prevents embedded newlines and terminal controls from corrupting rows.
			if e.Executions > 0 {
				_, err = fmt.Fprintf(out, "%6d  %s  %3s  %5dx  %s\n", e.ID, time.UnixMilli(e.StartedAt).Format(time.RFC3339), status, e.Executions, strconv.Quote(e.Command))
			} else {
				_, err = fmt.Fprintf(out, "%6d  %s  %3s  %s\n", e.ID, time.UnixMilli(e.StartedAt).Format(time.RFC3339), status, strconv.Quote(e.Command))
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
