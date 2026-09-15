#!/usr/bin/env bash
# Records the README demo into .github/assets/demo.gif: the CLI tour from
# demo/demo.sh, then the same results in the browser.
#
#   make demo-up && make demo-seed && make demo-gif
#
# Needs asciinema, agg, ffmpeg and Chrome.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="${NF_DEMO_DIR:-${TMPDIR:-/tmp}/noisefloor-demo}"
out="$root/.github/assets/demo.gif"
chrome="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
port=9095

for tool in asciinema agg ffmpeg; do
	command -v "$tool" >/dev/null || { echo "$tool not installed: brew install asciinema agg ffmpeg" >&2; exit 1; }
done
[ -x "$chrome" ] || { echo "Chrome not found; set CHROME=/path/to/chrome" >&2; exit 1; }
curl -sf http://localhost:9090/-/ready >/dev/null || {
	echo "demo Prometheus is not up: make demo-up && make demo-seed" >&2; exit 1; }

CGO_ENABLED=0 go build -o "$root/noisefloor" ./cmd/noisefloor

rm -rf "$work"
mkdir -p "$work/rules" "$work/shots"
cp "$root"/demo/prometheus/rules/*.yml "$work/rules/"
cp "$root/demo/demo.sh" "$work/"

# remediate and propose edit rule files through git, so the checkout they
# are pointed at has to be one.
git -C "$work" init -q
git -C "$work" add -A
git -C "$work" -c user.name=demo -c user.email=demo@example.invalid commit -qm "demo rules"

# demo.sh runs `noisefloor init` on camera and init refuses to overwrite, so
# the config cannot exist yet; this runs straight after it. It turns on the
# two things a default config leaves off: where the rule files live, and
# which group a starter rule for the blind-spot service belongs in.
# Appending a second `rules:` block would be a duplicate mapping key, hence
# the substitution.
cat >"$work/configure.sh" <<'SETUP'
set -e
sed -i.bak 's|^  # path: ./rules$|  path: ./rules|' noisefloor.yaml
rm -f noisefloor.yaml.bak
cat >>noisefloor.yaml <<'YAML'
coverage:
  rule_targets:
    search: {file: rules/coverage.yml, group: services}
YAML
grep -q '^  path: ./rules$' noisefloor.yaml
SETUP

# --- the terminal half -----------------------------------------------
(
	cd "$work"
	PATH="$root:$PATH" TERM=xterm-256color \
		asciinema rec --quiet --window-size 140x36 --command "bash demo.sh" demo.cast
)

# --idle-time-limit is above the longest pause in demo.sh, so the pauses
# that make each step readable survive; the script's sleeps are the pacing.
agg --quiet --theme github-dark --font-size 14 --line-height 1.4 \
	--idle-time-limit 15 --fps-cap 20 "$work/demo.cast" "$work/cli.gif"

# --- the browser half -------------------------------------------------
# serve renders the scan and coverage the terminal half just ran. Chrome
# screenshots it headless, so this stays reproducible rather than being a
# hand-made screen capture. Chrome exits slowly after writing the file, so
# each shot waits for the file and then kills it.
(cd "$work" && "$root/noisefloor" serve -addr "127.0.0.1:$port") >/dev/null 2>&1 &
serve=$!
# bash announces a killed background job on stderr unless it is disowned.
disown "$serve" 2>/dev/null || true
trap 'kill "$serve" 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do curl -sf "http://127.0.0.1:$port/" >/dev/null && break; sleep 0.2; done

shot() { # url window-height destination
	rm -f "$3"
	"$chrome" --headless=new --disable-gpu --hide-scrollbars \
		--virtual-time-budget=3000 --screenshot="$3" --window-size="1197,$2" \
		--user-data-dir="$work/chrome/$(basename "$3" .png)" "$1" >/dev/null 2>&1 &
	local pid=$! i
	for ((i = 0; i < 60; i++)); do [ -s "$3" ] && break; sleep 0.5; done
	sleep 1
	kill "$pid" 2>/dev/null || true
	[ -s "$3" ] || { echo "screenshot failed: $1" >&2; exit 1; }
}

# DemoFlapping's id is whatever this scan assigned it, so read it back.
rule=$(curl -s "http://127.0.0.1:$port/" |
	grep -o 'href="/rules/[0-9]*">DemoFlapping' | grep -o '[0-9]*' | head -1)
[ -n "$rule" ] || { echo "could not find DemoFlapping in the leaderboard" >&2; exit 1; }

shot "http://127.0.0.1:$port/" 725 "$work/shots/leaderboard.png"
shot "http://127.0.0.1:$port/rules/$rule" 2000 "$work/shots/rule.png"
shot "http://127.0.0.1:$port/coverage" 1000 "$work/shots/coverage.png"
kill "$serve" 2>/dev/null || true

# --- one GIF ----------------------------------------------------------
# The three shots become five held frames: the leaderboard, then the rule
# page at its top, its signal breakdown and its counterfactual, then the
# coverage grid. The crops are offsets into the tall screenshots.
mkdir -p "$(dirname "$out")"
ffmpeg -v error \
	-i "$work/cli.gif" \
	-loop 1 -t 3 -i "$work/shots/leaderboard.png" \
	-loop 1 -t 3 -i "$work/shots/rule.png" \
	-loop 1 -t 3 -i "$work/shots/rule.png" \
	-loop 1 -t 4 -i "$work/shots/rule.png" \
	-loop 1 -t 4 -i "$work/shots/coverage.png" \
	-filter_complex "
		[0:v]fps=20,setsar=1[v0];
		[1:v]fps=20,crop=1197:725:0:0,setsar=1[v1];
		[2:v]fps=20,crop=1197:725:0:0,setsar=1[v2];
		[3:v]fps=20,crop=1197:725:0:585,setsar=1[v3];
		[4:v]fps=20,crop=1197:725:0:1150,setsar=1[v4];
		[5:v]fps=20,crop=1197:725:0:150,setsar=1[v5];
		[v0][v1][v2][v3][v4][v5]concat=n=6:v=1:a=0[cat];
		[cat]split[s0][s1];[s0]palettegen=stats_mode=diff[p];
		[s1][p]paletteuse=dither=bayer:bayer_scale=5" \
	-loop 0 -y "$out"

echo "wrote ${out#"$root"/} ($(du -h "$out" | cut -f1), $(
	ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$out" | cut -d. -f1)s)"
