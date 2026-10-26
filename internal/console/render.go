package console

import (
	"fmt"
	"math"
	"strings"
	"time"

	"netdiag/internal/stats"
	"netdiag/internal/verdict"
)

// SGR colour sequences.
const (
	sgrReset  = "\x1b[0m"
	sgrBold   = "\x1b[1m"
	sgrDim    = "\x1b[2m"
	sgrRed    = "\x1b[31m"
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrGrey   = "\x1b[90m"
	sgrBoldRd = "\x1b[1;31m"
	sgrBoldGn = "\x1b[1;32m"
	sgrBoldYl = "\x1b[1;33m"
)

// seg is a run of text in one style ("" = default).
type seg struct{ text, sgr string }

// line is one output line made of styled runs. Widths count runes: every
// character the renderer uses is one console column wide.
type line []seg

func plain(s string) line { return line{{text: s}} }

func (l line) width() int {
	n := 0
	for _, s := range l {
		n += stringWidth(s.text)
	}
	return n
}

// fit cuts the line to w columns, ending a cut line with "…". A wide rune
// is never split: the column it would not fit in becomes a space.
func (l line) fit(w int) line {
	if l.width() <= w {
		return l
	}
	if w <= 0 {
		return nil
	}
	var out line
	left := w - 1
	for _, s := range l {
		sw := stringWidth(s.text)
		if sw >= left {
			prefix, pw := cutWidth(s.text, left)
			out = append(out, seg{prefix, s.sgr}, seg{"…", s.sgr})
			if pw < left {
				out = append(out, seg{strings.Repeat(" ", left-pw), ""})
			}
			return out
		}
		out = append(out, s)
		left -= sw
	}
	return out
}

