# Runs the bash completion function for a command line, as readline would
# call it. usage: bash complete.bash SCRIPT WORD... (the last word is the
# one being completed). Words are split at "=" like readline does with the
# default COMP_WORDBREAKS. Prints each reply as the whole word it completes.
# shellcheck source=/dev/null
source "$1"
shift
COMP_WORDS=()
for w in "$@"; do
    if [[ $w == -*=* ]]; then
        COMP_WORDS+=("${w%%=*}" "=")
        [[ -n ${w#*=} ]] && COMP_WORDS+=("${w#*=}")
    else
        COMP_WORDS+=("$w")
    fi
done
COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
COMP_LINE="$*"
COMP_POINT=${#COMP_LINE}
cur=${!#}
bashcur=${COMP_WORDS[COMP_CWORD]}
[[ $bashcur == "=" ]] && bashcur=
strip=${cur%"$bashcur"}
_hephaestus
for r in "${COMPREPLY[@]}"; do
    printf '%s%s\n' "$strip" "$r"
done
