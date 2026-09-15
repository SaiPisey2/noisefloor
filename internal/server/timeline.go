package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// timelineViewWidth and timelineViewHeight are the SVG viewBox dimensions
// the rule-detail template draws episode bars into. The viewBox is scaled
// to whatever width CSS gives the element, so these are just the
// coordinate space the bars below are computed in, not pixels on screen.
const (
	timelineViewWidth  = 1000.0
	timelineViewHeight = 60.0
	timelineFiringY    = 8.0
	timelinePendingY   = 34.0
	timelineBarHeight  = 18.0
	// timelineMinWidth keeps an episode visible even when it is far shorter
	// than the window it is plotted against -- exactly the case a flapping
	// rule's thousands of few-minute episodes over a 30-day window produce.
	// A cluster of many such bars packed together reads, correctly, as a
	// dense band: that texture IS what flapping looks like on this axis,
	// which a percentage on its own cannot show.
	timelineMinWidth = 0.6
)

// episodeBar is one rendered rect in the episode timeline SVG. Fields are
// plain numbers and short enum-like strings; nothing here is copied from
// rule/label/annotation free text, unlike the tooltip title.
type episodeBar struct {
	X, Width float64
	Y        float64
	Firing   bool
	Title    string // rendered as an SVG <title> child; html/template escapes it
	// Opacity is fill-opacity for this bar, an SVG presentation attribute
	// (not a CSS style, so it costs nothing under the strict
	// Content-Security-Policy this server sends -- see server.go). 1 for an
	// exact, one-bar-per-episode rendering; in bucketed mode it scales with
	// how many episodes the bucket represents, so a dense run of flapping
	// still reads as more solid than a quiet one.
	Opacity float64
}

// timeline is the rule-detail page's episode-timeline view model: the
// window it covers and the bars to draw inside it.
type timeline struct {
	Bars        []episodeBar
	WindowStart time.Time
	WindowEnd   time.Time
	ViewWidth   float64
	ViewHeight  float64
	Empty       bool

	// Bucketed is true when Bars are EpisodeTimelineSummary's fixed-width
	// aggregates rather than one bar per episode -- this rule has more
	// episodes than MaxTimelineEpisodes, so drawing one rectangle each
	// would be both unreadable (bars sub-pixel wide) and, unbounded, a
	// per-request memory cost that scales with a flapping rule's entire
	// history. The template states this explicitly rather than rendering
	// a plausible-looking picture that silently dropped most of the data.
	Bucketed      bool
	TotalEpisodes int
	BucketCount   int
}

// newTimeline lays out eps onto a fixed-width view box spanning from the
// earliest episode's start to the later of the last episode's end or now --
// so a still-firing episode's bar reaches the right edge rather than
// implying the window ended when the query did.
//
// eps is sorted oldest-first here rather than assumed to arrive that way:
// store.ListEpisodesForRule orders most-recent-first (what pagination
// wants), so this is the one place that order is turned back into the
// chronological draw order a reviewer scanning left-to-right expects.
func newTimeline(eps []store.Episode, now time.Time) timeline {
	tl := timeline{ViewWidth: timelineViewWidth, ViewHeight: timelineViewHeight}
	if len(eps) == 0 {
		tl.Empty = true
		return tl
	}
	eps = append([]store.Episode(nil), eps...)
	sort.Slice(eps, func(i, j int) bool { return eps[i].StartedAt.Before(eps[j].StartedAt) })

	tl.WindowStart = eps[0].StartedAt
	tl.WindowEnd = eps[0].EndedAt
	for _, e := range eps {
		if e.StartedAt.Before(tl.WindowStart) {
			tl.WindowStart = e.StartedAt
		}
		if e.EndedAt.After(tl.WindowEnd) {
			tl.WindowEnd = e.EndedAt
		}
	}
	if now.After(tl.WindowEnd) {
		tl.WindowEnd = now
	}

	span := tl.WindowEnd.Sub(tl.WindowStart)
	if span <= 0 {
		span = time.Second
	}
	scale := timelineViewWidth / span.Seconds()

	for _, e := range eps {
		x := e.StartedAt.Sub(tl.WindowStart).Seconds() * scale
		w := e.Duration().Seconds() * scale
		if w < timelineMinWidth {
			w = timelineMinWidth
		}
		y := timelineFiringY
		if e.State != store.StateFiring {
			y = timelinePendingY
		}
		tl.Bars = append(tl.Bars, episodeBar{
			X: x, Width: w, Y: y, Firing: e.State == store.StateFiring, Opacity: 1,
			Title: e.State + " " + e.StartedAt.Format(time.RFC3339) + " (" + formatDuration(e.Duration()) + ")",
		})
	}
	return tl
}

