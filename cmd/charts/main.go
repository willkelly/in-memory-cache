// Command charts renders the benchmark results to standalone SVG files for the
// blog post. Pure standard library: the SVGs are built as plain strings. Run:
//
//	go run ./cmd/charts        # writes charts/*.svg
//
// Data is the GOMAXPROCS=1/2/4/8 latency (ns/op) from results/by-impl.txt.
package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

var impls = []string{"mutex", "rwmutex", "syncmap", "sharded", "cow"}
var colors = []string{"#5F5E5A", "#378ADD", "#BA7517", "#1D9E75", "#D85A30"}
var dashes = []string{"", "6,4", "2,3", "", "8,4,2,4"}
var widths = []float64{2, 2, 2, 3, 2}

var cores = []float64{1, 2, 4, 8}

type quad = [4]float64

// ns[dist][mix] holds one quad per implementation (same order as `impls`).
var ns = map[string]map[string][5]quad{
	"uniform": {
		"r100": {{110.5, 148.5, 156.75, 165.75}, {110.5, 63.65, 56.74, 56.77}, {232.95, 113.85, 57.54, 29.30}, {130.05, 67.89, 36.47, 20.22}, {92.42, 44.06, 22.05, 11.15}},
		"r90":  {{112.7, 150.45, 160.05, 169.40}, {115.0, 255.45, 227.00, 247.35}, {270.7, 132.80, 67.75, 35.65}, {131.6, 69.79, 37.78, 21.18}, {16370525, 7644872, 9063532, 11302155}},
		"r50":  {{124.2, 167.25, 171.25, 179.15}, {115.5, 276.20, 234.05, 240.60}, {375.5, 216.95, 103.75, 55.10}, {133.2, 73.47, 40.06, 22.61}, {65239102, 45150270, 46397637, 40316875}},
		"r10":  {{139.7, 184.60, 178.95, 187.40}, {159.0, 225.90, 202.60, 206.50}, {615.7, 304.85, 137.50, 71.45}, {138.0, 76.29, 42.24, 23.91}, {118473027, 90491720, 86301497, 80583242}},
	},
	"zipf": {
		"r100": {{72.94, 79.79, 90.34, 105.85}, {63.44, 59.36, 58.54, 60.69}, {127.05, 62.37, 31.07, 15.465}, {86.36, 48.50, 29.32, 19.315}, {59.65, 28.44, 14.10, 7.101}},
		"r90":  {{74.10, 86.18, 101.70, 115.95}, {64.83, 171.35, 184.30, 207.85}, {148.30, 75.83, 41.02, 22.86}, {86.36, 52.06, 32.58, 22.12}, {16008292, 5852862, 6966330, 8933500}},
		"r50":  {{77.26, 94.90, 118.35, 132.00}, {69.61, 183.45, 181.80, 183.35}, {251.20, 134.10, 76.30, 44.46}, {90.19, 58.73, 38.95, 27.50}, {80624027, 48659440, 45342592, 45152347}},
		"r10":  {{78.91, 97.55, 121.55, 136.85}, {73.89, 118.70, 138.90, 143.30}, {377.85, 188.00, 103.15, 65.91}, {90.14, 60.24, 40.71, 29.69}, {114977782, 90531400, 82555952, 82986020}},
	},
}

var mixTitles = map[string]string{
	"r100": "read-only (r100)", "r90": "read-heavy (r90)",
	"r50": "balanced (r50)", "r10": "write-heavy (r10)",
}

const txt = "#2C2C2A"  // near-black, for light backgrounds
const muted = "#5F5E5A"
const gridc = "rgba(120,120,120,0.18)"

func tput(nsv float64) float64 { return 1000.0 / nsv } // Mops/s

func f(b *strings.Builder, format string, a ...any) { fmt.Fprintf(b, format, a...) }

