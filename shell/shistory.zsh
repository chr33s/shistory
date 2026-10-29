# Source from an interactive Zsh. Native history and keybindings remain available.
[[ -o interactive ]] || return 0
(( ${+_SHISTORY_ZSH_LOADED} )) && return 0
if ! (( $+commands[shistory] )); then
  print -u2 'shistory: add the shistory binary to PATH before loading the plugin'
  return 1
fi
typeset -g _SHISTORY_ZSH_LOADED=1
autoload -Uz add-zsh-hook
zmodload zsh/datetime
typeset -g SHISTORY_SESSION_ID="$(command shistory session)"
typeset -gi SHISTORY_SEQ=0
typeset -g SHISTORY_SCOPE='' _shistory_pending_cmd='' _shistory_pending_cwd=''
typeset -gi _shistory_pending=0 _shistory_started_at=0
typeset -g _shistory_nav_query='' _shistory_nav_buffer='' _shistory_nav_scope=''
typeset -gi _shistory_nav_id=0

_shistory_refresh_scope() {
  SHISTORY_SCOPE="$(command shistory scope --cwd "$PWD" 2>/dev/null)" || SHISTORY_SCOPE=''
  _shistory_nav_id=0
}

_shistory_preexec() {
  _shistory_pending_cmd="$1"
  _shistory_pending_cwd="$PWD"
  _shistory_started_at=$(( EPOCHREALTIME * 1000 ))
  _shistory_pending=1
}

_shistory_precmd() {
  local exit_code=$?
  if (( _shistory_pending )); then
    local -i duration=$(( EPOCHREALTIME * 1000 - _shistory_started_at ))
    (( duration < 0 )) && duration=0
    (( ++SHISTORY_SEQ ))
    command shistory record --session "$SHISTORY_SESSION_ID" --seq "$SHISTORY_SEQ" \
      --shell zsh --cwd "$_shistory_pending_cwd" --command "$_shistory_pending_cmd" \
      --started-at "$_shistory_started_at" --duration-ms "$duration" --status "$exit_code" \
      --tty "${TTY:-}" 2>/dev/null
    _shistory_pending=0
  fi
  return 0
}

_zsh_autosuggest_strategy_shistory() {
  local result
  suggestion=''
  [[ -n "$SHISTORY_SCOPE" ]] || return 0
  # A sentinel preserves trailing newlines through command substitution.
  result="$(command shistory suggest --scope "$SHISTORY_SCOPE" --prefix "$1"; printf '.')" 2>/dev/null
  result="${result%.}"
  suggestion="${result%$'\n'}"
}

_shistory_navigate() {
  local direction="$1" result
  [[ -n "$SHISTORY_SCOPE" ]] || return 0
  if [[ "$SHISTORY_SCOPE" != "$_shistory_nav_scope" || "$BUFFER" != "$_shistory_nav_buffer" ]]; then
    _shistory_nav_query="$BUFFER"
    _shistory_nav_id=0
    _shistory_nav_scope="$SHISTORY_SCOPE"
  fi
  local -a cursor
  if (( _shistory_nav_id )); then
    if [[ "$direction" == older ]]; then cursor=(--before-id "$_shistory_nav_id")
    else cursor=(--after-id "$_shistory_nav_id"); fi
  elif [[ "$direction" == newer ]]; then
    return 0
  fi
  result="$(command shistory navigate --scope "$SHISTORY_SCOPE" --query "$_shistory_nav_query" \
    --direction "$direction" "${cursor[@]}" 2>/dev/null; printf '.')"
  result="${result%.}"
  if [[ "$result" == *$'\t'* ]]; then
    _shistory_nav_id="${result%%$'\t'*}"
    BUFFER="${result#*$'\t'}"
  elif [[ "$direction" == newer ]]; then
    BUFFER="$_shistory_nav_query"
    _shistory_nav_id=0
  fi
  CURSOR=${#BUFFER}
  _shistory_nav_buffer="$BUFFER"
}
_shistory_history_up() { _shistory_navigate older; }
_shistory_history_down() { _shistory_navigate newer; }

_shistory_reverse_search() {
  (( $+commands[fzf] )) || { zle -M 'shistory: install fzf to use scoped reverse search'; return 0; }
  [[ -n "$SHISTORY_SCOPE" ]] || return 0
  local selected
  zle -I
  selected="$(
    command shistory search --scope "$SHISTORY_SCOPE" --match fuzzy --query "$BUFFER" --format fzf |
      SHISTORY_SEARCH_SCOPE="$SHISTORY_SCOPE" command fzf --read0 --no-print0 --no-multi \
        --disabled --no-sort --with-shell='sh -c' --delimiter=$'\t' --with-nth=2.. \
        --query="$BUFFER" --prompt='scoped> ' \
        --bind='change:reload(sleep 0.1; command shistory search --scope "$SHISTORY_SEARCH_SCOPE" --match fuzzy --format fzf --query {q})'
    printf '.'
  )"
  selected="${selected%.}"
  selected="${selected%$'\n'}"
  if [[ "$selected" == *$'\t'* ]]; then BUFFER="${selected#*$'\t'}"; CURSOR=${#BUFFER}; fi
  zle redisplay
}

zle -N shistory-history-up _shistory_history_up
zle -N shistory-history-down _shistory_history_down
zle -N shistory-reverse-search _shistory_reverse_search
add-zsh-hook preexec _shistory_preexec
add-zsh-hook precmd _shistory_precmd
add-zsh-hook chpwd _shistory_refresh_scope
_shistory_refresh_scope

# Names are exposed for opt-in zsh-autosuggestions integration.
typeset -ga ZSH_AUTOSUGGEST_CLEAR_WIDGETS
for _shistory_widget in shistory-history-up shistory-history-down; do
  if (( ! ${ZSH_AUTOSUGGEST_CLEAR_WIDGETS[(Ie)$_shistory_widget]} )); then
    ZSH_AUTOSUGGEST_CLEAR_WIDGETS+=("$_shistory_widget")
  fi
done
unset _shistory_widget
(( $+functions[h] || $+aliases[h] )) || function h { command shistory list --cwd "$PWD" "$@"; }
(( $+functions[hs] || $+aliases[hs] )) || function hs { command shistory search --cwd "$PWD" "$@"; }
(( $+functions[hg] || $+aliases[hg] )) || function hg { command shistory search --global "$@"; }

# Bind only on request. Users can also bind the widgets themselves after loading.
if [[ "${SHISTORY_BIND_KEYS:-0}" == 1 ]]; then
  bindkey '^R' shistory-reverse-search
  [[ -n "${terminfo[kcuu1]:-}" ]] && bindkey "${terminfo[kcuu1]}" shistory-history-up
  [[ -n "${terminfo[kcud1]:-}" ]] && bindkey "${terminfo[kcud1]}" shistory-history-down
fi
