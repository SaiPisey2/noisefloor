#!/usr/bin/env bash
# One step of the README demo. Each step is recorded on its own so that
# demo/record.sh can put a title card between them; `bash demo.sh scan`
# also just runs the step in your own terminal.
#
# Every command here is real and its output is whatever the demo stack
# actually returns -- nothing is pre-baked. The pipes exist only to keep
# each step on one screen.
set -u

prompt='$ '

# Types a command out character by character, so the recording looks like
# someone using it rather than a wall of text appearing at once.
type_cmd() {
	printf '%s' "$prompt"
	local i
	for ((i = 0; i < ${#1}; i++)); do
		printf '%s' "${1:i:1}"
		sleep 0.03
	done
	printf '\n'
}

run() {
	type_cmd "$1"
	eval "$1"
}

clear
sleep 0.6

case "${1:?usage: demo.sh <step>}" in
init)
	run "noisefloor init"
	sleep 2
	;;
scan)
	run "noisefloor scan"
	sleep 6
	;;
coverage)
	run "noisefloor coverage"
	sleep 9
	;;
remediate)
	run "noisefloor remediate | grep -A 24 '^tune:'"
	sleep 11
	;;
propose)
	run "noisefloor propose | head -27"
	sleep 11
	;;
serve)
	# Backgrounded and killed so the recording ends; the output is the
	# same as running it in the foreground.
	type_cmd "noisefloor serve"
	noisefloor serve &
	serve=$!
	sleep 3
	kill "$serve" 2>/dev/null
	wait "$serve" 2>/dev/null
	sleep 0.5
	;;
*)
	echo "unknown step: $1" >&2
	exit 2
	;;
esac
