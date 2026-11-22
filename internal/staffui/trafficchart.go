package staffui

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
)

// The interface traffic chart — FR-NET-006.
//
// Drawn as inline SVG on the server, not with a charting library: the console
// is server-rendered with one small script, and a graph that needs JavaScript
// to appear at all would be blank for exactly the operator who opens it on a
// phone over a poor link during an outage. Everything is computed here and the
// template only places numbers; the hover readout in console.js is an
// enhancement over a chart that already reads without it.
//
// Two series, In and Out, in the first two categorical slots of the console's
// validated chart palette (blue, orange — checked for colour-vision
// separation in both light and dark). One y-axis in bit/s. The lines are the
// bucket averages; each bucket's peak is in the hover readout and the table,
// because a two-hour average on a 30-day graph flattens a saturated hour.

const (
	chartW      = 760.0
	chartH      = 260.0
	chartLeft   = 64.0 // room for "999 Mbit/s"
	chartRight  = 12.0
	chartTop    = 12.0
	chartBottom = 28.0
)

type chartTick struct {
	Pos   float64
	Label string
}

type chartView struct {
	Width, Height       float64
	PlotLeft, PlotRight float64
	PlotTop, PlotBottom float64
	InPath, OutPath     string
	YTicks, XTicks      []chartTick
	Empty               bool
	PointsJSON          string
	PeakIn, PeakOut     string
	AvgIn, AvgOut       string
	Points              []chartRow
	Range               nastraffic.Range
	Coverage            string
}

// chartRow is one bucket for the table view, which carries every value the
// lines do — identity and values are never colour-only.
type chartRow struct {
	At                             string
	InAvg, OutAvg, InPeak, OutPeak string
}

// buildChart lays out pts over [from, from+r.Span).
func buildChart(pts []nastraffic.Point, r nastraffic.Range, from time.Time, loc *time.Location) chartView {
	v := chartView{
		Width: chartW, Height: chartH,
		PlotLeft: chartLeft, PlotRight: chartW - chartRight,
		PlotTop: chartTop, PlotBottom: chartH - chartBottom,
		Range: r,
	}
	to := from.Add(r.Span)
	v.XTicks = xTicks(from, to, r, v, loc)

	if len(pts) == 0 {
		v.Empty = true
		v.YTicks = yTicks(0, v)
		return v
	}

	var maxAvg, peakIn, peakOut int64
	var sumIn, sumOut float64
	for _, p := range pts {
		maxAvg = max(maxAvg, p.InAvgBps, p.OutAvgBps)
		peakIn = max(peakIn, p.InPeakBps)
		peakOut = max(peakOut, p.OutPeakBps)
		sumIn += float64(p.InAvgBps)
		sumOut += float64(p.OutAvgBps)
	}
	top := niceCeiling(maxAvg)
	v.YTicks = yTicks(top, v)
	v.PeakIn, v.PeakOut = FormatBps(peakIn), FormatBps(peakOut)
	v.AvgIn = FormatBps(int64(sumIn / float64(len(pts))))
	v.AvgOut = FormatBps(int64(sumOut / float64(len(pts))))
	expected := int(r.Span / r.Bucket)
	v.Coverage = fmt.Sprintf("%d of %d intervals measured", len(pts), expected)

	x := func(t time.Time) float64 {
		return v.PlotLeft + (v.PlotRight-v.PlotLeft)*float64(t.Sub(from))/float64(r.Span)
	}
	y := func(bps int64) float64 {
		if top == 0 {
			return v.PlotBottom
		}
		return v.PlotBottom - (v.PlotBottom-v.PlotTop)*float64(bps)/float64(top)
	}
	// A bucket is plotted at its midpoint, and a missing bucket breaks the
	// line: an unmeasured period is drawn as a gap, never bridged, because a
	// straight line across an outage reads as steady traffic through it.
	var in, out strings.Builder
	type hover struct {
		X                           float64
		T, In, Out, InPeak, OutPeak string
	}
	hovers := make([]hover, 0, len(pts))
	for i, p := range pts {
		mid := p.At.Add(r.Bucket / 2)
		cmd := "L"
		if i == 0 || p.At.Sub(pts[i-1].At) > r.Bucket {
			cmd = "M"
		}
		px := x(mid)
		fmt.Fprintf(&in, "%s%.1f %.1f ", cmd, px, y(p.InAvgBps))
		fmt.Fprintf(&out, "%s%.1f %.1f ", cmd, px, y(p.OutAvgBps))
		label := p.At.In(loc).Format("2 Jan 15:04")
		hovers = append(hovers, hover{
			X: math.Round(px*10) / 10, T: label,
			In: FormatBps(p.InAvgBps), Out: FormatBps(p.OutAvgBps),
			InPeak: FormatBps(p.InPeakBps), OutPeak: FormatBps(p.OutPeakBps),
		})
		v.Points = append(v.Points, chartRow{
			At: label, InAvg: FormatBps(p.InAvgBps), OutAvg: FormatBps(p.OutAvgBps),
			InPeak: FormatBps(p.InPeakBps), OutPeak: FormatBps(p.OutPeakBps),
		})
	}
	v.InPath = strings.TrimSpace(in.String())
	v.OutPath = strings.TrimSpace(out.String())
	if b, err := json.Marshal(hovers); err == nil {
		v.PointsJSON = string(b)
	}
	return v
}

