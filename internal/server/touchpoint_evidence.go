package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type touchpointEvidenceCandidate struct {
	Artifact     EvidenceArtifact
	SourcePhase  string
	AttemptIndex int
}

func touchpointEvidenceForRun(ctx context.Context, artifactStore ArtifactStore, run RunReplayData) ([]TouchpointEvidence, error) {
	required := requiredEvidenceForRun(run)
	requiredCounts := requiredEvidenceCounts(required)
	candidates := evidenceCandidatesForRun(run)
	evidence := make([]TouchpointEvidence, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		artifact := candidate.Artifact
		blobName, ok := artifactBlobNameForEvidence(run, artifact)
		if !ok || seen[blobName] {
			continue
		}
		seen[blobName] = true
		artifact.Kind = firstNonEmpty(NormalizeEvidenceKind(artifact.Kind), EvidenceKindForRef(blobName))
		if artifactStore != nil {
			if err := validateEvidenceArtifact(ctx, artifactStore, artifact.Kind, blobName); err != nil {
				return nil, err
			}
		}
		attemptIndex := candidate.AttemptIndex
		originalRef := firstNonEmpty(artifact.Ref, artifact.ArtifactPath, artifact.URL)
		ref := "blob://artifacts/" + blobName
		if strings.TrimSpace(artifact.Label) == "" {
			artifact.Label = evidenceLabel(originalRef)
		}
		artifact.Ref = ref
		if strings.TrimSpace(artifact.URL) == "" {
			artifact.URL = artifactURLForBlobName(blobName)
		}
		artifact.ArtifactPath = blobName
		artifact.SourcePhase = firstNonEmpty(strings.TrimSpace(artifact.SourcePhase), candidate.SourcePhase)
		if artifact.SourceAttemptIndex == nil {
			artifact.SourceAttemptIndex = &attemptIndex
		}
		evidence = append(evidence, TouchpointEvidence{
			Kind:               artifact.Kind,
			Ref:                artifact.Ref,
			Label:              artifact.Label,
			URL:                artifact.URL,
			ArtifactPath:       artifact.ArtifactPath,
			ContentType:        artifact.ContentType,
			SizeBytes:          artifact.SizeBytes,
			DurationMS:         artifact.DurationMS,
			SourcePhase:        artifact.SourcePhase,
			SourceAttemptIndex: artifact.SourceAttemptIndex,
		})
	}
	if len(requiredCounts) > 0 {
		if artifactStore == nil {
			return nil, ValidationError{Message: "artifact store not configured for required evidence validation"}
		}
		actualCounts := map[string]int{}
		for _, item := range evidence {
			kind := firstNonEmpty(NormalizeEvidenceKind(item.Kind), EvidenceKindForRef(item.Ref))
			actualCounts[kind]++
		}
		for kind, count := range requiredCounts {
			if actualCounts[kind] == 0 {
				return nil, ValidationError{Message: fmt.Sprintf("required %s evidence was not recorded", kind)}
			}
			if actualCounts[kind] < count {
				return nil, ValidationError{Message: fmt.Sprintf("required %d %s evidence artifacts but only %d were recorded", count, kind, actualCounts[kind])}
			}
		}
	}
	return evidence, nil
}

func requiredEvidenceForRun(run RunReplayData) []EvidenceRequirement {
	if len(run.EvidenceRequirements) > 0 {
		return run.EvidenceRequirements
	}
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(run.Attempts[i].PhaseOutputs["test_plan"])
		if raw == "" {
			continue
		}
		payload, ok := decodeEvidenceJSONOutputObject(raw)
		if !ok {
			continue
		}
		return MergeEvidenceRequirements(
			EvidenceRequirementsFromRaw(payload["required_evidence"]),
			EvidenceRequirementsFromRaw(payload["evidence_requirements"]),
		)
	}
	return nil
}

func requiredEvidenceCounts(requirements []EvidenceRequirement) map[string]int {
	counts := map[string]int{}
	for _, requirement := range requirements {
		if requirement.Optional {
			continue
		}
		kind := NormalizeEvidenceKind(requirement.Kind)
		if kind == "" {
			kind = EvidenceKindVideo
		}
		counts[kind]++
	}
	return counts
}

func evidenceCandidatesForRun(run RunReplayData) []touchpointEvidenceCandidate {
	candidates := make([]touchpointEvidenceCandidate, 0)
	for _, attempt := range run.Attempts {
		if attempt.Verification != nil {
			for _, artifact := range attempt.Verification.Evidence {
				candidates = append(candidates, touchpointEvidenceCandidate{
					Artifact:     artifact,
					SourcePhase:  attempt.Phase,
					AttemptIndex: attempt.AttemptIndex,
				})
			}
			for _, ref := range attempt.Verification.EvidenceRefs {
				candidates = append(candidates, touchpointEvidenceCandidate{
					Artifact:     EvidenceArtifact{Ref: ref},
					SourcePhase:  attempt.Phase,
					AttemptIndex: attempt.AttemptIndex,
				})
			}
		}
		raw := strings.TrimSpace(attempt.PhaseOutputs["verification"])
		if raw == "" {
			continue
		}
		for _, artifact := range EvidenceArtifactsFromVerificationOutput(raw) {
			candidates = append(candidates, touchpointEvidenceCandidate{
				Artifact:     artifact,
				SourcePhase:  attempt.Phase,
				AttemptIndex: attempt.AttemptIndex,
			})
		}
	}
	return candidates
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

func artifactBlobNameForEvidence(run RunReplayData, artifact EvidenceArtifact) (string, bool) {
	ref := firstNonEmpty(artifact.ArtifactPath, artifact.Ref, artifact.URL)
	artifactPath, ok := artifactPathFromEvidenceRef(ref)
	if !ok {
		artifactPath = strings.TrimSpace(ref)
		if strings.HasPrefix(artifactPath, "screenshots/") || strings.HasPrefix(artifactPath, "videos/") || strings.HasPrefix(artifactPath, "evidence/") || strings.HasPrefix(artifactPath, "inspections/") {
			if strings.TrimSpace(run.Project) == "" || strings.TrimSpace(run.ID) == "" {
				return "", false
			}
			artifactPath = "runs/" + strings.Trim(strings.TrimSpace(run.Project), "/") + "/" + strings.Trim(strings.TrimSpace(run.ID), "/") + "/" + strings.Trim(artifactPath, "/")
		}
	}
	artifactPath = trimEvidenceURLSuffix(artifactPath)
	if EvidenceKindForRef(artifactPath) == EvidenceKindArtifact && NormalizeEvidenceKind(artifact.Kind) == "" {
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

func validateEvidenceArtifact(ctx context.Context, store ArtifactStore, kind, blobName string) error {
	artifact, err := store.Download(ctx, blobName)
	switch {
	case errors.Is(err, ErrArtifactNotFound):
		return ValidationError{Message: "evidence artifact not found: " + blobName}
	case err != nil:
		return fmt.Errorf("validate evidence artifact %q: %w", blobName, err)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(artifact.ContentType, ";")[0]))
	switch NormalizeEvidenceKind(kind) {
	case EvidenceKindScreenshot:
		if contentType != "" && !strings.HasPrefix(contentType, "image/") {
			return ValidationError{Message: fmt.Sprintf("screenshot artifact %s has non-image content type %q", blobName, artifact.ContentType)}
		}
	case EvidenceKindVideo:
		if contentType != "" && !strings.HasPrefix(contentType, "video/") {
			return ValidationError{Message: fmt.Sprintf("video artifact %s has non-video content type %q", blobName, artifact.ContentType)}
		}
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
