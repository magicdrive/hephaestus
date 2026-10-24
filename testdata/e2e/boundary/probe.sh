#!/usr/bin/env bash
# Runs as a Hephaestus build command inside the action's local step and
# fails when build code can see a publishing credential:
#   - in its own environment;
#   - on Linux, in the readable environment of any ancestor process up to
#     the runner worker (the step's shell in particular).
# Also checks that Hephaestus itself is not dumpable on Linux, so its
# environment cannot be read through /proc.
# The workflow sets every token to a value containing $marker.
set -euo pipefail
marker="heph-boundary-""sentinel"
leak=0

if env | grep -q "$marker"; then
  echo "probe: a sentinel token is in the build environment"; leak=1
fi
for v in GITHUB_TOKEN GH_TOKEN HOMEBREW_TAP_GITHUB_TOKEN ACTIONS_RUNTIME_TOKEN ACTIONS_ID_TOKEN_REQUEST_TOKEN ACTIONS_ID_TOKEN_REQUEST_URL; do
  if [[ -n "${!v:-}" ]]; then
    echo "probe: $v is set in the build environment"; leak=1
  fi
done

if [[ -r /proc/self/environ ]]; then
  parent_comm="$(cat "/proc/$PPID/comm")"
  if [[ "$parent_comm" != hephaestus* ]]; then
    echo "probe: parent is $parent_comm, expected hephaestus"; leak=1
  elif { tr '\0' '\n' <"/proc/$PPID/environ"; } >/dev/null 2>&1; then
    echo "probe: the environment of hephaestus ($PPID) is readable (process is dumpable)"; leak=1
  else
    echo "probe: hephaestus ($PPID) environment is not readable"
  fi
  pid=$PPID checked=0 worker=0
  while [[ "$pid" -gt 1 ]]; do
    comm="$(cat "/proc/$pid/comm")"
    if [[ "$comm" == Runner.Worker ]]; then
      worker=1
      break
    fi
    if environ="$( { tr '\0' '\n' <"/proc/$pid/environ"; } 2>/dev/null)"; then
      checked=$((checked + 1))
      if grep -q "$marker" <<<"$environ"; then
        echo "probe: a sentinel token is in the environment of ancestor $pid ($comm)"; leak=1
      fi
      if grep -qE '^ACTIONS_ID_TOKEN_REQUEST_(TOKEN|URL)=.' <<<"$environ"; then
        echo "probe: an OIDC request token is in the environment of ancestor $pid ($comm)"; leak=1
      fi
    fi
    # Field 4 of /proc/PID/stat, after the parenthesised command name.
    stat="$(cat "/proc/$pid/stat")"
    read -r _ pid _ <<<"${stat##*) }"
  done
  echo "probe: checked $checked readable ancestor environments below the runner worker"
  if [[ -n "${GITHUB_ACTIONS:-}" && $worker -eq 0 ]]; then
    echo "probe: the ancestor walk did not reach Runner.Worker"; leak=1
  fi
fi

mkdir -p "$(dirname "$HEPHAESTUS_OUTPUT")"
printf 'probe\n' >"$HEPHAESTUS_OUTPUT"
exit "$leak"
