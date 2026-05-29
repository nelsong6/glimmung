// PNG OG image renderer.
//
// Why this file exists alongside og_run.go's SVG renderer:
//   Discord rejects SVG og:images outright (its image proxy only
//   rasterises PNG/JPEG/GIF/WEBP). The first round of this feature shipped
//   an SVG og:image and the Discord card came up with title + description
//   but a broken-image placeholder where the graph should be. This
//   renderer produces a PNG of the same card so Discord (and any other
//   strictly-raster unfurler) shows the image preview too.
//
//   The SVG endpoint stays around — it costs nothing, renders crisper
//   on platforms that accept it, and serves as an alternate-format link
//   if anyone wants the vector. The SPA HTML injection points og:image
//   at the PNG variant from this commit forward.
//
// Implementation:
//   - Pure-Go raster via `image/png` from stdlib, with text rendered
//     through `golang.org/x/image/font/opentype` against the Go fonts
//     bundled at `golang.org/x/image/font/gofont/{goregular,gobold}`.
//     No TTF files shipped in the repo and no cgo. The fonts aren't IBM
//     Plex Sans (the design-system head font), but they're a clean
//     humanist sans that holds the design tokens' colors and layout.
//   - Layout: header strip + state pill + workflow headline + phase row
//     + footer, on a 1200x630 canvas.
//   - State colours come from the shared stateColors helper in og_run.go.

package server

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// runOGImagePNG serves the PNG OG image for a run. Public/no-auth so
// unfurlers reach it directly, same posture as the SVG endpoint.
//
// GET /og/runs/{project}/{issue_number}/{run_number}.png
func runOGImagePNG(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runStore, ok := store.(RunStore)
		if !ok || runStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || issueNumber < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		project := r.PathValue("project")
		raw := r.PathValue("run_number")
		// PNG is the only format served. Any other suffix (notably the
		// retired vector format) must 404, not silently fall through to
		// PNG. Per .tank/docs/migration-policy.md the retired surface
		// stays deleted — no alias, no fallback.
		if !strings.HasSuffix(raw, ".png") {
			writeProblem(w, http.StatusNotFound, "og image URL must end with .png")
			return
		}
		runNumber := strings.TrimSuffix(raw, ".png")
		report, err := runStore.GetRunReportByNumber(r.Context(), project, issueNumber, runNumber)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "get run report failed")
			return
		}
		buf, err := renderRunOGPNG(project, issueNumber, runNumber, report)
		if err != nil {
			writeInternalError(w, r, err, "render og png failed")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=30")
		_, _ = w.Write(buf)
	}
}

