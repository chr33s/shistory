# shistory

shistory keeps Bash and Zsh command history in SQLite, scoped to the current Git
project or, outside a project, the exact current directory. Search commands from
that scope, search globally, or opt in to scoped suggestions and Ctrl-R.
Native shell history remains available.

## Install

Zsh 5.9 and Bash 3.2 / 5.3 are tested. Bash 4+ supports the optional Readline
Ctrl-R binding; Bash 3.2 supports recording and all CLI commands. The CLI works
without fzf. Python, SQLite's CLI, and a C compiler are not runtime dependencies.

### From a release

Download the matching macOS or Linux archive for ARM64 or AMD64 from
[GitHub Releases](https://github.com/chr33s/shistory/releases). Extract it, copy
its `shistory` binary to `~/.local/bin`, and keep the shell adapters in a permanent
location. The archives include the adapters and completions.

On macOS, verify the archive with `shasum -a 256 -c SHA256SUMS` (the command also
reports missing archives for platforms you did not download). CI signs macOS
release binaries with Developer ID and submits them to Apple for notarization.
Standalone executables cannot have notarization tickets stapled, so Gatekeeper
needs internet access to retrieve their tickets.
[Apple documents the signing and notarization checks](https://support.apple.com/en-us/102445).

### From source

Run these commands from the repository directory:

```sh
mise trust
mise install
mise run build
mkdir -p ~/.local/bin
cp bin/shistory ~/.local/bin/shistory
```

Development tools are pinned in `mise.toml`: Go 1.26.5, Python 3.14.7 (the PTY
test runner), fzf 0.74.4 (selector tests), and GitHub CLI 2.97.0 (release publishing).
Dependencies are pinned in `go.mod` and verified by `go.sum`.

### Enable recording

Add `~/.local/bin` to `PATH`, then add one of these to your shell configuration:

```zsh
# ~/.zshrc
source /path/to/shistory/shistory.plugin.zsh
```

```bash
# ~/.bashrc
source /path/to/shistory/shell/shistory.bash
```

Start a new shell to load the adapter. Loading it twice is safe; re-executing the
shell starts a fresh random session. No keybindings change by default.

## Use

```sh
h                            # list current project/directory history
hs migrate                   # search the current scope
hg kubectl                   # search all scopes
shistory list --directory    # only executions from this exact directory
shistory list --failed
shistory search --global --limit 20 'docker compose'
shistory search --rank recent git
shistory search --match fuzzy gts
shistory scope --json
shistory doctor
```

### Ranking and matching

Listing and Up/Down navigation are newest first, ordered by execution timestamp
then row ID. Search and suggestions default to **frecency**: each execution adds
`2^(-age_days / 30)` to its command's score, so a run's contribution halves every
30 days. Future timestamps contribute at most one run. Scores count only events
within the selected scope and filters; ties use the latest execution time and
row ID. Use `--rank recent` or `[search] rank = "recent"` for recency ordering.

Search and suggestions show each exact command once, represented by its newest
matching event. Storage retains every execution. `--dedupe=false` displays all
matching executions; with frecency ranking, each execution uses its command's
aggregate score, with execution time and ID breaking ties. Grouped and ranked
table results include execution counts and timestamps; their JSON results also
include `executions`, `last_used` (Unix milliseconds), and `score`.

CLI search defaults to a literal substring: `%`, `_`, and backslashes are literal.
`--match fuzzy` matches query characters in order, allowing gaps (`gts` matches
`git status`). Lowercase fuzzy queries ignore case, including Unicode; an
uppercase query character makes the match case-sensitive. Spaces and punctuation
are literal; fzf's extended query operators are not interpreted. Suggestions
always use a literal, case-sensitive prefix. `--session`, `--hostname`, and
`--failed` filter events before aggregation and ranking.

### Output formats

`--format json` returns complete structured events, `raw` prints command text,
and `nul` separates unmodified commands with NUL bytes. Table output quotes
command text to preserve multiline readability and avoid terminal control
characters. `suggest` emits command text only, with a final output newline;
`navigate` emits `ID<TAB>command` with no added newline. `fzf` format is
`ID<TAB>command<NUL>` so selections preserve multiline commands.

### Scope

The adapters capture the starting directory before execution, so `cd ../other`
belongs to the directory it was entered from. Git roots take precedence over
configured markers. Symlinks are resolved to canonical directories; linked Git
worktrees have their own project scopes. No database sync occurs.

## Configuration and privacy

Configuration defaults to `$XDG_CONFIG_HOME/shistory/config.toml`, or
`~/.config/shistory/config.toml`. Missing default configuration is fine; an
explicit missing `SHISTORY_CONFIG` is an error. Unknown settings are rejected.

```toml
[database]
path = "~/.local/share/shistory/history.db"

[root]
git = true
markers = [] # opt in, e.g. ["go.mod", "package.json", "pyproject.toml"]

[history]
ignore_leading_space = true
store_failed = true
max_results = 100
ignore = ["^ls$", "^pwd$", "^clear$"]

[search]
scope = "auto" # auto, directory, or global; affects search's default scope
dedupe = true
rank = "frecent" # frecent or recent; affects search and suggestions
```

`[root] vcs = false` is an alias for disabling Git detection. `SHISTORY_CONFIG`
selects a different config file; `SHISTORY_DB` overrides the database path;
`SHISTORY_IGNORE` adds one Go regular expression to ignore rules. Without an
explicit database path, `$XDG_DATA_HOME/shistory/history.db` is used, falling
back to `~/.local/share/shistory/history.db`.

Leading-space commands are ignored by default. The dedicated database directory
has mode 0700 and the database 0600. Custom shared parent directories are not
chmodded. Credentials can still appear in recorded commands; ignore rules do
not perform secret detection.

```sh
shistory delete --id 928
shistory delete --scope .
shistory prune --days 90          # global retention
shistory prune --before 1700000000000 --scope .
shistory vacuum
```

Deletion and pruning print the number of removed rows. Vacuum reclaims database
space. SQLite uses WAL, a 1000 ms busy timeout, and short inserts; failed backend
writes do not interrupt an interactive shell. `doctor` checks integrity, schema,
WAL, timeout, and database permissions.

## Zinit and Zsh plugins

Install the binary first; loading the plugin does not download or build it.

```zsh
zinit light zsh-users/zsh-completions
autoload -Uz compinit
compinit
zinit light zsh-users/zsh-autosuggestions
zinit light zsh-users/zsh-history-substring-search
zinit light chr33s/shistory
zicdreplay # apply completion registrations captured by Zinit

# Opt in to scoped suggestions. A scope cache refreshes at startup and chpwd.
ZSH_AUTOSUGGEST_STRATEGY=(shistory completion)

# Bind your terminal's actual sequences. No bindings change by default.
bindkey '^[[A' shistory-history-up
bindkey '^[[B' shistory-history-down
bindkey '^R' shistory-reverse-search

# Optional native/global history fallback.
bindkey '^P' history-substring-search-up
bindkey '^N' history-substring-search-down
```

The adapter adds its navigation widgets to
`ZSH_AUTOSUGGEST_CLEAR_WIDGETS` without replacing existing entries. Empty-buffer
Up/Down traverses the scope; a nonempty buffer filters by substring. Repeated Up
walks older matches, Down walks newer matches and restores the original query
at the newest boundary. Row-ID cursors remain stable when other shells insert.
Changing scope resets navigation. To refresh scope after creating a repository
without changing directories, run `_shistory_refresh_scope`.

Zsh hooks are registered using `add-zsh-hook`. Completion definitions can be
loaded before `compinit`; when `compdef` is already available the Zinit entrypoint
registers `_shistory` immediately. Existing `h`, `hs`, and `hg` commands are
preserved.

## Optional scoped Ctrl-R

Install `fzf`, then bind `shistory-reverse-search` in Zsh. On Bash 4+, set
`SHISTORY_BIND_KEYS=1` before sourcing the adapter or run:

```bash
bind -x '"\C-r":_shistory_reverse_search'
```

Selections are inserted into the editing buffer and never executed automatically.
Cancellation leaves the buffer unchanged. Ctrl-R uses the backend's fuzzy
matching and selected ranking; fzf displays results without reordering them.
Typing reloads results after a 100 ms debounce. Each query searches the full
selected scope before applying `history.max_results`, so commands outside the
initial displayed results remain discoverable. The backend also matches the
initial editing buffer.

Zsh's `SHISTORY_BIND_KEYS=1` binds Ctrl-R and terminfo Up/Down sequences when
available. Manual bindings remain the most portable choice.

## Bash behavior

Bash captures the starting directory and time once through a DEBUG trap, then
obtains the complete accepted history entry at the next prompt. Pipelines and multiline commands are
stored as one event. Both scalar and array `PROMPT_COMMAND` are chained, and an
existing DEBUG trap is retained with its incoming status. Native `HISTFILE`,
`HISTCONTROL`, and `HISTIGNORE` remain configured by the user; `histappend` is
enabled and `history -a` runs at each prompt.

Bash commands excluded from native history cannot be recovered by this approach.
In particular, `ignoredups`/`erasedups` may prevent repeated accepted lines from
being captured. To retain every execution, omit those native suppression options.
`cmdhist` and `lithist` preserve multiline entries as typed; without `lithist`,
Bash may normalize multiline syntax into semicolon-separated history text.
On older Bash versions, duration timestamps have one-second resolution.
Commands that terminate or replace the shell (`exit`, `exec`) cannot be finalized
at a later prompt. These lifecycle limits also apply to Zsh prompt-based recording.

## Development and verification

```sh
mise run verify       # Go tests, go vet, real Bash/Zsh PTY smoke tests
mise run compat-test  # downloads pinned upstream plugins, tests real Zinit/ZLE
mise run release     # CGO-free macOS/Linux, amd64/arm64 tarballs in dist/
```

The compatibility profile uses revisions in `tests/plugins.json` and tests
completion registration, rendered autosuggestions, project-only navigation,
native fallback bindings, scope transitions, retained hooks, and shell re-exec.
Go tests cover simultaneous first-run migrations/writes, worktrees, symlinks,
privacy rules, original multiline text, query filters, cursor stability,
frecency, command summaries, schema upgrades, and maintenance commands. Selector
PTY tests exercise live query reloads, commands beyond the display limit, and
literal shell syntax in queries. CI runs both profiles on macOS and Linux.

## Performance and database upgrades

Search keeps per-directory command summaries in SQLite, updated transactionally
with new events. Deletion and pruning rebuild only affected command summaries.
Session, hostname, and failure filters calculate scores from their matching raw
events. Schema v1 databases upgrade automatically to v2, preserving history;
the first query that uses summaries builds them for existing events and can take
longer on a large database. Ranked queries and their representative events use
one database snapshot.

Run the reproducible search benchmarks (100k and 1m events) with:

```sh
mise run benchmark
```

Measured on Apple M5 Max / macOS arm64, Go 1.26.5, averaging 10 iterations:

| Operation | 100k events | 1m events |
| --- | ---: | ---: |
| Scoped frecency search | 1.75 ms | 2.00 ms |
| Scoped fuzzy search | 1.38 ms | 1.44 ms |
| Global frecency search | 10.80 ms | 13.01 ms |
| Scoped suggestion, query only | 0.13 ms | 0.14 ms |
| Scoped suggestion, full CLI process | 6.55 ms | 7.01 ms |

The fixture has 20 projects with 1,000 distinct commands each, shared across
projects, repeated over 90 days. Query timings use an open database; CLI timings
start a fresh process each time, after warming the executable once. Dataset
creation and migration backfill are excluded. Summary query cost grows with the
number of distinct command/directory combinations; filtered queries that need
raw events can be slower. These measurements support using summaries without
adding a daemon or a separate text-search index for this workload.

## Publishing releases

Each push to `main` publishes a release tagged `main-<full-commit-SHA>` after the
Linux and macOS checks pass. Archives include the binary, shell adapters,
completions, README, and license. `SHA256SUMS` lists archive checksums. Reruns
preserve published releases and resume unfinished drafts.

Local `mise run release` builds remain unsigned unless `MACOS_SIGNING_REQUIRED=1`.

### Release signing secrets

Add these repository secrets under **Settings → Secrets and variables → Actions**:

| Secret | Value |
| --- | --- |
| `DEVELOPER_ID_CERTIFICATE_BASE64` | Base64 of a Developer ID Application `.p12` export including its private key. |
| `DEVELOPER_ID_CERTIFICATE_PASSWORD` | Password protecting that `.p12` export. |
| `DEVELOPER_ID_APPLICATION` | Full signing identity: `Developer ID Application: Your Name (TEAMID)`. |
| `APPLE_ID` | Apple developer account email used for notarization. |
| `APPLE_TEAM_ID` | Developer team ID matching the signing certificate. |
| `APPLE_APP_SPECIFIC_PASSWORD` | App-specific Apple account password for notarization. |

On macOS, `base64 -i DeveloperIDApplication.p12 | pbcopy` prepares the certificate
secret. CI generates a random password for a temporary keychain and removes the
keychain and certificate export when signing finishes or fails. Missing secrets
or failed notarization prevent publication; PR checks do not use these secrets.

## Design and future work

See [the specification](spec.md) for the current architecture, command contracts,
and acceptance criteria. Its [future-work section](spec.md#future-work) covers
history-file import, command analytics, export, FTS5 optimization, redaction,
Bash scope caching or a daemon, and encrypted sync.
