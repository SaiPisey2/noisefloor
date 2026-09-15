#!/usr/bin/env bash
# Records the README demo into .github/assets/demo.gif: a title card and a
# terminal recording per step, then the same results in the browser, cross
# faded into one GIF.
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

width=1197  # matches a 140x36 terminal at the font size agg is given below
height=725
fps=15
# Cross fades are what the file size is made of: every blended frame is a
# full-frame update, so 0.35s here costs 2.4MB against 1MB at 0.15s.
xfade=0.15

for tool in asciinema agg ffmpeg ffprobe; do
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

# The segment list the final GIF is assembled from, in order. Each entry is
# a source file, a y offset to crop from it (screenshots are taller than a
# frame) and how long to hold it; terminal recordings hold themselves, so
# their duration is read back from the file.
srcs=() cropys=() durs=()
segment() { srcs+=("$1"); cropys+=("$2"); durs+=("$3"); }

# --- title cards ------------------------------------------------------
# Rendered as a web page rather than drawn with ffmpeg: this ffmpeg has no
# drawtext filter, and Chrome is already a dependency for the browser half.
card() { # number title subtitle
	local file="$work/shots/card$1.png"
	cat >"$work/card.html" <<HTML
<!doctype html><meta charset="utf-8">
<style>
  html, body { margin: 0; height: 100%; }
  body {
    background: #171b21; color: #e6edf3;
    display: flex; align-items: center; justify-content: center;
    font-family: -apple-system, BlinkMacSystemFont, "Helvetica Neue", sans-serif;
  }
  .card { width: 760px; }
  .n { font-family: Menlo, monospace; font-size: 15px; letter-spacing: .2em;
       color: #6e7681; margin-bottom: 24px; }
  h1 { font-size: 44px; font-weight: 600; letter-spacing: -.01em; margin: 0 0 16px; }
  p  { font-size: 20px; color: #9198a1; margin: 0; }
</style>
<div class="card">
  <div class="n">$1 / 5</div>
  <h1>$2</h1>
  <p>$3</p>
</div>
HTML
	shot "file://$work/card.html" "$height" "$file"
	segment "$file" 0 1.6
}

shot() { # url window-height destination
	rm -f "$3"
	"$chrome" --headless=new --disable-gpu --hide-scrollbars \
		--virtual-time-budget=3000 --screenshot="$3" --window-size="$width,$2" \
		--user-data-dir="$work/chrome/$(basename "$3" .png)" "$1" >/dev/null 2>&1 &
	local pid=$! i
	for ((i = 0; i < 60; i++)); do [ -s "$3" ] && break; sleep 0.5; done
	sleep 1
	kill "$pid" 2>/dev/null || true
	[ -s "$3" ] || { echo "screenshot failed: $1" >&2; exit 1; }
}

# --- terminal steps ---------------------------------------------------
step() { # name hold-seconds
	local name=$1
	local hold=$2
	local gif="$work/seg-$name.gif"
	(
		cd "$work"
		PATH="$root:$PATH" TERM=xterm-256color \
			asciinema rec --quiet --window-size 140x36 --command "bash demo.sh $name" "$name.cast"
	)
	# The pause demo.sh leaves at the end of a step produces no terminal
	# output, so the recording stops at the last line printed and the pause
	# is not in it. --last-frame-duration puts it back, which is also why
	# the hold is stated here rather than read off the recording.
	agg --quiet --theme github-dark --font-size 14 --line-height 1.4 \
		--idle-time-limit 15 --last-frame-duration "$hold" --fps-cap "$fps" \
		"$work/$name.cast" "$gif"
	segment "$gif" 0 "$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$gif")"
}

card 1 "Point it at Prometheus" "No agent, no webhook, no month of waiting."
step init 2
(cd "$work" && bash configure.sh)

card 2 "Score every rule" "Against thirty days of its own firing history."
step scan 6

card 3 "Find what nothing alerts on" "Services no rule names specifically."
step coverage 9

card 4 "Propose the fix" "One pull request per rule. Dry run by default."
step remediate 11
step propose 11

card 5 "The same results in a browser" "Read-only. It renders the last scan."
step serve 3

# --- the browser half -------------------------------------------------
# serve renders the scan and coverage the terminal steps just ran. Chrome
# screenshots it headless, so this stays reproducible rather than being a
# hand-made screen capture.
(cd "$work" && "$root/noisefloor" serve -addr "127.0.0.1:$port") >/dev/null 2>&1 &
serve=$!
disown "$serve" 2>/dev/null || true
trap 'kill "$serve" 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do curl -sf "http://127.0.0.1:$port/" >/dev/null && break; sleep 0.2; done

# DemoFlapping's id is whatever this scan assigned it, so read it back.
rule=$(curl -s "http://127.0.0.1:$port/" |
	grep -o 'href="/rules/[0-9]*">DemoFlapping' | grep -o '[0-9]*' | head -1)
[ -n "$rule" ] || { echo "could not find DemoFlapping in the leaderboard" >&2; exit 1; }

shot "http://127.0.0.1:$port/" "$height" "$work/shots/leaderboard.png"
shot "http://127.0.0.1:$port/rules/$rule" 2000 "$work/shots/rule.png"
shot "http://127.0.0.1:$port/coverage" 1000 "$work/shots/coverage.png"
kill "$serve" 2>/dev/null || true

# The rule page is one tall screenshot held at three offsets: its header,
# its signal breakdown, and the counterfactual the remediate step proposed.
segment "$work/shots/leaderboard.png" 0    3.0
segment "$work/shots/rule.png"        0    3.0
segment "$work/shots/rule.png"        585  3.5
segment "$work/shots/rule.png"        1150 4.5
segment "$work/shots/coverage.png"    150  4.0

# --- one GIF ----------------------------------------------------------
# Every segment is normalised to the same size and frame rate, then chained
# through xfade. Each xfade's offset is where the cross fade starts on the
# stream built so far: the running total minus one fade, because each fade
# overlaps the two segments it joins.
inputs=() graph=""
for i in "${!srcs[@]}"; do
	case "${srcs[i]}" in
	*.png) inputs+=(-loop 1 -t "${durs[i]}" -i "${srcs[i]}") ;;
	*) inputs+=(-i "${srcs[i]}") ;;
	esac
	graph+="[$i:v]fps=$fps,crop=$width:$height:0:${cropys[i]},format=rgb24,setsar=1[s$i];"
done

prev="[s0]" run="${durs[0]}"
for i in "${!srcs[@]}"; do
	[ "$i" = 0 ] && continue
	offset=$(awk -v r="$run" -v x="$xfade" 'BEGIN{printf "%.3f", r - x}')
	graph+="$prev[s$i]xfade=transition=fade:duration=$xfade:offset=$offset[x$i];"
	prev="[x$i]"
	run=$(awk -v r="$run" -v d="${durs[i]}" -v x="$xfade" 'BEGIN{printf "%.3f", r + d - x}')
done
graph+="${prev}split[a][b];[a]palettegen=stats_mode=diff:max_colors=160[p];[b][p]paletteuse=dither=none"

mkdir -p "$(dirname "$out")"
ffmpeg -v error "${inputs[@]}" -filter_complex "$graph" -loop 0 -y "$out"

echo "wrote ${out#"$root"/} ($(du -h "$out" | cut -f1), $(
	ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$out" | cut -d. -f1)s, ${#srcs[@]} segments)"