// renderRunOGPNG draws the same card the SVG renderer produces, but as a
// raster PNG that Discord and other strictly-raster unfurlers accept.
func renderRunOGPNG(project string, issueNumber int, runSlug string, report RunReport) ([]byte, error) {
	const W, H = 1200, 630

	img := image.NewRGBA(image.Rect(0, 0, W, H))
	bg := hexToRGBA("#0e0e10")
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	// Outer border.
	border := hexToRGBA("#2a2a2e")
	drawRectStroke(img, image.Rect(0, 0, W, H), border, 2)

	// Header strip: wordmark + ref line.
	accent := hexToRGBA("#60a5fa")
	muted := hexToRGBA("#888888")
	fg := hexToRGBA("#d4d4d4")

	if err := drawText(img, "glimmung", 60, 84, 30, true, accent); err != nil {
		return nil, err
	}
	headerRef := fmt.Sprintf("%s · #%d · run %s", project, issueNumber, runSlug)
	if err := drawText(img, headerRef, 60, 124, 22, false, muted); err != nil {
		return nil, err
	}

	// State pill (top-right).
	pillText := strings.ToLower(report.State)
	if pillText == "" {
		pillText = "unknown"
	}
	pillFG, pillBG := stateColors(report.State)
	pillW, _ := measureTextWidth(pillText, 18, true)
	pillW += 36
	pillX := W - 60 - pillW
	const pillY = 70
	const pillH = 40
	drawRoundedRect(img, image.Rect(pillX, pillY, pillX+pillW, pillY+pillH), 14, hexToRGBA(pillBG))
	if err := drawText(img, pillText, pillX+18, pillY+27, 18, true, hexToRGBA(pillFG)); err != nil {
		return nil, err
	}

	// Workflow headline.
	headline := report.Workflow
	if headline == "" {
		headline = "run"
	}
	if len(headline) > 60 {
		headline = headline[:57] + "..."
	}
	if err := drawText(img, headline, 60, 208, 46, true, fg); err != nil {
		return nil, err
	}

	// Phase row.
	if err := drawPhaseRowPNG(img, report.PhaseExecutions, 60, 280, W-120, 200); err != nil {
		return nil, err
	}

	// Footer.
	footerParts := []string{}
	if report.AttemptsCount > 0 {
		footerParts = append(footerParts, fmt.Sprintf("%d attempt%s", report.AttemptsCount, plural(report.AttemptsCount)))
	}
	if report.CurrentPhase != nil && *report.CurrentPhase != "" {
		footerParts = append(footerParts, fmt.Sprintf("phase: %s", *report.CurrentPhase))
	}
	if report.CumulativeCostUSD > 0 {
		footerParts = append(footerParts, fmt.Sprintf("$%.2f", report.CumulativeCostUSD))
	}
	if footer := strings.Join(footerParts, "  ·  "); footer != "" {
		if err := drawText(img, footer, 60, H-60, 20, false, muted); err != nil {
			return nil, err
		}
	}

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawPhaseRowPNG mirrors renderPhaseRow's tile layout in raster form.
func drawPhaseRowPNG(img *image.RGBA, phases []RunPhaseExecution, x, y, w, h int) error {
	if len(phases) == 0 {
		return drawText(img, "no phases yet", x, y+h/2, 22, false, hexToRGBA("#666666"))
	}
	const maxTiles = 6
	tiles := phases
	overflow := 0
	if len(tiles) > maxTiles {
		overflow = len(tiles) - (maxTiles - 1)
		tiles = tiles[:maxTiles-1]
	}
	count := len(tiles)
	if overflow > 0 {
		count++
	}
	const gap = 24
	tileW := (w - gap*(count-1)) / count
	if tileW < 120 {
		tileW = 120
	}
	tileH := h
	cy := y + tileH/2

	cursor := x
	for i, ph := range tiles {
		if err := drawPhaseTilePNG(img, cursor, y, tileW, tileH, ph.Name, ph.State, ph.Kind); err != nil {
			return err
		}
		if i < count-1 {
			drawArrowPNG(img, cursor+tileW, cy, cursor+tileW+gap, cy)
		}
		cursor += tileW + gap
	}
	if overflow > 0 {
		if err := drawPhaseTilePNG(img, cursor, y, tileW, tileH, fmt.Sprintf("+%d more", overflow), "info", ""); err != nil {
			return err
		}
	}
	return nil
}

func drawPhaseTilePNG(img *image.RGBA, x, y, w, h int, label, state, kind string) error {
	fg, bg := stateColors(state)
	bgCol := hexToRGBA("#15151a")
	strokeCol := hexToRGBA(bg)
	drawRoundedRect(img, image.Rect(x, y, x+w, y+h), 10, bgCol)
	drawRoundedRectStroke(img, image.Rect(x, y, x+w, y+h), 10, strokeCol, 2)
	// Top state strip.
	drawRoundedRect(img, image.Rect(x, y, x+w, y+6), 10, strokeCol)

	labelText := truncate(label, 18)
	labelW, _ := measureTextWidth(labelText, 22, true)
	if err := drawText(img, labelText, x+(w-labelW)/2, y+h/2-10, 22, true, hexToRGBA("#d4d4d4")); err != nil {
		return err
	}

	st := strings.ToLower(state)
	if st == "" {
		st = "pending"
	}
	stW, _ := measureTextWidth(st, 16, false)
	if err := drawText(img, st, x+(w-stW)/2, y+h/2+22, 16, false, hexToRGBA(fg)); err != nil {
		return err
	}

	if kind != "" && kind != label {
		kindW, _ := measureTextWidth(kind, 13, false)
		if err := drawText(img, kind, x+(w-kindW)/2, y+h-18, 13, false, hexToRGBA("#666666")); err != nil {
			return err
		}
	}
	return nil
}

func drawArrowPNG(img *image.RGBA, x1, y1, x2, y2 int) {
	col := hexToRGBA("#3a3a3e")
	// Line.
	drawHLine(img, x1, x2-8, y2, col, 3)
	// Arrowhead triangle: (x2-8, y2-6), (x2, y2), (x2-8, y2+6).
	fillTriangle(img, image.Point{X: x2 - 8, Y: y2 - 6}, image.Point{X: x2, Y: y2}, image.Point{X: x2 - 8, Y: y2 + 6}, col)
}

// ---------- raster primitives ----------

func hexToRGBA(s string) color.RGBA {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.RGBA{0xff, 0xff, 0xff, 0xff}
	}
	parse := func(off int) uint8 {
		v, err := strconv.ParseUint(s[off:off+2], 16, 8)
		if err != nil {
			return 0
		}
		return uint8(v)
	}
	return color.RGBA{R: parse(0), G: parse(2), B: parse(4), A: 0xff}
}