// linePanel draws a throughput-vs-cores panel at (px,py) sized pw x ph.
func linePanel(b *strings.Builder, px, py, pw, ph float64, title string, data [5]quad, ymax float64) {
	const ml, mr, mt, mb = 50, 16, 26, 34
	plotW := pw - ml - mr
	plotH := ph - mt - mb
	x0, y0 := px+ml, py+mt
	sx := func(i int) float64 { return x0 + float64(i)*plotW/3 }
	sy := func(v float64) float64 { return y0 + plotH*(1-v/ymax) }

	f(b, `<text x="%.1f" y="%.1f" font-size="13" font-weight="500" fill="%s">%s</text>`, px+ml, py+14, txt, title)
	// y gridlines + labels
	for k := 0; k <= 4; k++ {
		v := ymax * float64(k) / 4
		yy := sy(v)
		f(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x0, yy, x0+plotW, yy, gridc)
		f(b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">%.0f</text>`, x0-6, yy+3, muted, v)
	}
	// x labels
	for i := range cores {
		f(b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="middle">%.0f</text>`, sx(i), y0+plotH+16, muted, cores[i])
	}
	f(b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="middle">GOMAXPROCS</text>`, x0+plotW/2, y0+plotH+30, muted)
	f(b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="middle" transform="rotate(-90 %.1f %.1f)">Mops/s</text>`, px+12, y0+plotH/2, muted, px+12, y0+plotH/2)

	// series
	for s := range impls {
		var pts strings.Builder
		for i := 0; i < 4; i++ {
			pts.WriteString(fmt.Sprintf("%.1f,%.1f ", sx(i), sy(tput(data[s][i]))))
		}
		dash := ""
		if dashes[s] != "" {
			dash = fmt.Sprintf(` stroke-dasharray="%s"`, dashes[s])
		}
		f(b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="%.1f"%s/>`, strings.TrimSpace(pts.String()), colors[s], widths[s], dash)
		for i := 0; i < 4; i++ {
			f(b, `<circle cx="%.1f" cy="%.1f" r="2.6" fill="%s"/>`, sx(i), sy(tput(data[s][i])), colors[s])
		}
	}
}

func legend(b *strings.Builder, x, y float64) {
	cx := x
	for s := range impls {
		dash := ""
		if dashes[s] != "" {
			dash = fmt.Sprintf(` stroke-dasharray="%s"`, dashes[s])
		}
		f(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="3"%s/>`, cx, y, cx+22, y, colors[s], dash)
		f(b, `<text x="%.1f" y="%.1f" font-size="12" fill="%s">%s</text>`, cx+27, y+4, txt, impls[s])
		cx += 27 + float64(len(impls[s]))*7.5 + 20
	}
}

func svgHeader(b *strings.Builder, w, h float64, title, desc string) {
	f(b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-family="-apple-system,Segoe UI,Roboto,sans-serif" role="img">`, w, h, w, h)
	f(b, `<title>%s</title><desc>%s</desc>`, title, desc)
}

func writeFile(name, content string) {
	path := filepath.Join("charts", name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", path)
}

// gridChart: 2x2 small multiples of throughput-vs-cores, one per mix.
func gridChart(dist string, ymax float64) {
	const w, h = 760, 540
	var b strings.Builder
	svgHeader(&b, w, h, "Throughput vs cores by mix ("+dist+")",
		"Throughput in millions of ops/sec versus GOMAXPROCS for five cache implementations, "+dist+" key distribution, one panel per read/write mix.")
	f(&b, `<text x="20" y="26" font-size="15" font-weight="500" fill="%s">throughput vs cores by mix &#8212; %s distribution (1M keys)</text>`, txt, dist)
	legend(&b, 20, 48)
	order := []string{"r100", "r90", "r50", "r10"}
	pw, ph := (w-30)/2.0, (h-70)/2.0
	for idx, mix := range order {
		col := float64(idx % 2)
		row := float64(idx / 2)
		px := 10 + col*(pw+10)
		py := 64 + row*(ph+6)
		linePanel(&b, px, py, pw, ph, mixTitles[mix], ns[dist][mix], ymax)
	}
	b.WriteString("</svg>")
	writeFile("throughput_by_mix_"+dist+".svg", b.String())
}

// efficiencyChart: speedup vs 1-core for read-only/uniform, with ideal line.
func efficiencyChart() {
	const w, h = 720, 460
	const ml, mr, mt, mb = 60, 20, 70, 60
	plotW, plotH := float64(w-ml-mr), float64(h-mt-mb)
	x0, y0 := float64(ml), float64(mt)
	ymax := 9.0
	sx := func(i int) float64 { return x0 + float64(i)*plotW/3 }
	sy := func(v float64) float64 { return y0 + plotH*(1-v/ymax) }
	base := ns["uniform"]["r100"]

	var b strings.Builder
	svgHeader(&b, w, h, "Scaling efficiency (read-only, uniform)",
		"Throughput speedup relative to a single core versus GOMAXPROCS, read-only uniform workload, with an ideal linear-scaling reference.")
	f(&b, `<text x="20" y="26" font-size="15" font-weight="500" fill="%s">scaling efficiency &#8212; read-only, uniform</text>`, txt)
	f(&b, `<text x="20" y="44" font-size="12" fill="%s">speedup vs 1 core; dotted line = perfect linear scaling</text>`, muted)
	legend(&b, 20, 60)

	for k := 0; k <= 9; k++ {
		yy := sy(float64(k))
		f(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x0, yy, x0+plotW, yy, gridc)
		f(&b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">%d&#215;</text>`, x0-6, yy+3, muted, k)
	}
	for i := range cores {
		f(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">%.0f</text>`, sx(i), y0+plotH+18, muted, cores[i])
	}
	f(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">GOMAXPROCS</text>`, x0+plotW/2, y0+plotH+38, muted)
	f(&b, `<text x="22" y="%.1f" font-size="11" fill="%s" text-anchor="middle" transform="rotate(-90 22 %.1f)">speedup vs 1 core</text>`, y0+plotH/2, muted, y0+plotH/2)

	// ideal line (1,2,4,8)
	var ideal strings.Builder
	for i := 0; i < 4; i++ {
		ideal.WriteString(fmt.Sprintf("%.1f,%.1f ", sx(i), sy(cores[i])))
	}
	f(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="1.5" stroke-dasharray="2,3"/>`, strings.TrimSpace(ideal.String()), muted)
	f(&b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">ideal 8&#215;</text>`, sx(3)-4, sy(8)-4, muted)

	for s := range impls {
		var pts strings.Builder
		for i := 0; i < 4; i++ {
			pts.WriteString(fmt.Sprintf("%.1f,%.1f ", sx(i), sy(base[s][0]/base[s][i])))
		}
		dash := ""
		if dashes[s] != "" {
			dash = fmt.Sprintf(` stroke-dasharray="%s"`, dashes[s])
		}
		f(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="%.1f"%s/>`, strings.TrimSpace(pts.String()), colors[s], widths[s], dash)
		for i := 0; i < 4; i++ {
			f(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, sx(i), sy(base[s][0]/base[s][i]), colors[s])
		}
	}
	b.WriteString("</svg>")
	writeFile("scaling_efficiency_r100_uniform.svg", b.String())
}

// skewChart: bar chart of skew speedup (uniform/zipf latency) at 8 cores.
func skewChart() {
	const w, h = 720, 440
	const ml, mr, mt, mb = 54, 16, 76, 50
	plotW, plotH := float64(w-ml-mr), float64(h-mt-mb)
	x0, y0 := float64(ml), float64(mt)
	ymax := 2.0
	sy := func(v float64) float64 { return y0 + plotH*(1-v/ymax) }
	mixes := []string{"r100", "r50"}
	barColors := map[string]string{"r100": "#1D9E75", "r50": "#BA7517"}
	mixLabel := map[string]string{"r100": "read-only", "r50": "balanced"}

	var b strings.Builder
	svgHeader(&b, w, h, "Skew speedup at 8 cores",
		"Latency ratio of uniform to Zipfian access at 8 cores per implementation; above 1 means skew helps, below 1 means skew hurts.")
	f(&b, `<text x="20" y="26" font-size="15" font-weight="500" fill="%s">effect of skew &#8212; uniform &#247; zipf latency at 8 cores</text>`, txt)
	f(&b, `<text x="20" y="44" font-size="12" fill="%s">above 1.0&#215; = faster under skew (cache locality); below = slower (hot-key contention)</text>`, muted)
	// legend for the two mixes
	cx := 20.0
	for _, mix := range mixes {
		f(&b, `<rect x="%.1f" y="54" width="11" height="11" rx="2" fill="%s"/>`, cx, barColors[mix])
		f(&b, `<text x="%.1f" y="63" font-size="12" fill="%s">%s</text>`, cx+16, txt, mixLabel[mix])
		cx += 16 + float64(len(mixLabel[mix]))*7.5 + 22
	}

	for k := 0; k <= 4; k++ {
		v := ymax * float64(k) / 4
		yy := sy(v)
		f(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x0, yy, x0+plotW, yy, gridc)
		f(&b, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">%.1f&#215;</text>`, x0-6, yy+3, muted, v)
	}
	// reference line at 1.0
	f(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.5" stroke-dasharray="5,3"/>`, x0, sy(1), x0+plotW, sy(1), "#A32D2D")

	groupW := plotW / float64(len(impls))
	bw := groupW * 0.28
	for s := range impls {
		gx := x0 + float64(s)*groupW + groupW/2
		f(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="%s" text-anchor="middle">%s</text>`, gx, y0+plotH+20, txt, impls[s])
		for mi, mix := range mixes {
			ratio := ns["uniform"][mix][s][3] / ns["zipf"][mix][s][3]
			bx := gx + (float64(mi)-0.5)*(bw+4) - bw/2 + (bw+4)/2
			by := sy(ratio)
			top := math.Min(by, sy(0))
			height := math.Abs(sy(0) - by)
			f(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s"/>`, bx-bw/2, top, bw, height, barColors[mix])
			f(&b, `<text x="%.1f" y="%.1f" font-size="9.5" fill="%s" text-anchor="middle">%.2f</text>`, bx, by-4, muted, ratio)
		}
	}
	b.WriteString("</svg>")
	writeFile("skew_speedup_8cores.svg", b.String())
}

func main() {
	if err := os.MkdirAll("charts", 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gridChart("uniform", 92)
	gridChart("zipf", 145)
	efficiencyChart()
	skewChart()
}
