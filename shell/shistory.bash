# Bash 3.2+; source only in an interactive shell.
[[ $- == *i* ]] || return 0
[[ ${_SHISTORY_BASH_LOADED:-0} == 1 ]] && return 0
if ! command -v shistory >/dev/null 2>&1; then
  printf '%s\n' 'shistory: add the shistory binary to PATH before loading the plugin' >&2
  return 1
fi
_SHISTORY_BASH_LOADED=1
SHISTORY_SESSION_ID=$(command shistory session)
SHISTORY_SEQ=0
_shistory_pending=0
_shistory_ready=0
_shistory_last_history=''
shopt -s histappend

_shistory_now() {
  if [[ -n ${EPOCHREALTIME:-} ]]; then
    local stamp=${EPOCHREALTIME/./}
    _shistory_now_ms=$((10#$stamp / 1000))
  else
    _shistory_now_ms=$(command date +%s)
    _shistory_now_ms=$((_shistory_now_ms * 1000))
  fi
}

_shistory_debug() {
  # Called only from the top-level trap. One capture per accepted history entry.
  [[ ${_shistory_ready:-0} == 1 && ${_shistory_pending:-0} == 0 ]] || return 0
  local entry
  entry=$(HISTTIMEFORMAT= builtin history 1)
  [[ -n $entry && $entry != "$_shistory_last_history" ]] || return 0
  _shistory_pending_cwd=$PWD
  _shistory_now
  _shistory_started_at=$_shistory_now_ms
  _shistory_pending=1
}

_shistory_prompt() {
  local exit_code=$?
  _shistory_ready=0
  local entry number command_text
  # The sentinel keeps command text's final newlines intact.
  entry=$(HISTTIMEFORMAT= builtin history 1; printf '.')
  entry=${entry%.}
  entry=${entry%$'\n'}
  if [[ $_shistory_pending == 1 && $entry != "$_shistory_last_history" ]]; then
    if [[ $entry =~ ^[[:space:]]*([0-9]+)[[:space:]][[:space:]](.*)$ ]]; then
      number=${BASH_REMATCH[1]}
      command_text=${BASH_REMATCH[2]}
      _shistory_now
      local duration=$((_shistory_now_ms - _shistory_started_at))
      (( duration < 0 )) && duration=0
      SHISTORY_SEQ=$((SHISTORY_SEQ + 1))
      command shistory record --session "$SHISTORY_SESSION_ID" --seq "$SHISTORY_SEQ" \
        --shell bash --cwd "$_shistory_pending_cwd" --command "$command_text" \
        --started-at "$_shistory_started_at" --duration-ms "$duration" --status "$exit_code" 2>/dev/null
    fi
  fi
  _shistory_pending=0
  # Match DEBUG's history representation (command substitution strips newlines).
  _shistory_last_history=$(HISTTIMEFORMAT= builtin history 1)
  builtin history -a
  return "$exit_code"
}

_shistory_prompt_ready() { _shistory_ready=1; }

_shistory_reverse_search() {
  command -v fzf >/dev/null 2>&1 || return 0
  local selected
  IFS= read -r -d '' selected < <(
    command shistory search --cwd "$PWD" --match fuzzy --query "${READLINE_LINE:-}" --format fzf |
      SHISTORY_SEARCH_CWD="$PWD" command fzf --read0 --print0 --no-multi \
        --disabled --no-sort --with-shell='sh -c' --delimiter=$'\t' --with-nth=2.. \
        --query="${READLINE_LINE:-}" --prompt='scoped> ' \
        --bind='change:reload(sleep 0.1; command shistory search --cwd "$SHISTORY_SEARCH_CWD" --match fuzzy --format fzf --query {q})'
  )
  if [[ $selected == *$'\t'* ]]; then
    READLINE_LINE=${selected#*$'\t'}
    READLINE_POINT=${#READLINE_LINE}
  fi
}

# Preserve existing DEBUG code and its incoming status. The extracted string is
# shell-generated quoting from `trap -p`, rather than parsing arbitrary commands.
_shistory_old_debug=$(trap -p DEBUG)
if [[ -n $_shistory_old_debug ]]; then
  _shistory_old_debug=${_shistory_old_debug#trap -- }
  _shistory_old_debug=${_shistory_old_debug% DEBUG}
  eval "_shistory_old_debug=$_shistory_old_debug"
fi
# Saving $? and restoring it before the old trap keeps status-aware traps working.
_shistory_restore_status() { return "$1"; }
_shistory_debug_trap='_shistory_trap_status=$?; _shistory_debug; _shistory_restore_status "$_shistory_trap_status"; eval "${_shistory_old_debug:-:}"'
# Bash 3.2 restores a sourced file's DEBUG trap on return. Install at the top
# level of the first prompt instead, without changing functrace globally.
_shistory_debug_installed=0
_shistory_install_trap='if [[ $_shistory_debug_installed == 0 ]]; then trap "$_shistory_debug_trap" DEBUG; _shistory_debug_installed=1; fi'

# Preserve both array and scalar PROMPT_COMMAND forms, including embedded newlines.
# Bash < 5.1 runs only PROMPT_COMMAND[0], so wrap that element as a scalar.
if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == 'declare -a '* ]]; then
  if (( BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1) )); then
    PROMPT_COMMAND=(_shistory_prompt "${PROMPT_COMMAND[@]}" "$_shistory_install_trap" _shistory_prompt_ready)
  else
    PROMPT_COMMAND[0]="_shistory_prompt${PROMPT_COMMAND[0]:+$'\n'${PROMPT_COMMAND[0]}}"$'\n'"$_shistory_install_trap"$'\n_shistory_prompt_ready'
  fi
else
  PROMPT_COMMAND="_shistory_prompt${PROMPT_COMMAND:+$'\n'$PROMPT_COMMAND}"$'\n'"$_shistory_install_trap"$'\n_shistory_prompt_ready'
fi
if ! type h >/dev/null 2>&1; then function h { command shistory list --cwd "$PWD" "$@"; }; fi
if ! type hs >/dev/null 2>&1; then function hs { command shistory search --cwd "$PWD" "$@"; }; fi
if ! type hg >/dev/null 2>&1; then function hg { command shistory search --global "$@"; }; fi
if [[ ${SHISTORY_BIND_KEYS:-0} == 1 ]] && (( BASH_VERSINFO[0] >= 4 )); then
  bind -x '"\C-r":_shistory_reverse_search'
fi
