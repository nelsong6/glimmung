package server

import "strings"

const (
	EvidenceGateJobID    = "evidence-verification-gate"
	EvidenceGateStepSlug = "evaluate-verdict"
)

const evidenceGateRunScript = `set -Eeuo pipefail
raw="${GLIMMUNG_INPUT_VERIFICATION:-}"
if [ -z "${raw}" ]; then
  echo "verification input is empty" >&2
  exit 2
fi
status="$(printf '%s' "${raw}" | jq -er '.status // empty')" || {
  echo "verification input is not valid JSON or missing status" >&2
  exit 2
}
printf "verification.status = '%s'\n" "${status}"
printf '%s' "${raw}" | jq -r '.reasons[]? | "reason: \(.)"'
if [ "${status}" = "pass" ]; then
  exit 0
fi
exit 1
`

// CanonicalNativePhase returns the runtime phase shape Glimmung actually
// launches. Evidence gates are a Glimmung-owned primitive, so any project-
// supplied container details are replaced with the managed gate runner while
// preserving a stable job id when one was already registered.
func CanonicalNativePhase(phase PhaseSpec) PhaseSpec {
	if !phase.EvidenceVerificationGate {
		return phase
	}
	phase.Jobs = []NativeJobSpec{canonicalEvidenceGateJob(phase)}
	return phase
}

func CanonicalNativePhaseJobs(phase PhaseSpec) []NativeJobSpec {
	return CanonicalNativePhase(phase).Jobs
}

func canonicalEvidenceGateJob(phase PhaseSpec) NativeJobSpec {
	jobID := EvidenceGateJobID
	name := "Evidence verification gate"
	timeout := 60
	if len(phase.Jobs) > 0 {
		existing := phase.Jobs[0]
		if id := strings.TrimSpace(existing.ID); id != "" {
			jobID = id
		}
		if existing.Name != nil && strings.TrimSpace(*existing.Name) != "" {
			name = strings.TrimSpace(*existing.Name)
		}
		if existing.TimeoutSeconds != nil && *existing.TimeoutSeconds > 0 {
			timeout = *existing.TimeoutSeconds
		}
	}
	title := "Evaluate verification verdict"
	return NativeJobSpec{
		ID:             jobID,
		Name:           &name,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []NativeStepSpec{{
			Slug:  EvidenceGateStepSlug,
			Title: &title,
			Type:  "run",
			Run:   evidenceGateRunScript,
			Shell: "bash",
		}},
	}
}
