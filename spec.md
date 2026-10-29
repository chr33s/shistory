# shistory specification

This document describes the implemented behavior and design constraints of
shistory. It is for contributors changing the backend or shell adapters. See
[readme.md](readme.md) for installation, configuration examples, keybindings,
and release instructions. Unimplemented proposals are listed under
[Future work](#future-work).

## Purpose and boundaries

shistory stores completed Bash and Zsh execution events in SQLite. History is
shared across directories within a project; outside a project, it belongs to
the exact directory where the command began. Users can query either scope or
search across all scopes.

The adapters preserve native shell history. Scoped suggestions and keybindings
are opt-in. Selecting a command inserts it into the editing buffer and never
executes it automatically. No database sync occurs.

Repeated executions remain distinct events. Deduplication and frecency ranking
change query results, not the underlying execution log.

## Architecture

```text
Zsh preexec/precmd ─┐
                   ├── shistory CLI ── SQLite
Bash DEBUG/prompt ──┘        │
                            ├── scope detection and configuration
                            ├── privacy filters and event recording
                            └── queries, ranking, and output formatting
```

The Go backend owns configuration, scope detection, persistence, migrations,
filtering, query semantics, and formatting. The shell adapters capture lifecycle
state and connect query output to the editor. Keep that division when adding
features: quoting and shell lifecycle handling belong in the adapters; shared
history behavior belongs in Go.

The implementation uses Go's `database/sql`, the pure-Go `modernc.org/sqlite`
driver, TOML configuration, and the standard-library flag parser. Releases are
CGO-free binaries for macOS and Linux on ARM64 and AMD64. This supports the
selected deployment model without a Python interpreter or SQLite CLI at runtime.
Python is used for PTY tests. fzf is optional and supplies the Ctrl-R selector.

Go is the settled implementation choice. Earlier language comparisons and
prototypes are not requirements for contributors. A daemon, an external search
service, and FTS5 are not part of the current architecture.

| Responsibility                            | Source                                                                                                       |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| Entrypoint and command dispatch           | [cmd/shistory/main.go](cmd/shistory/main.go), [internal/cli/cli.go](internal/cli/cli.go)                     |
| Configuration and ignore rules            | [internal/config/config.go](internal/config/config.go)                                                       |
| Scope detection                           | [internal/scope/detect.go](internal/scope/detect.go)                                                         |
| Database setup and migrations             | [internal/db](internal/db)                                                                                   |
| Event recording and chronological queries | [internal/history/history.go](internal/history/history.go)                                                   |
| Ranking and summary maintenance           | [internal/history/rank.go](internal/history/rank.go), [internal/history/stats.go](internal/history/stats.go) |
| Session identity                          | [internal/session/session.go](internal/session/session.go)                                                   |
| Shell adapters                            | [shell/shistory.zsh](shell/shistory.zsh), [shell/shistory.bash](shell/shistory.bash)                         |
| Zinit entrypoint and completion           | [shistory.plugin.zsh](shistory.plugin.zsh), [shell/completions/_shistory](shell/completions/_shistory)       |

## Scope model

Each stored event contains:

| Field        | Meaning                                                    |
| ------------ | ---------------------------------------------------------- |
| `cwd`        | Canonical directory where the command began.               |
| `scope_type` | `project` when a root is detected; otherwise `directory`.  |
| `scope_path` | Canonical project root, or `cwd` when no root is detected. |

Scope detection resolves symlinks and requires an existing directory. It then:

1. Tries `git -C <cwd> rev-parse --show-toplevel` when Git detection is enabled.
   Ambient `GIT_*` variables are excluded so they cannot redirect detection to
   another repository. The Git subprocess has a one-second timeout.
2. If no Git root is found, walks from `cwd` toward the filesystem root looking
   for configured markers. The nearest matching directory wins. Marker detection
   is opt-in; the default list is empty.
3. Falls back to the exact canonical `cwd`.

Git roots take precedence over markers. Linked Git worktrees have separate
project scopes. Marker names may include `go.mod`, `package.json`, or
`pyproject.toml`; they must be individual names, not paths. Other VCS metadata
can be configured as markers, but there is no separate Mercurial or SVN detector.

For example, executions from `/home/me/src/acme/api` and
`/home/me/src/acme/web` share the scope `/home/me/src/acme` when that is their Git
root. An execution from `/home/me/Downloads` belongs only to that directory when
no project root is detected.

### Query scope

| Option          | Behavior                                                                               |
| --------------- | -------------------------------------------------------------------------------------- |
| No scope option | Detect the scope from the current directory. For `search`, apply `search.scope` first. |
| `--cwd DIR`     | Use `DIR` as the directory for scope detection.                                        |
| `--directory`   | Match the exact canonical directory instead of its project root.                       |
| `--scope PATH`  | Match a supplied scope path; intended for the canonical path cached by an adapter.     |
| `--global`      | Query all scopes.                                                                      |

`--global`, `--directory`, and `--scope` are mutually exclusive. Query
`--scope` expands the supplied path but does not rediscover its project root;
`delete --scope` and `prune --scope` detect the scope from the supplied directory.

The adapters capture `cwd` before execution. Therefore `cd ../other-project`
belongs to the directory it was entered from. The backend detects the scope
from that captured directory when it records the completed event.

## Recording and session identity

A shell session calls `shistory session` once to obtain a random 128-bit ID,
encoded as 32 hexadecimal characters. It maintains a positive, increasing
sequence number for completed events. The database's unique `(session_id, seq)`
constraint makes repeated delivery of an event idempotent.

The lifecycle is:

```text
before execution:
    capture the starting directory and timestamp
    Zsh also captures the complete command text here

at the next prompt:
    capture exit status and calculate duration
    Bash obtains the complete accepted history entry here
    submit one completed event with shistory record
    clear pending state
```

There is no open database transaction while the user's command runs. The adapter
makes one `record` call per captured event; the backend inserts the event and
updates its command summary in the same transaction. There are no `begin` or
`end` CLI commands.

The adapter-to-backend contract is:

```sh
shistory record \
  --session "$SHISTORY_SESSION_ID" \
  --seq "$SHISTORY_SEQ" \
  --shell "$SHISTORY_SHELL" \
  --cwd "$command_start_cwd" \
  --started-at "$started_at" \
  --status "$exit_status" \
  --duration-ms "$duration_ms" \
  --command "$command"
```

These variables represent values captured by the adapter. `record` requires a
nonempty session ID and shell name, a positive sequence number, and a nonnegative
start time in Unix milliseconds. Status, when supplied, must be 0–255; duration
must be nonnegative milliseconds. Omitted status and duration are stored as NULL.
The CLI defaults the start time to now and the hostname to the local hostname.

Original command text, including tabs and newlines, is retained as supplied to
the backend. Ignore rules run before insertion. Commands excluded by the shell
before the adapter can observe them cannot be recovered.

### Zsh capture

`preexec` saves the full command text, starting directory, and timestamp.
`precmd` captures the exit status before other work, computes the duration, and
records the event. Hooks are registered with `add-zsh-hook`; the adapter does
not replace hook arrays or define a competing standalone `precmd` function.

The adapter caches the current scope at startup and refreshes it through
`chpwd`. `_shistory_refresh_scope` supports explicit invalidation, for example
after creating a repository without changing directories. Suggestions and
navigation use the cached scope to avoid running Git detection on each keypress.

### Bash capture

The DEBUG trap captures the starting directory and time once per accepted
history entry. At the next prompt, the adapter reads the complete entry from
native history. It does not treat each `$BASH_COMMAND` value as a separate event:
DEBUG also fires for individual commands within pipelines and shell internals.

The adapter chains existing DEBUG code and both scalar and array
`PROMPT_COMMAND` forms. It retains the incoming status for existing hooks.
`histappend` is enabled and `history -a` runs at each prompt; native `HISTFILE`,
`HISTCONTROL`, and `HISTIGNORE` remain configured by the user.

Bash 3.2 supports recording and CLI commands. Readline Ctrl-R requires Bash 4+.
On Bash versions without `EPOCHREALTIME`, timestamps have one-second resolution.
With `cmdhist` and `lithist`, multiline entries are retained as typed; without
`lithist`, Bash may normalize them into semicolon-separated text.

### Capture limits

Both adapters finalize events at a later prompt. Commands that terminate or
replace the shell, such as `exit` and `exec`, cannot be finalized this way.
A shell killed before its next prompt also loses its pending event.

Bash history suppression, including `ignoredups` and `erasedups`, may prevent
repeated accepted lines from being captured. Users who want every execution
must omit those suppression options. Backend write failures are suppressed by
the adapters so recording errors do not interrupt interactive use.

## Storage and concurrency

The default database is `$XDG_DATA_HOME/shistory/history.db`, falling back to
`~/.local/share/shistory/history.db`. Configuration and `SHISTORY_DB` can select
a different path. The schema is defined by the embedded migrations:

- [001_init.sql](internal/db/001_init.sql) creates the execution log and its indexes.
- [002_command_stats.sql](internal/db/002_command_stats.sql) adds command summaries,
  invalidation tracking, and maintenance triggers.

The current schema version is 2. Existing version-1 databases upgrade
automatically without removing execution events. Their existing command groups
are marked for summary construction; the first query that uses summaries builds
them. That first query can take longer on a large history. Newer, unsupported
schema versions are rejected.

### Execution log

The `history` table stores `id`, `session_id`, `seq`, `started_at`, `duration_ms`,
`command`, `exit_status`, `cwd`, `scope_type`, `scope_path`, `shell`, `hostname`,
and `tty`. Time values are milliseconds. Nullable status and duration distinguish
missing metadata from successful or zero-duration executions.

Indexes support scope/time, directory/time, and global/time ordering, with row ID
as the timestamp tie-breaker. A unique session/sequence index suppresses duplicate
delivery. A scope/directory/command/time index supports summary reconstruction.
Do not deduplicate stored events by command text.

### Command summaries

`command_stats` has one row per exact `(scope_path, cwd, command)` combination.
It stores the execution count, newest timestamp and row ID, and a decayed weight
relative to that timestamp. Anchoring the weight at the newest event avoids an
exponentially growing weight measured from a fixed epoch.

Insert triggers update summaries transactionally, including out-of-order events.
Delete and update triggers mark affected groups in `dirty_commands`. Deletion
and pruning rebuild affected groups before committing. Queries using summaries
also repair dirty groups, including groups awaiting their first build after an
upgrade.

Unfiltered, deduplicated queries can combine these summaries. Session, hostname,
and failure filters calculate statistics from matching raw events. Queries with
deduplication disabled also use raw events. If a summary contains a future
timestamp, ranking falls back to raw events to clamp each contribution correctly.

### Locking and snapshots

SQLite uses WAL, `synchronous=NORMAL`, a 1000 ms busy timeout, and short write
transactions. Each backend process uses one open database connection. First-run
migration is serialized and rechecks the schema version after taking the write
lock. The rollback-to-WAL transition retries BUSY responses within a one-second
budget.

Ranked queries read statistics and representative events in one database
snapshot. If concurrent summary construction prevents a reader from upgrading
its snapshot to a writer, the query retries from a fresh snapshot within the
configured retry budget. No transaction spans the execution of a user command.

## Query contracts

The CLI provides `record`, `list`, `search`, `suggest`, `navigate`, `scope`,
`doctor`, `delete`, `prune`, `vacuum`, and `session`. `shistory help` lists the
commands; `<command> --help` lists its flags.

| Command    | Default result                                                          |
| ---------- | ----------------------------------------------------------------------- |
| `list`     | Execution events, newest first, without deduplication.                  |
| `search`   | Matching commands, deduplicated and frecency-ranked.                    |
| `suggest`  | One command matching a literal, case-sensitive prefix, frecency-ranked. |
| `navigate` | One older or newer event for chronological editor navigation.           |
| `scope`    | Detected scope path, or scope metadata with `--json`.                   |

`search.rank` controls ranking for search and suggestions. `search.dedupe`
controls search's default deduplication. `history.max_results` defaults to 100
and applies to list/search; suggest and navigate default to one result.
`--limit` overrides the default and must be between 1 and 100,000.

Session, hostname, failure, scope, directory, and text filters apply before
aggregation and ranking. `--failed` selects nonzero exit statuses; events with
unknown status do not match. Matching and ranking happen before the displayed
result limit is applied.

### Matching

CLI search defaults to a literal substring match. `%`, `_`, and backslashes are
escaped before constructing the SQL `LIKE` pattern. It uses SQLite's default
LIKE case behavior: ASCII letters match without regard to case; non-ASCII case
variants are distinct.

`search --match fuzzy` matches query characters in order, allowing gaps: `gts`
matches `git status`. Queries with no uppercase character ignore case, including
Unicode letters. Any uppercase character makes the query case-sensitive. Spaces
and punctuation are literal. The backend does not interpret fzf's extended
search operators.

Suggestions always use a literal, case-sensitive prefix, including literal
`%` and `_` characters. Navigation uses a substring query.

### Ranking and deduplication

`--rank frecent` is the default for search and suggestions. For each exact
command, sum the contributions of its matching executions:

```text
age_days = max(0, (now_ms - started_at_ms) / 86400000)
score = sum(2^(-age_days / 30))
```

Each run contributes one initially, one-half after 30 days, and one-quarter after
60 days. Future timestamps contribute at most one. Frequencies from excluded
scopes or filtered-out events do not affect the score. Equal scores are ordered
by newest matching execution time, then row ID, both descending.

`--rank recent` orders results by execution time and row ID without using the
score to determine their position. Listing and navigation remain chronological.

With deduplication enabled, each exact command appears once, represented by its
newest matching event. This collapses nonadjacent repetitions as well as adjacent
ones. With `--dedupe=false`, every matching execution is eligible for display;
under frecency ranking, each uses its command's aggregate score, with the event's
own timestamp and ID breaking ties.

For a scope containing these executions in order:

```text
./manage.py migrate --plan
alembic upgrade head
./manage.py migrate
```

literal search with explicit recency ordering produces:

```text
$ shistory search --rank recent --format raw migrate
./manage.py migrate
./manage.py migrate --plan
```

`alembic upgrade head` is excluded because it does not contain `migrate`.
The `raw` format in this example deliberately omits table metadata.

### Navigation

Navigation cursors identify a stored row; the backend resolves its timestamp
and compares `(started_at, id)` tuples. `--before-id` walks older events and
`--after-id` walks newer events. Concurrent inserts do not shift an offset.
A cursor whose row has been deleted returns an error. Ranked or deduplicated
queries do not support navigation cursors.

The Zsh widgets retain the original query, current row ID, last returned buffer,
and scope. An empty buffer traverses the scope; a nonempty buffer filters by
substring. Repeated Up walks older matches. Down walks newer matches and restores
the original query at the newest boundary. Editing the returned buffer or
changing scope resets navigation.

### Output formats

| Format             | Contract                                                                                                                                |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| `table`            | Row ID, execution timestamp, exit status, and quoted command text; grouped/ranked results also show execution count.                    |
| `json`             | Structured event array. Grouped/ranked results add `executions`, `last_used`, and `score`; zero-valued aggregate fields may be omitted. |
| `raw`              | Original command text followed by an output newline.                                                                                    |
| `nul`              | Original commands separated by NUL bytes.                                                                                               |
| `fzf`              | `ID<TAB>command<NUL>` records, preserving multiline commands.                                                                           |
| Navigation default | `ID<TAB>command` with no added newline.                                                                                                 |

`suggest` defaults to `raw`; `navigate` defaults to its navigation format and can
also return JSON. Table output quotes command text so embedded newlines and
terminal control characters do not corrupt rows. Adapters must split IDs from
command text and preserve original trailing newlines. Use the existing adapters
as the implementation reference rather than reimplementing command substitution.

## Interactive integration

The adapters expose `h` for listing, `hs` for search using the configured default
scope, and `hg` for global search, while preserving existing commands with those
names. Loading an adapter twice is safe; re-executing the shell creates a new
session. Loading the plugin neither builds nor downloads the binary.

### Ctrl-R

The selector pipeline is:

```text
initial buffer or changed query
    -> backend fuzzy match over the selected scope
    -> backend aggregation and ranking
    -> displayed result limit
    -> fzf selection
    -> insert command text into the shell editor
```

The backend owns both matching and ranking. fzf receives `--disabled --no-sort`
and NUL-separated records, so it does not apply its own matching or scoring.
Query changes reload results after a 100 ms debounce. Each reload searches the
full selected scope before limiting results; there is no fixed 10,000-event
candidate window.

Zsh uses its cached scope. Bash passes the current directory to search, which
also respects `search.scope`. Query placeholders are shell-quoted by fzf, and
reload commands use `sh -c`. Cancellation leaves the original editing buffer
unchanged. Multiline selections, tabs, quotes, and trailing newlines must survive
selection without execution.

### Zsh plugins and bindings

| Plugin                         | Integration                                                                                                                          |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `zsh-completions`              | Keep completion behavior; the entrypoint adds shistory's completion directory and registers `_shistory` when `compdef` is available. |
| `zsh-autosuggestions`          | Expose the `shistory` strategy; the user enables it with `ZSH_AUTOSUGGEST_STRATEGY=(shistory completion)`.                           |
| `zsh-history-substring-search` | Preserve native/global widgets; users may bind shistory's widgets to arrows and retain native widgets on alternate keys.             |

The adapter adds its navigation widgets to `ZSH_AUTOSUGGEST_CLEAR_WIDGETS`
without replacing existing entries. It registers hooks through `add-zsh-hook`.
The [README's Zinit example](readme.md#zinit-and-zsh-plugins) provides the load
order, completion replay, scoped bindings, and optional Ctrl-P/Ctrl-N fallback.
Arrow-key escape sequences vary by terminal; explicit bindings use the user's
actual sequences.

No bindings change by default. `SHISTORY_BIND_KEYS=1` enables Ctrl-R in Zsh and
Bash 4+; in Zsh it also binds available terminfo Up/Down sequences. Zsh users
can instead bind `shistory-history-up`, `shistory-history-down`, and
`shistory-reverse-search` individually.

Suggestions are latency-sensitive because they may run on each buffer change.
The design targets are less than 5 ms ideally and less than 15 ms for usable
process startup plus query time. These are targets, not guarantees. See
[the measured workload and timings](readme.md#performance-and-database-upgrades)
for the current implementation.

## Configuration, privacy, and maintenance

The configuration file is `$XDG_CONFIG_HOME/shistory/config.toml`, falling back
to `~/.config/shistory/config.toml`. A missing default file is allowed; an explicit
missing `SHISTORY_CONFIG` is an error. Unknown settings and invalid values are
rejected. The [README configuration example](readme.md#configuration-and-privacy)
is the user-facing reference.

| Setting                        | Default or effect                                                                |
| ------------------------------ | -------------------------------------------------------------------------------- |
| `database.path`                | XDG data path, or `~/.local/share/shistory/history.db`.                          |
| `root.git`                     | `true`; `root.vcs` is an alias controlling the same Git setting.                 |
| `root.markers`                 | Empty; opt in to marker-based roots.                                             |
| `history.ignore_leading_space` | `true`.                                                                          |
| `history.store_failed`         | `true`.                                                                          |
| `history.max_results`          | `100`.                                                                           |
| `history.ignore`               | Empty list of Go regular expressions.                                            |
| `search.scope`                 | `auto`; also accepts `directory` or `global`. Applies to search's default scope. |
| `search.dedupe`                | `true`.                                                                          |
| `search.rank`                  | `frecent`; also accepts `recent`. Applies to search and suggestions.             |

`SHISTORY_CONFIG` selects a config file. `SHISTORY_DB` overrides the database
path. `SHISTORY_IGNORE` adds one Go regular expression to the configured ignore
rules. Blank commands are ignored. Leading-space commands are ignored by default;
failed events are ignored when `history.store_failed` is false.

The database may contain credentials pasted into commands. Ignore rules do not
perform secret detection. The database file has mode 0600 and its dedicated
`shistory` directory has mode 0700. Custom shared parent directories are not
chmodded, and a database path that names a symlink or another nonregular file
is rejected. There is no synchronization or redaction feature.

| Command                  | Effect                                                                                                                             |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| `delete --id ID`         | Remove one execution event.                                                                                                        |
| `delete --scope DIR`     | Detect the scope of `DIR` and remove its events.                                                                                   |
| `prune --before UNIX_MS` | Remove events with a start time strictly before the cutoff.                                                                        |
| `prune --days N`         | Compute a calendar-day cutoff; pruning is global unless `--scope DIR` is supplied.                                                 |
| `vacuum`                 | Reclaim database space.                                                                                                            |
| `doctor`                 | Check configuration loading, database integrity, schema version, WAL, timeout, and file permissions; report directory permissions. |

Deletion and pruning print the number of removed execution rows. Their summary
updates commit with the deletion. `vacuum` is a separate operation.

## Verification and acceptance criteria

`mise run verify` runs Go tests, `go vet`, and interactive Bash/Zsh PTY tests.
`mise run compat-test` downloads the pinned revisions in
[tests/plugins.json](tests/plugins.json) and exercises the Zinit profile.
CI runs both on macOS and Linux. `mise run benchmark` measures 100k- and
1m-event datasets; it is a performance measurement, not a fixed latency gate.

Changes must preserve these behaviors:

- Project subdirectories share history; other projects and unrelated directories
  do not leak into scoped results. Worktrees and symlinks follow the scope rules.
- `cd` is recorded against its starting directory. Pipelines and multiline input
  are captured as one event where the shell lifecycle permits it.
- Repeated executions remain stored; repeated delivery of the same session/sequence
  does not create another event. Ignore rules retain their privacy behavior.
- Search deduplicates nonadjacent commands and computes frequencies within all
  active filters. Literal prefix characters and fuzzy case rules remain intact.
- Search can find a command older than 10,000 newer events. Live Ctrl-R can find
  results outside its initial display limit without executing query syntax.
- Suggestions render in the current scope. Scoped Up/Down retains its cursor
  across concurrent inserts and restores the original query at the newest boundary.
- Selection preserves multiline text and trailing newlines; cancellation preserves
  the original buffer. Selecting a command does not execute it.
- Multiple shells can initialize and write the database concurrently. Schema
  upgrades preserve events; summary rebuilding, deletion, and pruning retain
  accurate counts and representative rows.
- `compinit` and existing completions work; native/global fallback bindings remain
  available; existing hooks are retained. Changing scopes refreshes editor state.
- Re-sourcing does not duplicate hooks, and `exec zsh` starts a new session without
  duplicate registration.

## Future work

These proposals are not implemented and are not release commitments:

| Proposal                                  | Constraint or reason to revisit                                                                                           |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Import `.zsh_history` and `.bash_history` | Define how imported entries represent missing scope, status, duration, and session metadata.                              |
| Analytics and export                      | Preserve the distinction between execution events and aggregated commands.                                                |
| Separate executable-name field            | Could support tool-level analytics; exact-command counts already work without normalization.                              |
| FTS5 or another SQLite search index       | Consider when measured workloads or tokenized matching requirements justify it; retain literal and fuzzy query contracts. |
| Bash scope cache or daemon                | Revisit if measured startup or scope-detection cost warrants the added lifecycle complexity.                              |
| Configurable redaction                    | Treat it as best-effort handling, not a secret-detection security boundary.                                               |
| Encrypted sync                            | Design encryption and conflict handling explicitly before introducing synchronization.                                    |
