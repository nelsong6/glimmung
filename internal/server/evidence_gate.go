package server

import "strings"

const (
	EvidenceGateJobID    = "evidence-verification-gate"
	EvidenceGateStepSlug = "evaluate-verdict"
	PRTouchpointJobID    = "pr-touchpoint"
	PRTouchpointStepSlug = "ensure-pr-touchpoint"

	JobPrimitivePRTouchpoint = "pr_touchpoint"
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

const prTouchpointRunScript = `set -Eeuo pipefail
if [ -z "${GLIMMUNG_PR_TOUCHPOINT_URL:-}" ]; then
  echo "GLIMMUNG_PR_TOUCHPOINT_URL is not configured" >&2
  exit 2
fi
echo "Ensuring PR touchpoint for ${GLIMMUNG_RUN_REF:-unknown run}"
response="$(mktemp)"
status="$(curl -sS -o "${response}" -w '%{http_code}' -X POST "${GLIMMUNG_PR_TOUCHPOINT_URL}")" || {
  code="$?"
  echo "PR touchpoint request failed with curl exit ${code}" >&2
  exit "${code}"
}
cat "${response}" | jq .
if [ "${status}" -lt 200 ] || [ "${status}" -ge 300 ]; then
  echo "PR touchpoint request returned HTTP ${status}" >&2
  exit 1
fi
result_status="$(jq -r '.status // empty' "${response}")"
if [ "${result_status}" = "skipped" ]; then
  echo "PR touchpoint skipped: $(jq -r '.reason // "no reason"' "${response}")"
  exit 0
fi
if [ "${result_status}" != "ensured" ]; then
  echo "PR touchpoint returned unexpected status '${result_status}'" >&2
  exit 2
fi
pr_number="$(jq -r '.pr_number // empty' "${response}")"
touchpoint_ref="$(jq -r '.touchpoint_ref // empty' "${response}")"
html_url="$(jq -r '.html_url // empty' "${response}")"
{
  if [ -n "${pr_number}" ]; then printf 'pr_number=%s\n' "${pr_number}"; fi
  if [ -n "${touchpoint_ref}" ]; then printf 'touchpoint_ref=%s\n' "${touchpoint_ref}"; fi
  if [ -n "${html_url}" ]; then printf 'pr_url=%s\n' "${html_url}"; fi
} >>"${GLIMMUNG_OUTPUT_FILE}"
echo "PR touchpoint ensured: ${touchpoint_ref:-unknown}"
if [ -n "${html_url}" ]; then
  echo "PR URL: ${html_url}"
fi
`

func CanonicalWorkflow(wf Workflow) Workflow {
	for i := range wf.Phases {
		wf.Phases[i] = CanonicalNativePhase(wf.Phases[i])
	}
	return wf
}

// CanonicalNativePhase returns the runtime phase shape Glimmung actually
// launches. Evidence gates are a Glimmung-owned primitive, so any project-
// supplied container details are replaced with the managed gate runner while
// preserving a stable job id when one was already registered.
func CanonicalNativePhase(phase PhaseSpec) PhaseSpec {
	if phase.EvidenceVerificationGate {
		phase.Jobs = []NativeJobSpec{canonicalEvidenceGateJob(phase)}
		return phase
	}
	for i := range phase.Jobs {
		phase.Jobs[i] = CanonicalNativeJob(phase.Jobs[i])
	}
	return phase
}

func CanonicalNativePhaseJobs(phase PhaseSpec) []NativeJobSpec {
	return CanonicalNativePhase(phase).Jobs
}

func CanonicalNativeJob(job NativeJobSpec) NativeJobSpec {
	switch strings.TrimSpace(job.Primitive) {
	case JobPrimitivePRTouchpoint:
		return canonicalPRTouchpointJob(&job)
	default:
		return job
	}
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

func canonicalPRTouchpointJob(existing *NativeJobSpec) NativeJobSpec {
	jobID := PRTouchpointJobID
	name := "PR touchpoint"
	timeout := 120
	if existing != nil {
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
	title := "Ensure PR touchpoint"
	return NativeJobSpec{
		ID:             jobID,
		Name:           &name,
		Primitive:      JobPrimitivePRTouchpoint,
		Managed:        true,
		TimeoutSeconds: &timeout,
		Steps: []NativeStepSpec{{
			Slug:  PRTouchpointStepSlug,
			Title: &title,
			Type:  "run",
			Run:   prTouchpointRunScript,
			Shell: "bash",
		}},
	}
}