// String renders the line, with SGR sequences when color is set.
func (l line) String(color bool) string {
	var b strings.Builder
	for _, s := range l {
		if color && s.sgr != "" {
			b.WriteString(s.sgr + s.text + sgrReset)
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

// trunc shortens s to n columns, ending with "…" when cut (padded with a
// space where a wide rune did not fit whole).
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if stringWidth(s) <= n {
		return s
	}
	prefix, pw := cutWidth(s, n-1)
	return prefix + "…" + strings.Repeat(" ", n-1-pw)
}

// padL left-aligns s in n columns (truncating), padR right-aligns.
func padL(s string, n int) string {
	s = trunc(s, n)
	return s + strings.Repeat(" ", n-stringWidth(s))
}

func padR(s string, n int) string {
	s = trunc(s, n)
	return strings.Repeat(" ", n-stringWidth(s)) + s
}

// ms formats a millisecond value: one decimal below 1 s.
func ms(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// sparkBlocks are the eight sparkline levels.
var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline draws cells scaled to their own min–max (SPEC §9): a flat
// series is the lowest block, lost is a red "×", empty is a space, and
// values above threshold are yellow.
func sparkline(cells []Cell, threshold float64) line {
	lo, hi, have := math.Inf(1), math.Inf(-1), false
	for _, c := range cells {
		if c.Kind == CellOK {
			lo, hi, have = min(lo, c.RTT), max(hi, c.RTT), true
		}
	}
	var out line
	add := func(r rune, sgr string) {
		if n := len(out); n > 0 && out[n-1].sgr == sgr {
			out[n-1].text += string(r)
			return
		}
		out = append(out, seg{string(r), sgr})
	}
	for _, c := range cells {
		switch c.Kind {
		case CellOK:
			lvl := 0
			if have && hi > lo {
				lvl = int(math.Round((c.RTT - lo) / (hi - lo) * float64(len(sparkBlocks)-1)))
			}
			sgr := ""
			if c.RTT > threshold {
				sgr = sgrYellow
			}
			add(sparkBlocks[lvl], sgr)
		case CellLost:
			add('×', sgrRed)
		default:
			add(' ', "")
		}
	}
	return out
}

// Render draws a frame of at most width columns per line.
func Render(v View, width int, loc *time.Location) []line {
	var out []line
	add := func(l line) { out = append(out, l.fit(width)) }

	web := "off"
	if v.WebURL != "" {
		web = v.WebURL
	}
	add(plain(fmt.Sprintf("netdiag %s  %s  Config: %s  Web UI: %s", v.Version, v.Time.In(loc).Format("15:04:05"), v.ConfigSource, web)))
	gw := v.Gateway
	if v.GatewayMAC != "" {
		gw += " (" + v.GatewayMAC + ")"
	}
	add(plain(fmt.Sprintf("Adapter: %s  IP: %s  Gateway: %s  DNS: %s", v.Adapter, v.LocalIP, gw, v.DNS)))
	l3 := "Public IP: " + v.PublicIP
	if v.WiFi != "" {
		l3 += "  Wi-Fi: " + v.WiFi
	}
	add(plain(l3 + "  CPU: " + v.CPU))
	add(verdictLine(v.Result))
	add(qualityLine(v, width))
	add(plain(v.Teams))
	add(nil)
	out = append(out, targetTable(v, width)...)
	if len(v.Hops) > 0 {
		add(nil)
		add(line{{"Hops to " + v.TraceTarget, sgrBold}})
		out = append(out, hopTable(v.Hops, width)...)
	}
	switch {
	case v.Stopping:
		add(nil)
		add(line{{"Stopping…", sgrYellow}})
	case v.Keys:
		add(nil)
		add(line{{"q quit  r reset counters", sgrGrey}})
	}
	return out
}

func verdictLine(r verdict.Result) line {
	sgr := sgrBoldRd
	switch r.Verdict {
	case verdict.OK:
		sgr = sgrBoldGn
	case verdict.Checking:
		sgr = sgrBoldYl
	}
	text := string(r.Verdict)
	if r.Detail != "" {
		text += " (" + r.Detail + ")"
	}
	l := line{{"Verdict: ", ""}, {text, sgr}, {"  Flags: ", ""}}
	if len(r.Flags) == 0 {
		return append(l, seg{"-", ""})
	}
	return append(l, seg{strings.Join(r.Flags, " "), sgrYellow})
}

// qualityLine is the call quality line; its sparkline is added only when
// it fits whole.
func qualityLine(v View, width int) line {
	q := v.Result.Quality
	if !q.HasData {
		return plain("Call quality: " + q.Status)
	}
	sgr := sgrYellow
	switch q.Status {
	case verdict.QualityGood:
		sgr = sgrGreen
	case verdict.QualityPoor:
		sgr = sgrRed
	}
	jitter := "-"
	if q.HasJitter {
		jitter = ms(q.JitterMs) + " ms"
	}
	l := line{{"Call quality: ", ""}, {q.Status, sgr},
		{fmt.Sprintf("  MOS %.2f  RTT %s ms  jitter %s  loss %.1f%%  [%s]", q.MOS, ms(q.RTTms), jitter, q.LossPct, v.QualityLabel), ""}}
	if v.Sparkline && v.QualitySpark != nil && l.width()+2+SparkSeconds <= width {
		l = append(append(l, seg{"  ", ""}), sparkline(v.QualitySpark, v.RTTThreshold)...)
	}
	return l
}

// Table geometry: fixed numeric columns, then Name and Host share the
// rest (SPEC §9: the sparkline is dropped first, then names shrink).
const (
	nameMax, nameMin = 16, 6
	hostMax, hostMin = 40, 10
)

var statCols = []struct {
	title string
	w     int
}{{"Loss%", 6}, {"Snt", 5}, {"Last", 6}, {"Avg", 6}, {"Best", 6}, {"Wrst", 6}, {"StDev", 6}, {"Jitter", 6}}

// statsWidth is the numeric columns with a leading space each.
func statsWidth(cols int) int {
	n := 0
	for _, c := range statCols[:cols] {
		n += 1 + c.w
	}
	return n
}

// columns sizes the name and host columns for a table of fixed width
// fixed, and decides whether the sparkline fits.
func columns(names, hosts []string, fixed, width int, wantSpark bool) (nameW, hostW int, sparkOn bool) {
	nameW, hostW = len("Name"), len("Host")
	for _, n := range names {
		nameW = max(nameW, stringWidth(n))
	}
	for _, h := range hosts {
		hostW = max(hostW, stringWidth(h))
	}
	nameW, hostW = min(nameW, nameMax), min(hostW, hostMax)
	if wantSpark && nameW+1+hostW+fixed+1+SparkSeconds <= width {
		return nameW, hostW, true
	}
	if over := nameW + 1 + hostW + fixed - width; over > 0 {
		cut := min(over, max(hostW-hostMin, 0))
		hostW -= cut
		over -= cut
		nameW -= min(over, max(nameW-nameMin, 0))
	}
	return nameW, hostW, false
}

func targetTable(v View, width int) []line {
	names := make([]string, len(v.Rows))
	hosts := make([]string, len(v.Rows))
	for i, r := range v.Rows {
		names[i], hosts[i] = r.Name, r.Host
	}
	fixed := statsWidth(len(statCols))
	nameW, hostW, sparkOn := columns(names, hosts, fixed, width, v.Sparkline)

	head := padL("Name", nameW) + " " + padL("Host", hostW)
	for _, c := range statCols {
		head += " " + padR(c.title, c.w)
	}
	if sparkOn {
		head += " " + padL(fmt.Sprintf("Last %d s", SparkSeconds), SparkSeconds)
	}
	out := []line{line{{head, sgrBold}}.fit(width)}
	for _, r := range v.Rows {
		out = append(out, targetRow(r, v, nameW, hostW, sparkOn).fit(width))
	}
	return out
}

func targetRow(r Row, v View, nameW, hostW int, sparkOn bool) line {
	lead := padL(r.Name, nameW) + " " + padL(r.Host, hostW)
	s := r.Stats
	rest := statsWidth(len(statCols)) - statsWidth(2)
	if r.Skipped {
		text := lead + statsText(s, 2) + " " + padL("skipped: "+r.Reason, rest-1)
		l := line{{text, sgrGrey}}
		if sparkOn {
			l = append(l, seg{" " + strings.Repeat(" ", SparkSeconds), ""})
		}
		return l
	}
	var text string
	switch {
	case r.IsMTU:
		mtu := "-"
		if r.MTU > 0 {
			mtu = fmt.Sprint(r.MTU)
		}
		text = lead + statsText(s, 2) + " " + padL("path MTU "+mtu, rest-1)
	default:
		text = lead + statsText(s, len(statCols))
	}
	l := line{{text, ""}}
	if sparkOn {
		l = append(l, seg{" ", ""})
		if r.Spark != nil {
			l = append(l, sparkline(r.Spark, v.RTTThreshold)...)
		} else {
			l = append(l, seg{strings.Repeat(" ", SparkSeconds), ""})
		}
	}
	return l
}

// statsText formats the first cols numeric columns, each with a leading
// space.
func statsText(s stats.Summary, cols int) string {
	rtt := func(ok bool, v float64) string {
		if !ok {
			return "-"
		}
		return ms(v)
	}
	vals := []string{
		fmt.Sprintf("%.1f%%", s.LossPct), fmt.Sprint(s.Sent),
		rtt(s.HasRTT, s.Last), rtt(s.HasRTT, s.Avg), rtt(s.HasRTT, s.Best), rtt(s.HasRTT, s.Worst),
		rtt(s.HasRTT, s.StdDev), rtt(s.HasJitter, s.Jitter),
	}
	var b strings.Builder
	for i := range cols {
		b.WriteString(" " + padR(vals[i], statCols[i].w))
	}
	return b.String()
}

// hopCols are the hops table numeric columns: no jitter, as mtr.
const hopCols = 7

func hopTable(hops []HopRow, width int) []line {
	fixed := statsWidth(hopCols)
	hostW := hostMin
	for _, h := range hops {
		hostW = max(hostW, stringWidth(h.Hosts))
	}
	hostW = max(min(hostW, width-4-fixed, 2*hostMax), hostMin)

	head := padR("#", 3) + " " + padL("Host", hostW)
	for _, c := range statCols[:hopCols] {
		head += " " + padR(c.title, c.w)
	}
	out := []line{line{{head, sgrBold}}.fit(width)}
	for _, h := range hops {
		lead := padR(fmt.Sprint(h.TTL), 3) + " " + padL(h.Hosts, hostW)
		loss := " " + padR(fmt.Sprintf("%.1f%%", h.Stats.LossPct), statCols[0].w)
		lossSGR := ""
		if h.DimLoss {
			lossSGR = sgrDim
		}
		l := line{{lead, ""}, {loss, lossSGR}, {statsText(h.Stats, hopCols)[1+statCols[0].w:], ""}}
		out = append(out, l.fit(width))
	}
	return out
}
