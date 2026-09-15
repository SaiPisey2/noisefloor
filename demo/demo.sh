#!/usr/bin/env bash
# The on-camera half of the README demo. demo/record.sh runs this inside
# asciinema; running it directly just replays the tour in your terminal.
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
sleep 1

# 1. Point it at a Prometheus.
run "noisefloor init"
sleep 2
bash configure.sh

# 2. Score every rule against its own firing history.
clear
run "noisefloor scan"
sleep 7

# 3. Find the services nothing alerts on.
clear
run "noisefloor coverage"
sleep 10

# 4. A pull request for a rule that fires too eagerly.
clear
run "noisefloor remediate | grep -A 24 '^tune:'"
sleep 12

# 5. A starter rule for the blind spot coverage found.
clear
run "noisefloor propose | head -27"
sleep 12

# 6. The same results in a browser. Backgrounded and killed so the
# recording ends; the output is the same as running it in the foreground.
clear
type_cmd "noisefloor serve"
noisefloor serve &
serve=$!
sleep 4
kill "$serve" 2>/dev/null
wait "$serve" 2>/dev/null
sleep 1
