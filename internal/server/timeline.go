package server

import (
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
}

// newTimeline lays out eps (already sorted oldest-first by
// ListEpisodesForRule) onto a fixed-width view box spanning from the
// earliest episode's start to the later of the last episode's end or now --
// so a still-firing episode's bar reaches the right edge rather than
// implying the window ended when the query did.
func newTimeline(eps []store.Episode, now time.Time) timeline {
	tl := timeline{ViewWidth: timelineViewWidth, ViewHeight: timelineViewHeight}
	if len(eps) == 0 {
		tl.Empty = true
		return tl
	}

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
			X: x, Width: w, Y: y, Firing: e.State == store.StateFiring,
			Title: e.State + " " + e.StartedAt.Format(time.RFC3339) + " (" + formatDuration(e.Duration()) + ")",
		})
	}
	return tl
}