func drawRectStroke(img *image.RGBA, rect image.Rectangle, col color.RGBA, thickness int) {
	for i := 0; i < thickness; i++ {
		drawHLine(img, rect.Min.X+i, rect.Max.X-1-i, rect.Min.Y+i, col, 1)
		drawHLine(img, rect.Min.X+i, rect.Max.X-1-i, rect.Max.Y-1-i, col, 1)
		drawVLine(img, rect.Min.X+i, rect.Min.Y+i, rect.Max.Y-1-i, col, 1)
		drawVLine(img, rect.Max.X-1-i, rect.Min.Y+i, rect.Max.Y-1-i, col, 1)
	}
}

func drawRoundedRect(img *image.RGBA, rect image.Rectangle, radius int, col color.RGBA) {
	uniform := &image.Uniform{C: col}
	// Centre band.
	draw.Draw(img, image.Rect(rect.Min.X, rect.Min.Y+radius, rect.Max.X, rect.Max.Y-radius), uniform, image.Point{}, draw.Src)
	// Top + bottom bands without the corner squares.
	draw.Draw(img, image.Rect(rect.Min.X+radius, rect.Min.Y, rect.Max.X-radius, rect.Min.Y+radius), uniform, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(rect.Min.X+radius, rect.Max.Y-radius, rect.Max.X-radius, rect.Max.Y), uniform, image.Point{}, draw.Src)
	// Corners — quarter discs.
	fillQuarterDisc(img, rect.Min.X+radius, rect.Min.Y+radius, radius, col, -1, -1)
	fillQuarterDisc(img, rect.Max.X-radius-1, rect.Min.Y+radius, radius, col, +1, -1)
	fillQuarterDisc(img, rect.Min.X+radius, rect.Max.Y-radius-1, radius, col, -1, +1)
	fillQuarterDisc(img, rect.Max.X-radius-1, rect.Max.Y-radius-1, radius, col, +1, +1)
}

func drawRoundedRectStroke(img *image.RGBA, rect image.Rectangle, radius int, col color.RGBA, thickness int) {
	for t := 0; t < thickness; t++ {
		r := image.Rect(rect.Min.X+t, rect.Min.Y+t, rect.Max.X-t, rect.Max.Y-t)
		drawHLine(img, r.Min.X+radius, r.Max.X-radius, r.Min.Y, col, 1)
		drawHLine(img, r.Min.X+radius, r.Max.X-radius, r.Max.Y-1, col, 1)
		drawVLine(img, r.Min.X, r.Min.Y+radius, r.Max.Y-radius, col, 1)
		drawVLine(img, r.Max.X-1, r.Min.Y+radius, r.Max.Y-radius, col, 1)
		strokeQuarterArc(img, r.Min.X+radius, r.Min.Y+radius, radius-t, col, -1, -1)
		strokeQuarterArc(img, r.Max.X-radius-1, r.Min.Y+radius, radius-t, col, +1, -1)
		strokeQuarterArc(img, r.Min.X+radius, r.Max.Y-radius-1, radius-t, col, -1, +1)
		strokeQuarterArc(img, r.Max.X-radius-1, r.Max.Y-radius-1, radius-t, col, +1, +1)
	}
}

