// Run OpenGraph embed surface.
//
// Why this exists:
//   When a glimmung run URL is pasted into Discord (or any unfurling chat
//   client / link-card surface), the unfurler fetches the HTML and scrapes
//   <meta og:*> tags. The dashboard is a React SPA — without server-side
//   meta tags the unfurler sees an empty shell. This file injects per-run
//   tags into the SPA HTML and serves a public image of the run's phase
//   graph so the embed actually carries the same picture a human sees on
//   the page.
//
// What this file owns:
//   - URL matching for the run routes the SPA exposes
//   - SVG renderer that turns a run's phase topology into an embeddable
//     image (no JS runtime, no external libs — pure Go)
//   - HTML <meta> injection into the served index.html
//   - HTTP handlers for the public OG image and the SPA wrapper
//
// Authentication note:
//   Run reads (/v1/projects/{project}/issues/.../runs/.../report) and graph
//   reads are unauthenticated. The OG image endpoint matches that posture
//   so external scrapers without cookies can fetch it.

package server

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// runURLMatch describes a run extracted from a dashboard URL path.
// runSlug is the slug exposed in the SPA route (issue-scoped run number or
// cycle number, what the frontend calls "cycle N").
type runURLMatch struct {
	project     string
	issueNumber int
	runSlug     string
}

// Issue-scoped run URL. Matches the SPA's
// /projects/{project}/issues/{issue_number}/runs/{run_slug}[/cycles/{cycle}...]
// routes — see App.tsx route table.
var runIssueScopedURLPattern = regexp.MustCompile(
	`^/projects/([^/]+)/issues/(\d+)/runs/([^/]+)`,
)

// Project-scoped run URL (no issue), used by ProjectRunRedirectRoute.
// /projects/{project}/runs/{run_id}. We don't enrich these because the URL
// doesn't carry an issue number; the SPA itself redirects to the
// issue-scoped variant.
var runProjectScopedURLPattern = regexp.MustCompile(
	`^/projects/([^/]+)/runs/([^/]+)`,
)

// matchRunURL extracts a run identifier from a dashboard URL path, or
// returns ok=false if the path is not a run URL we know how to enrich.
func matchRunURL(urlPath string) (runURLMatch, bool) {
	if m := runIssueScopedURLPattern.FindStringSubmatch(urlPath); m != nil {
		issueNumber, err := strconv.Atoi(m[2])
		if err != nil {
			return runURLMatch{}, false
		}
		project, errP := url.PathUnescape(m[1])
		if errP != nil {
			return runURLMatch{}, false
		}
		runSlug, errR := url.PathUnescape(m[3])
		if errR != nil {
			return runURLMatch{}, false
		}
		return runURLMatch{
			project:     project,
			issueNumber: issueNumber,
			runSlug:     runSlug,
		}, true
	}
	if runProjectScopedURLPattern.MatchString(urlPath) {
		return runURLMatch{}, false
	}
	return runURLMatch{}, false
}

// runOGImageDispatch picks between the PNG and SVG OG renderers based on
// the suffix of the {run_number} path segment. net/http.ServeMux can't
// place a literal extension after a `{wildcard}`, so the format choice
// has to happen in-handler.
//
// - {run_number} ends in `.png` → PNG (Discord, strictly-raster clients)
// - {run_number} ends in `.svg` → SVG (Slack, browsers, crisper preview)
// - no extension → SVG (preserves the original endpoint shape)
func runOGImageDispatch(store ReadStore) http.HandlerFunc {
	png := runOGImagePNG(store)
	svg := runOGImage(store)
	return func(w http.ResponseWriter, r *http.Request) {
		run := r.PathValue("run_number")
		if strings.HasSuffix(run, ".png") {
			png(w, r)
			return
		}
		svg(w, r)
	}
}

// runOGImage serves the SVG OG image for a run. Public/no-auth so unfurlers
// reach it directly.
func runOGImage(store ReadStore) http.HandlerFunc {
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
		runNumber := strings.TrimSuffix(r.PathValue("run_number"), ".svg")
		report, err := runStore.GetRunReportByNumber(r.Context(), project, issueNumber, runNumber)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "get run report failed")
			return
		}
		svg := renderRunOGSVG(project, issueNumber, runNumber, report)
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		// Short cache: run state changes frequently while a run is live.
		w.Header().Set("Cache-Control", "public, max-age=30")
		_, _ = w.Write(svg)
	}
}