// niceCeiling rounds a maximum up to 1, 2, 2.5 or 5 times a power of ten, so
// the axis reads in round numbers. Zero stays zero: an idle link draws flat
// on the baseline rather than being stretched to look busy.
func niceCeiling(v int64) int64 {
	if v <= 0 {
		return 0
	}
	exp := math.Pow(10, math.Floor(math.Log10(float64(v))))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if c := m * exp; c >= float64(v) {
			return int64(c)
		}
	}
	return int64(10 * exp)
}

// yTicks places four gridlines from zero to top. With no data the axis still
// shows its zero line, so an empty chart is visibly a chart with nothing in
// it rather than a broken image.
func yTicks(top int64, v chartView) []chartTick {
	if top == 0 {
		return []chartTick{{Pos: v.PlotBottom, Label: "0"}}
	}
	ticks := make([]chartTick, 0, 5)
	for i := 0; i <= 4; i++ {
		val := top * int64(i) / 4
		pos := v.PlotBottom - (v.PlotBottom-v.PlotTop)*float64(i)/4
		ticks = append(ticks, chartTick{Pos: pos, Label: FormatBps(val)})
	}
	return ticks
}

// xTicks labels the time axis in the operator's time zone: every four hours
// for a day, every day for a week, every five days for a month.
func xTicks(from, to time.Time, r nastraffic.Range, v chartView, loc *time.Location) []chartTick {
	var step time.Duration
	var layout string
	switch r.Key {
	case "24h":
		step, layout = 4*time.Hour, "15:04"
	case "7d":
		step, layout = 24*time.Hour, "Mon 2"
	default:
		step, layout = 5*24*time.Hour, "2 Jan"
	}
	local := from.In(loc)
	// First tick at the next whole step boundary in local time.
	t := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for !t.After(local) {
		t = t.Add(step)
	}
	var ticks []chartTick
	for ; t.Before(to); t = t.Add(step) {
		pos := v.PlotLeft + (v.PlotRight-v.PlotLeft)*float64(t.Sub(from))/float64(r.Span)
		ticks = append(ticks, chartTick{Pos: math.Round(pos*10) / 10, Label: t.Format(layout)})
	}
	return ticks
}

// FormatBps renders a rate in SI bit/s units, as network gear and ISP plans
// state them (decimal: 1 Mbit/s is 1,000,000 bit/s, not 1,048,576).
func FormatBps(bps int64) string {
	f := float64(bps)
	switch {
	case bps <= 0:
		return "0 bit/s"
	case f >= 1e9:
		return trimFloat(f/1e9) + " Gbit/s"
	case f >= 1e6:
		return trimFloat(f/1e6) + " Mbit/s"
	case f >= 1e3:
		return trimFloat(f/1e3) + " kbit/s"
	}
	return strconv.FormatInt(bps, 10) + " bit/s"
}

// trimFloat gives three significant figures without trailing zeros.
func trimFloat(f float64) string {
	prec := 2
	switch {
	case f >= 100:
		prec = 0
	case f >= 10:
		prec = 1
	}
	s := strconv.FormatFloat(f, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