func fillQuarterDisc(img *image.RGBA, cx, cy, r int, col color.RGBA, dx, dy int) {
	for y := 0; y <= r; y++ {
		for x := 0; x <= r; x++ {
			if x*x+y*y <= r*r {
				img.SetRGBA(cx+dx*x, cy+dy*y, col)
			}
		}
	}
}

func strokeQuarterArc(img *image.RGBA, cx, cy, r int, col color.RGBA, dx, dy int) {
	if r <= 0 {
		return
	}
	for y := 0; y <= r; y++ {
		for x := 0; x <= r; x++ {
			d2 := x*x + y*y
			if d2 <= r*r && d2 >= (r-1)*(r-1) {
				img.SetRGBA(cx+dx*x, cy+dy*y, col)
			}
		}
	}
}

func drawHLine(img *image.RGBA, x1, x2, y int, col color.RGBA, thickness int) {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	for t := 0; t < thickness; t++ {
		for x := x1; x <= x2; x++ {
			img.SetRGBA(x, y+t-thickness/2, col)
		}
	}
}

func drawVLine(img *image.RGBA, x, y1, y2 int, col color.RGBA, thickness int) {
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	for t := 0; t < thickness; t++ {
		for y := y1; y <= y2; y++ {
			img.SetRGBA(x+t-thickness/2, y, col)
		}
	}
}

func fillTriangle(img *image.RGBA, a, b, c image.Point, col color.RGBA) {
	minX := min3(a.X, b.X, c.X)
	maxX := max3(a.X, b.X, c.X)
	minY := min3(a.Y, b.Y, c.Y)
	maxY := max3(a.Y, b.Y, c.Y)
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			if pointInTriangle(image.Point{X: x, Y: y}, a, b, c) {
				img.SetRGBA(x, y, col)
			}
		}
	}
}

func pointInTriangle(p, a, b, c image.Point) bool {
	d1 := sign(p, a, b)
	d2 := sign(p, b, c)
	d3 := sign(p, c, a)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func sign(p, a, b image.Point) int {
	return (p.X-b.X)*(a.Y-b.Y) - (a.X-b.X)*(p.Y-b.Y)
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

func max3(a, b, c int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// ---------- font loading + text rendering ----------

var (
	fontOnce         sync.Once
	fontRegular      *opentype.Font
	fontBold         *opentype.Font
	fontFaceCache    = map[fontCacheKey]font.Face{}
	fontFaceCacheMu  sync.Mutex
	fontParseErrInit error
)

type fontCacheKey struct {
	size float64
	bold bool
}

func initFonts() error {
	fontOnce.Do(func() {
		regular, err := opentype.Parse(goregular.TTF)
		if err != nil {
			fontParseErrInit = fmt.Errorf("parse goregular: %w", err)
			return
		}
		bold, err := opentype.Parse(gobold.TTF)
		if err != nil {
			fontParseErrInit = fmt.Errorf("parse gobold: %w", err)
			return
		}
		fontRegular = regular
		fontBold = bold
	})
	return fontParseErrInit
}

func face(size float64, bold bool) (font.Face, error) {
	if err := initFonts(); err != nil {
		return nil, err
	}
	key := fontCacheKey{size: size, bold: bold}
	fontFaceCacheMu.Lock()
	defer fontFaceCacheMu.Unlock()
	if f, ok := fontFaceCache[key]; ok {
		return f, nil
	}
	src := fontRegular
	if bold {
		src = fontBold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, err
	}
	fontFaceCache[key] = f
	return f, nil
}

func drawText(img *image.RGBA, text string, x, y int, size float64, bold bool, col color.RGBA) error {
	f, err := face(size, bold)
	if err != nil {
		return err
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: col},
		Face: f,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(text)
	return nil
}

func measureTextWidth(text string, size float64, bold bool) (int, error) {
	f, err := face(size, bold)
	if err != nil {
		return 0, err
	}
	w := font.MeasureString(f, text)
	return w.Ceil(), nil
}