// renderRunOGSVG renders an embedable SVG card for a run. Target dimensions
// match the standard OG image aspect (1200x630) so unfurlers crop predictably.
func renderRunOGSVG(project string, issueNumber int, runSlug string, report RunReport) []byte {
	const width = 1200
	const height = 630
	bg := "#0e0e10"
	fg := "#d4d4d4"
	muted := "#888"
	accent := "#60a5fa"

	stateFG, stateBG := stateColors(report.State)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="glimmung run %s for %s issue %d">`,
		width, height, width, height, xmlEscape(runSlug), xmlEscape(project), issueNumber)
	fmt.Fprintf(&buf, `<rect width="%d" height="%d" fill="%s"/>`, width, height, bg)
	fmt.Fprintf(&buf, `<rect x="0" y="0" width="%d" height="%d" fill="none" stroke="#2a2a2e" stroke-width="2"/>`, width-1, height-1)

	// Header: wordmark + project / issue ref.
	fmt.Fprintf(&buf, `<text x="60" y="84" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="28" font-weight="600" fill="%s">glimmung</text>`, accent)
	headerRef := fmt.Sprintf("%s · #%d · run %s", project, issueNumber, runSlug)
	fmt.Fprintf(&buf, `<text x="60" y="124" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="22" fill="%s">%s</text>`, muted, xmlEscape(headerRef))

	// State pill (top-right).
	pillText := strings.ToLower(report.State)
	if pillText == "" {
		pillText = "unknown"
	}
	pillW := estimatePillWidth(pillText)
	pillX := width - 60 - pillW
	pillY := 70
	fmt.Fprintf(&buf, `<rect x="%d" y="%d" rx="14" ry="14" width="%d" height="40" fill="%s"/>`, pillX, pillY, pillW, stateBG)
	fmt.Fprintf(&buf, `<text x="%d" y="%d" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="18" font-weight="600" fill="%s">%s</text>`, pillX+18, pillY+27, stateFG, xmlEscape(pillText))

	// Headline: workflow name.
	headline := report.Workflow
	if headline == "" {
		headline = "run"
	}
	if len(headline) > 60 {
		headline = headline[:57] + "..."
	}
	fmt.Fprintf(&buf, `<text x="60" y="200" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="46" font-weight="700" fill="%s">%s</text>`, fg, xmlEscape(headline))

	// Phase row: render up to N phases as a horizontal flow.
	const phaseRowY = 280
	const phaseRowH = 200
	renderPhaseRow(&buf, report.PhaseExecutions, 60, phaseRowY, width-120, phaseRowH)

	// Footer: attempts, current phase, cost.
	footerY := height - 60
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
	footer := strings.Join(footerParts, "  ·  ")
	if footer != "" {
		fmt.Fprintf(&buf, `<text x="60" y="%d" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="20" fill="%s">%s</text>`, footerY, muted, xmlEscape(footer))
	}

	buf.WriteString(`</svg>`)
	return buf.Bytes()
}

// renderPhaseRow draws phases as labeled boxes connected by arrows, scaled
// to fit the given bounding box. Truncates with an overflow tile if there
// are more phases than will reasonably fit.
func renderPhaseRow(buf *bytes.Buffer, phases []RunPhaseExecution, x, y, w, h int) {
	if len(phases) == 0 {
		fmt.Fprintf(buf, `<text x="%d" y="%d" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="22" fill="#666">no phases yet</text>`, x, y+h/2)
		return
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
	gap := 24
	tileW := (w - gap*(count-1)) / count
	if tileW < 120 {
		tileW = 120
	}
	tileH := h
	cy := y + tileH/2

	cursor := x
	for i, ph := range tiles {
		drawPhaseTile(buf, cursor, y, tileW, tileH, ph.Name, ph.State, ph.Kind)
		if i < count-1 {
			arrowFromX := cursor + tileW
			arrowToX := arrowFromX + gap
			drawArrow(buf, arrowFromX, cy, arrowToX, cy)
		}
		cursor += tileW + gap
	}
	if overflow > 0 {
		drawPhaseTile(buf, cursor, y, tileW, tileH, fmt.Sprintf("+%d more", overflow), "info", "")
	}
}

func drawPhaseTile(buf *bytes.Buffer, x, y, w, h int, label, state, kind string) {
	fg, bg := stateColors(state)
	fmt.Fprintf(buf, `<rect x="%d" y="%d" rx="10" ry="10" width="%d" height="%d" fill="%s" stroke="%s" stroke-width="2"/>`, x, y, w, h, "#15151a", bg)
	// State strip across the top of the tile.
	fmt.Fprintf(buf, `<rect x="%d" y="%d" rx="10" ry="10" width="%d" height="6" fill="%s"/>`, x, y, w, bg)
	// Label.
	fmt.Fprintf(buf, `<text x="%d" y="%d" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="22" font-weight="600" fill="#d4d4d4" text-anchor="middle">%s</text>`, x+w/2, y+h/2-10, xmlEscape(truncate(label, 18)))
	// State sub-label.
	st := strings.ToLower(state)
	if st == "" {
		st = "pending"
	}
	fmt.Fprintf(buf, `<text x="%d" y="%d" font-family="IBM Plex Sans, system-ui, sans-serif" font-size="16" fill="%s" text-anchor="middle">%s</text>`, x+w/2, y+h/2+22, fg, xmlEscape(st))
	if kind != "" && kind != label {
		fmt.Fprintf(buf, `<text x="%d" y="%d" font-family="IBM Plex Mono, ui-monospace, monospace" font-size="13" fill="#666" text-anchor="middle">%s</text>`, x+w/2, y+h-18, xmlEscape(kind))
	}
}

func drawArrow(buf *bytes.Buffer, x1, y1, x2, y2 int) {
	fmt.Fprintf(buf, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#3a3a3e" stroke-width="3"/>`, x1, y1, x2-8, y2)
	fmt.Fprintf(buf, `<polygon points="%d,%d %d,%d %d,%d" fill="#3a3a3e"/>`, x2-8, y2-6, x2, y2, x2-8, y2+6)
}