// newBucketedTimeline lays out store.EpisodeTimelineSummary's fixed-width
// buckets onto the same view box newTimeline uses, one bar per non-empty
// lane per bucket instead of one bar per episode. total is the exact
// episode count EpisodeTimelineSummary counted (not the number of buckets
// or bars), stated on the page so this reads as a summary, not a picture
// that happens to look complete.
func newBucketedTimeline(buckets []store.EpisodeBucket, bucketWidth time.Duration, total int, windowStart, windowEnd, now time.Time) timeline {
	tl := timeline{
		ViewWidth: timelineViewWidth, ViewHeight: timelineViewHeight,
		Bucketed: true, TotalEpisodes: total, BucketCount: len(buckets),
	}
	if len(buckets) == 0 {
		tl.Empty = true
		return tl
	}
	tl.WindowStart, tl.WindowEnd = windowStart, windowEnd
	if now.After(tl.WindowEnd) {
		tl.WindowEnd = now
	}

	span := tl.WindowEnd.Sub(tl.WindowStart)
	if span <= 0 {
		span = time.Second
	}
	scale := timelineViewWidth / span.Seconds()

	// maxCount anchors the opacity scale: the busiest bucket (in either
	// lane) renders fully solid, and every other bucket's opacity is
	// relative to it -- so the picture shows WHERE episodes concentrate,
	// not just that they exist somewhere in every bucket.
	maxCount := 1
	for _, b := range buckets {
		if b.Firing > maxCount {
			maxCount = b.Firing
		}
		if b.Pending > maxCount {
			maxCount = b.Pending
		}
	}

	for _, b := range buckets {
		bucketStart := tl.WindowStart.Add(time.Duration(b.Index) * bucketWidth)
		bucketEnd := bucketStart.Add(bucketWidth)
		x := bucketStart.Sub(tl.WindowStart).Seconds() * scale
		w := bucketWidth.Seconds() * scale
		if w < timelineMinWidth {
			w = timelineMinWidth
		}
		if b.Firing > 0 {
			tl.Bars = append(tl.Bars, episodeBar{
				X: x, Width: w, Y: timelineFiringY, Firing: true,
				Opacity: bucketOpacity(b.Firing, maxCount),
				Title: fmt.Sprintf("%d firing episode(s), %s to %s",
					b.Firing, bucketStart.Format(time.RFC3339), bucketEnd.Format(time.RFC3339)),
			})
		}
		if b.Pending > 0 {
			tl.Bars = append(tl.Bars, episodeBar{
				X: x, Width: w, Y: timelinePendingY, Firing: false,
				Opacity: bucketOpacity(b.Pending, maxCount),
				Title: fmt.Sprintf("%d pending episode(s), %s to %s",
					b.Pending, bucketStart.Format(time.RFC3339), bucketEnd.Format(time.RFC3339)),
			})
		}
	}
	return tl
}

// bucketOpacity maps a bucket's episode count against the busiest bucket
// to a fill-opacity, floored at 0.25 so even a lightly-populated bucket
// stays visible rather than fading to nothing.
func bucketOpacity(count, maxCount int) float64 {
	if maxCount <= 0 {
		return 1
	}
	o := 0.25 + 0.75*float64(count)/float64(maxCount)
	if o > 1 {
		o = 1
	}
	return o
}
