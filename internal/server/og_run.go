// Run OpenGraph embed surface.
//
// Why this exists:
//   When a glimmung run URL is pasted into Discord (or any unfurling chat
//   client / link-card surface), the unfurler fetches the HTML and scrapes
//   <meta og:*> tags. The dashboard is a React SPA — without server-side
//   meta tags the unfurler sees an empty shell. This file injects per-run
//   tags into the SPA HTML; the PNG renderer in og_run_png.go serves the
//   image those tags point at.
//
// What this file owns:
//   - URL matching for the run routes the SPA exposes
//   - HTML <meta> injection into the served index.html
//   - SPA wrapper that decides when to enrich the response
//   - Shared raster helpers used by the PNG renderer (state palette,
//     small-formatter utilities)
//
// Authentication note:
//   Run reads (/v1/projects/{project}/issues/.../runs/.../report) and graph
//   reads are unauthenticated. The OG image endpoint matches that posture
//   so external scrapers without cookies can fetch it.
//
// Single renderer:
//   The OG image is PNG only. An earlier iteration shipped a vector
//   renderer alongside; Discord (and a few other unfurlers) silently drop
//   vector og:images, so that format never reached the named target. Per
//   .tank/docs/migration-policy.md, the retired vector path is deleted
//   end to end — no live route, no dispatcher branch, no test, no
//   documented format. The CI guard in scripts/check-removed-og-svg.mjs
//   enforces the absence at every push.

package server

import (
	"bytes"
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

// --- shared raster helpers used by og_run_png.go ---

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

// --- SPA HTML meta injection ---

// serveSPAWithOG wraps serveSPA with logic that, for run URLs, reads the
// run report and rewrites the HTML head to include OG/Twitter meta tags
// that describe the run and point to the public PNG image.
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
// og:image points at the PNG image surface served by runOGImagePNG.
// Discord (and a few other unfurlers) only rasterise PNG/JPEG/GIF/WEBP
// and silently drop SVG, which is why PNG is the only format we serve.
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