// stateColors maps run / phase states to the design-system state pill
// colours. Keep this aligned with frontend/src/App.tsx#runStatePill and
// design-system/colors_and_type.css.
func stateColors(state string) (fg string, bg string) {
	switch strings.ToLower(state) {
	case "completed", "complete", "passed", "succeeded", "free", "ok":
		return "#4ade80", "#14532d"
	case "in_progress", "running", "pending", "queued", "needs_review", "busy":
		return "#fb923c", "#422006"
	case "failed", "aborted", "errored", "drain", "dead":
		return "#f87171", "#3f1c1c"
	case "skipped":
		return "#888", "#2a2a2e"
	default:
		return "#60a5fa", "#1e3a5f"
	}
}

func estimatePillWidth(label string) int {
	// Rough estimate; pill sizing isn't load-bearing for fit. Pad by 36px.
	return 36 + 11*len(label)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// --- SPA HTML meta injection ---

// serveSPAWithOG wraps serveSPA with logic that, for run URLs, reads the
// run report and rewrites the HTML head to include OG/Twitter meta tags
// that describe the run and point to the public SVG image.
//
// On any failure to enrich (run lookup error, parse failure, etc.) the
// handler falls back to serving the plain SPA HTML. Run URLs are common
// and the SPA must still work for humans even when the embed enrichment
// path can't run.
func serveSPAWithOG(settings Settings, store ReadStore) http.HandlerFunc {
	plain := serveSPA(settings)
	return func(w http.ResponseWriter, r *http.Request) {
		match, ok := matchRunURL(r.URL.Path)
		if !ok {
			plain(w, r)
			return
		}
		runStore, ok := store.(RunStore)
		if !ok || runStore == nil {
			plain(w, r)
			return
		}
		indexPath, ok := staticFile(staticRoots(settings), "index.html")
		if !ok {
			plain(w, r)
			return
		}
		raw, err := os.ReadFile(indexPath)
		if err != nil {
			plain(w, r)
			return
		}
		report, err := runStore.GetRunReportByNumber(r.Context(), match.project, match.issueNumber, match.runSlug)
		if err != nil {
			// Unknown run — let the SPA render its own 404 surface.
			plain(w, r)
			return
		}
		tags := buildRunOGTags(r, match, report)
		patched := injectOGTags(raw, tags)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(patched)
	}
}

// buildRunOGTags produces the og:* / twitter:* meta tag block for a run.
//
// og:image points at the PNG variant. Discord (and a few other unfurlers)
// only rasterise PNG/JPEG/GIF/WEBP and silently drop SVG — pasting a run
// URL there with og:image=...svg produced the title+description card with
// a broken-image placeholder where the graph should be. PNG is universally
// accepted.
func buildRunOGTags(r *http.Request, match runURLMatch, report RunReport) string {
	base := externalBaseURL(r)
	pageURL := base + r.URL.Path
	imageURL := base + path.Join(
		"/og/runs",
		url.PathEscape(match.project),
		strconv.Itoa(match.issueNumber),
		url.PathEscape(match.runSlug)+".png",
	)

	workflow := report.Workflow
	if workflow == "" {
		workflow = "run"
	}
	title := fmt.Sprintf("%s · %s #%d · run %s", workflow, match.project, match.issueNumber, match.runSlug)

	descParts := []string{
		fmt.Sprintf("state: %s", orUnknown(report.State)),
	}
	if report.CurrentPhase != nil && *report.CurrentPhase != "" {
		descParts = append(descParts, fmt.Sprintf("phase: %s", *report.CurrentPhase))
	}
	if report.AttemptsCount > 0 {
		descParts = append(descParts, fmt.Sprintf("%d attempt%s", report.AttemptsCount, plural(report.AttemptsCount)))
	}
	if report.CumulativeCostUSD > 0 {
		descParts = append(descParts, fmt.Sprintf("$%.2f", report.CumulativeCostUSD))
	}
	desc := strings.Join(descParts, " · ")

	var b strings.Builder
	addMeta := func(prop, val string) {
		b.WriteString(`    <meta property="`)
		b.WriteString(html.EscapeString(prop))
		b.WriteString(`" content="`)
		b.WriteString(html.EscapeString(val))
		b.WriteString("\">\n")
	}
	addName := func(name, val string) {
		b.WriteString(`    <meta name="`)
		b.WriteString(html.EscapeString(name))
		b.WriteString(`" content="`)
		b.WriteString(html.EscapeString(val))
		b.WriteString("\">\n")
	}

	addMeta("og:type", "article")
	addMeta("og:site_name", "glimmung")
	addMeta("og:title", title)
	addMeta("og:description", desc)
	addMeta("og:url", pageURL)
	addMeta("og:image", imageURL)
	addMeta("og:image:type", "image/png")
	addMeta("og:image:width", "1200")
	addMeta("og:image:height", "630")
	addName("twitter:card", "summary_large_image")
	addName("twitter:title", title)
	addName("twitter:description", desc)
	addName("twitter:image", imageURL)
	addName("description", desc)

	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// injectOGTags rewrites an HTML document so the head includes the given
// meta tag block. If the document already has a <title>, the tags are
// inserted right after it; otherwise after <head>. If no <head>, returns
// the input unchanged.
func injectOGTags(htmlDoc []byte, tags string) []byte {
	lower := bytes.ToLower(htmlDoc)
	insertAt := -1
	if idx := bytes.Index(lower, []byte("</title>")); idx >= 0 {
		insertAt = idx + len("</title>")
	} else if idx := bytes.Index(lower, []byte("<head>")); idx >= 0 {
		insertAt = idx + len("<head>")
	} else {
		return htmlDoc
	}
	var buf bytes.Buffer
	buf.Grow(len(htmlDoc) + len(tags) + 2)
	buf.Write(htmlDoc[:insertAt])
	buf.WriteByte('\n')
	buf.WriteString(tags)
	buf.Write(htmlDoc[insertAt:])
	return buf.Bytes()
}

// externalBaseURL reconstructs the scheme+host the caller used to reach
// glimmung. Honors X-Forwarded-Proto / X-Forwarded-Host because the
// dashboard sits behind an ingress that terminates TLS.
func externalBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}
