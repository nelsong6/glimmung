package server

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

const (
	EvidenceKindArtifact   = "artifact"
	EvidenceKindScreenshot = "screenshot"
	EvidenceKindVideo      = "video"
)

type EvidenceRequirement struct {
	ID              string `json:"id,omitempty"`
	Kind            string `json:"kind"`
	Label           string `json:"label,omitempty"`
	Route           string `json:"route,omitempty"`
	URLPath         string `json:"url_path,omitempty"`
	MustShow        string `json:"must_show,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Optional        bool   `json:"optional,omitempty"`
}

type EvidenceArtifact struct {
	Kind               string `json:"kind"`
	Ref                string `json:"ref"`
	Label              string `json:"label"`
	URL                string `json:"url,omitempty"`
	ArtifactPath       string `json:"artifact_path,omitempty"`
	ContentType        string `json:"content_type,omitempty"`
	SizeBytes          int64  `json:"size_bytes,omitempty"`
	DurationMS         int    `json:"duration_ms,omitempty"`
	SourcePhase        string `json:"source_phase,omitempty"`
	SourceAttemptIndex *int   `json:"source_attempt_index,omitempty"`
}

func NormalizeEvidenceKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "screenshot", "image", "still":
		return EvidenceKindScreenshot
	case "video", "animation", "webm", "movie", "recording":
		return EvidenceKindVideo
	case "":
		return ""
	default:
		return EvidenceKindArtifact
	}
}

func EvidenceKindForRef(ref string) string {
	switch strings.ToLower(path.Ext(trimArtifactEvidenceSuffix(ref))) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return EvidenceKindScreenshot
	case ".webm", ".mp4", ".mov", ".m4v":
		return EvidenceKindVideo
	default:
		return EvidenceKindArtifact
	}
}

func EvidenceRequirementsFromIssueLabels(labels []string) []EvidenceRequirement {
	out := make([]EvidenceRequirement, 0)
	seen := map[string]bool{}
	add := func(req EvidenceRequirement) {
		req.Kind = NormalizeEvidenceKind(req.Kind)
		if req.Kind == "" {
			return
		}
		key := req.Kind + "\x00" + strings.TrimSpace(firstNonEmpty(req.ID, req.Label, req.Route, req.URLPath))
		if key == req.Kind+"\x00" {
			key = req.Kind
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, req)
	}
	for _, label := range labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "evidence:video", "evidence:animation", "video-evidence", "animation-evidence", "requires-video-evidence":
			add(EvidenceRequirement{
				ID:       "issue-label-video",
				Kind:     EvidenceKindVideo,
				Label:    "primary browser flow",
				MustShow: "browser behavior changed by this run",
			})
		case "evidence:screenshot", "screenshot-evidence", "requires-screenshot-evidence":
			add(EvidenceRequirement{
				ID:       "issue-label-screenshot",
				Kind:     EvidenceKindScreenshot,
				Label:    "final browser state",
				MustShow: "final visible state changed by this run",
			})
		}
	}
	return out
}

func EvidenceRequirementsFromRaw(raw any) []EvidenceRequirement {
	if typed, ok := raw.([]EvidenceRequirement); ok {
		return MergeEvidenceRequirements(typed)
	}
	values, ok := anyList(raw)
	if !ok {
		return nil
	}
	out := make([]EvidenceRequirement, 0, len(values))
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		req := EvidenceRequirement{
			ID:              strings.TrimSpace(stringValue(item["id"])),
			Kind:            NormalizeEvidenceKind(stringValue(item["kind"])),
			Label:           strings.TrimSpace(stringValue(item["label"])),
			Route:           strings.TrimSpace(stringValue(item["route"])),
			URLPath:         strings.TrimSpace(stringValue(item["url_path"])),
			MustShow:        strings.TrimSpace(stringValue(item["must_show"])),
			DurationSeconds: positiveIntValue(item["duration_seconds"]),
			Optional:        boolValue(item["optional"]),
		}
		if req.Kind == "" {
			req.Kind = EvidenceKindVideo
		}
		out = append(out, req)
	}
	return out
}

func MergeEvidenceRequirements(groups ...[]EvidenceRequirement) []EvidenceRequirement {
	out := make([]EvidenceRequirement, 0)
	seen := map[string]bool{}
	for _, group := range groups {
		for _, req := range group {
			req.Kind = NormalizeEvidenceKind(req.Kind)
			if req.Kind == "" {
				continue
			}
			key := req.Kind + "\x00" + strings.TrimSpace(firstNonEmpty(req.ID, req.Label, req.Route, req.URLPath))
			if key == req.Kind+"\x00" {
				key = req.Kind
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, req)
		}
	}
	return out
}

func EvidenceArtifactsFromVerificationOutput(raw string) []EvidenceArtifact {
	payload, ok := decodeEvidenceJSONOutputObject(raw)
	if !ok {
		return nil
	}
	return EvidenceArtifactsFromVerificationPayload(payload)
}

func EvidenceArtifactsFromVerificationPayload(payload map[string]any) []EvidenceArtifact {
	out := make([]EvidenceArtifact, 0)
	for _, key := range []string{"evidence", "evidence_artifacts"} {
		if values, ok := anyList(payload[key]); ok {
			for _, value := range values {
				if item, ok := value.(map[string]any); ok {
					out = appendNormalizedEvidenceArtifact(out, evidenceArtifactFromMap(item))
				}
			}
		}
	}
	for _, ref := range stringSliceFromAny(payload["evidence_refs"]) {
		out = appendNormalizedEvidenceArtifact(out, EvidenceArtifact{Ref: ref})
	}
	if values, ok := anyList(payload["evidence_results"]); ok {
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if ref := strings.TrimSpace(stringValue(item["video"])); ref != "" {
				out = appendNormalizedEvidenceArtifact(out, EvidenceArtifact{
					Kind:  EvidenceKindVideo,
					Ref:   ref,
					Label: strings.TrimSpace(stringValue(item["label"])),
				})
			}
			if ref := strings.TrimSpace(stringValue(item["screenshot"])); ref != "" {
				out = appendNormalizedEvidenceArtifact(out, EvidenceArtifact{
					Kind:  EvidenceKindScreenshot,
					Ref:   ref,
					Label: strings.TrimSpace(stringValue(item["label"])),
				})
			}
		}
	}
	return out
}

func EvidenceRefsFromArtifacts(artifacts []EvidenceArtifact) []string {
	out := make([]string, 0, len(artifacts))
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		ref := strings.TrimSpace(firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL))
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func appendMissingStrings(values []string, additions ...string) []string {
	if values == nil {
		values = []string{}
	}
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, addition := range additions {
		addition = strings.TrimSpace(addition)
		if addition == "" || seen[addition] {
			continue
		}
		seen[addition] = true
		values = append(values, addition)
	}
	return values
}

func evidenceArtifactFromMap(item map[string]any) EvidenceArtifact {
	return EvidenceArtifact{
		Kind:         NormalizeEvidenceKind(stringValue(item["kind"])),
		Ref:          strings.TrimSpace(stringValue(item["ref"])),
		Label:        strings.TrimSpace(stringValue(item["label"])),
		URL:          strings.TrimSpace(stringValue(item["url"])),
		ArtifactPath: strings.TrimSpace(stringValue(item["artifact_path"])),
		ContentType:  strings.TrimSpace(stringValue(item["content_type"])),
		SizeBytes:    int64Value(item["size_bytes"]),
		DurationMS:   positiveIntValue(item["duration_ms"]),
	}
}

func appendNormalizedEvidenceArtifact(out []EvidenceArtifact, artifact EvidenceArtifact) []EvidenceArtifact {
	ref := strings.TrimSpace(firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL))
	if ref == "" {
		return out
	}
	artifact.Ref = strings.TrimSpace(artifact.Ref)
	artifact.ArtifactPath = strings.TrimSpace(artifact.ArtifactPath)
	artifact.URL = strings.TrimSpace(artifact.URL)
	if artifact.Kind == "" {
		artifact.Kind = EvidenceKindForRef(ref)
	}
	artifact.Kind = NormalizeEvidenceKind(artifact.Kind)
	if artifact.Kind == "" {
		artifact.Kind = EvidenceKindArtifact
	}
	if strings.TrimSpace(artifact.Label) == "" {
		artifact.Label = evidenceLabel(ref)
	}
	key := artifact.Kind + "\x00" + ref
	for _, existing := range out {
		existingRef := strings.TrimSpace(firstNonEmpty(existing.Ref, existing.ArtifactPath, existing.URL))
		if existing.Kind+"\x00"+existingRef == key {
			return out
		}
	}
	return append(out, artifact)
}

func decodeEvidenceJSONOutputObject(raw string) (map[string]any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err == nil {
		return payload, true
	}
	var encoded string
	if err := json.Unmarshal([]byte(raw), &encoded); err != nil {
		return nil, false
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return nil, false
	}
	return payload, true
}

func anyList(raw any) ([]any, bool) {
	switch values := raw.(type) {
	case []any:
		return values, true
	case []map[string]any:
		out := make([]any, 0, len(values))
		for _, value := range values {
			out = append(out, value)
		}
		return out, true
	case []EvidenceArtifact:
		out := make([]any, 0, len(values))
		for _, value := range values {
			out = append(out, map[string]any{
				"kind":          value.Kind,
				"ref":           value.Ref,
				"label":         value.Label,
				"url":           value.URL,
				"artifact_path": value.ArtifactPath,
				"content_type":  value.ContentType,
				"size_bytes":    value.SizeBytes,
				"duration_ms":   value.DurationMS,
			})
		}
		return out, true
	default:
		return nil, false
	}
}

func boolValue(raw any) bool {
	value, ok := raw.(bool)
	return ok && value
}

func positiveIntValue(raw any) int {
	switch value := raw.(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		n := int(value)
		if n > 0 && value == float64(n) {
			return n
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func int64Value(raw any) int64 {
	switch value := raw.(type) {
	case int:
		if value > 0 {
			return int64(value)
		}
	case int64:
		if value > 0 {
			return value
		}
	case float64:
		n := int64(value)
		if n > 0 && value == float64(n) {
			return n
		}
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func trimArtifactEvidenceSuffix(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	return strings.Trim(value, "/")
}
