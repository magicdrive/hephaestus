#!/usr/bin/env zsh
# Captures what zsh's completion system offers for a command line.
# usage: zsh -f capture.zsh FPATH_DIR 'command line'
# Starts an interactive zsh in a pseudo-terminal (zsh/zpty), loads the
# completion system (compinit) with FPATH_DIR first on $fpath, types the
# line and a TAB, and prints every candidate passed to compadd as
# "HIT:<inserted word>", then "END".
emulate -L zsh
zmodload zsh/zpty || exit 2
zpty z zsh -f -i
# No echo of what is typed: only the shell's own output is read back. The
# markers are written split ("H""IT:") so that the typed commands never
# contain them.
zpty -w z "stty -echo; PROMPT='' RPROMPT='' PS2=''; unset zle_bracketed_paste"
zpty -w z "fpath=(${(q)1} \$fpath); autoload -Uz compinit; compinit -u -D"
zpty -w z 'zstyle ":completion:*" menu no; zstyle ":completion:*" list-prompt ""; zstyle ":completion:*" select-prompt ""'
zpty -w z 'compadd () { if (( ${@[(I)-[ODA]*]} )); then builtin compadd "$@"; return; fi; local -a __h; builtin compadd -A __h "$@"; local x; for x in $__h; do builtin print -r -- "H""IT:${IPREFIX}${x}"; done; builtin compadd "$@"; }'
zpty -w z '__cap_end () { builtin print -r -- "E""ND"; }'
zpty -w z '__cap () { comppostfuncs=(__cap_end); zle complete-word; }; zle -N __cap; bindkey "^I" __cap'
zpty -w z 'print RE""ADY'
local line
local n=0
while (( n < 400 )); do
  if zpty -r -t z line; then
    line=${line//[$'\r\n']/}
    [[ $line == *READY ]] && break
  else
    sleep 0.05; (( n++ ))
  fi
done
zpty -n -w z "$2"$'\t'
local hits=()
n=0
while (( n < 400 )); do
  if zpty -r -t z line; then
    line=${line//[$'\r\n']/}
    line=${line//$'\e'\[[0-9;?]#[A-Za-z]/}
    if [[ $line == *HIT:* && -n ${line#*HIT:} ]]; then hits+=("${line#*HIT:}"); fi
    [[ $line == *END ]] && break
  else
    sleep 0.05; (( n++ ))
  fi
done
zpty -d z
print -rl -- ${(u)hits}
(( n < 400 )) && print END || print TIMEOUT
