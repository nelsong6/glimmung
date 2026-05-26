package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

type touchpointScreenshotCandidate struct {
	Ref          string
	SourcePhase  string
	AttemptIndex int
}

func touchpointEvidenceForRun(ctx context.Context, artifactStore ArtifactStore, run RunReplayData) ([]TouchpointEvidence, error) {
	requiredScreenshots := requiredScreenshotEvidenceCount(run)
	candidates := screenshotCandidatesForRun(run)
	evidence := make([]TouchpointEvidence, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		blobName, ok := screenshotArtifactBlobName(run, candidate.Ref)
		if !ok || seen[blobName] {
			continue
		}
		seen[blobName] = true
		if artifactStore != nil {
			if err := validateScreenshotArtifact(ctx, artifactStore, blobName); err != nil {
				return nil, err
			}
		}
		attemptIndex := candidate.AttemptIndex
		evidence = append(evidence, TouchpointEvidence{
			Kind:               "screenshot",
			Ref:                "blob://artifacts/" + blobName,
			Label:              evidenceLabel(candidate.Ref),
			URL:                artifactURLForBlobName(blobName),
			ArtifactPath:       blobName,
			SourcePhase:        candidate.SourcePhase,
			SourceAttemptIndex: &attemptIndex,
		})
	}
	if requiredScreenshots > 0 {
		if artifactStore == nil {
			return nil, ValidationError{Message: "artifact store not configured for required screenshot evidence validation"}
		}
		if len(evidence) == 0 {
			return nil, ValidationError{Message: "visual evidence required but no screenshot artifacts were recorded"}
		}
		if len(evidence) < requiredScreenshots {
			return nil, ValidationError{Message: fmt.Sprintf("visual evidence required %d screenshot artifacts but only %d were recorded", requiredScreenshots, len(evidence))}
		}
	}
	return evidence, nil
}

func requiredScreenshotEvidenceCount(run RunReplayData) int {
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(run.Attempts[i].PhaseOutputs["test_plan"])
		if raw == "" {
			continue
		}
		payload, ok := decodeJSONOutputObject(raw)
		if !ok {
			continue
		}
		return countScreenshotRequirements(payload["required_evidence"])
	}
	return 0
}

func countScreenshotRequirements(raw any) int {
	values, ok := raw.([]any)
	if !ok {
		return 0
	}
	count := 0
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(stringValue(item["kind"])), "screenshot") {
			count++
		}
	}
	return count
}

func screenshotCandidatesForRun(run RunReplayData) []touchpointScreenshotCandidate {
	candidates := make([]touchpointScreenshotCandidate, 0)
	for _, attempt := range run.Attempts {
		raw := strings.TrimSpace(attempt.PhaseOutputs["verification"])
		if raw == "" {
			continue
		}
		for _, ref := range evidenceRefsFromVerificationOutput(raw) {
			candidates = append(candidates, touchpointScreenshotCandidate{
				Ref:          ref,
				SourcePhase:  attempt.Phase,
				AttemptIndex: attempt.AttemptIndex,
			})
		}
	}
	return candidates
}

func evidenceRefsFromVerificationOutput(raw string) []string {
	payload, ok := decodeJSONOutputObject(raw)
	if !ok {
		return nil
	}
	refs := stringSliceFromAny(payload["evidence_refs"])
	if values, ok := payload["evidence_results"].([]any); ok {
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if ref := strings.TrimSpace(stringValue(item["screenshot"])); ref != "" {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

func decodeJSONOutputObject(raw string) (map[string]any, bool) {
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

func stringSliceFromAny(raw any) []string {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s := strings.TrimSpace(stringValue(value))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func screenshotArtifactBlobName(run RunReplayData, ref string) (string, bool) {
	artifactPath, ok := artifactPathFromEvidenceRef(ref)
	if !ok {
		artifactPath = strings.TrimSpace(ref)
		if strings.HasPrefix(artifactPath, "screenshots/") {
			if strings.TrimSpace(run.Project) == "" || strings.TrimSpace(run.ID) == "" {
				return "", false
			}
			artifactPath = "runs/" + strings.Trim(strings.TrimSpace(run.Project), "/") + "/" + strings.Trim(strings.TrimSpace(run.ID), "/") + "/" + strings.Trim(artifactPath, "/")
		}
	}
	artifactPath = trimEvidenceURLSuffix(artifactPath)
	if !isScreenshotPath(artifactPath) {
		return "", false
	}
	return servingArtifactBlobName(artifactPath)
}

func artifactPathFromEvidenceRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "blob://artifacts/"):
		return strings.TrimPrefix(ref, "blob://artifacts/"), true
	case strings.HasPrefix(ref, "/v1/artifacts/"):
		return unescapeArtifactPath(strings.TrimPrefix(ref, "/v1/artifacts/")), true
	case strings.HasPrefix(ref, "runs/"), strings.HasPrefix(ref, "issues/"), strings.HasPrefix(ref, "reports/"):
		return ref, true
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		parsed, err := url.Parse(ref)
		if err != nil {
			return "", false
		}
		idx := strings.Index(parsed.Path, "/v1/artifacts/")
		if idx < 0 {
			return "", false
		}
		return unescapeArtifactPath(parsed.Path[idx+len("/v1/artifacts/"):]), true
	default:
		return "", false
	}
}

func unescapeArtifactPath(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func trimEvidenceURLSuffix(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	return strings.Trim(value, "/")
}

func isScreenshotPath(value string) bool {
	switch strings.ToLower(path.Ext(trimEvidenceURLSuffix(value))) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func validateScreenshotArtifact(ctx context.Context, store ArtifactStore, blobName string) error {
	artifact, err := store.Download(ctx, blobName)
	switch {
	case errors.Is(err, ErrArtifactNotFound):
		return ValidationError{Message: "screenshot artifact not found: " + blobName}
	case err != nil:
		return fmt.Errorf("validate screenshot artifact %q: %w", blobName, err)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(artifact.ContentType, ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return ValidationError{Message: fmt.Sprintf("screenshot artifact %s has non-image content type %q", blobName, artifact.ContentType)}
	}
	return nil
}

func artifactURLForBlobName(blobName string) string {
	parts := strings.Split(strings.Trim(blobName, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/v1/artifacts/" + strings.Join(parts, "/")
}
