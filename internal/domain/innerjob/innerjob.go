// Package innerjob owns the parser + value type for the inner-Job
// registration marker emitted by phase scripts. See
// docs/inner-job-observation.md for the contract this package
// implements.
//
// Single-line marker shape:
//
//	===GLIMMUNG-INNER-JOB=== {"namespace":"…","job_name":"…", ...}
//
// The runner's log streamer calls Parse on every stdout/stderr line.
// The hot path stays cheap: a HasPrefix check before any allocation.
package innerjob

import (
	"encoding/json"
	"errors"
	"strings"
)

// Marker is the prefix every inner-job registration line starts with.
// Long and distinctive so a real log payload cannot match by accident.
const Marker = "===GLIMMUNG-INNER-JOB==="

// Intent is the closed enum for what the inner Job is doing. Bounded
// at compile time so the metric label cardinality stays small. Unknown
// is the sentinel for "the phase script did not declare an intent."
type Intent string

const (
	IntentVerificationAgent Intent = "verification_agent"
	IntentHelper            Intent = "helper"
	IntentTooling           Intent = "tooling"
	IntentUnknown           Intent = "unknown"
)

// Registration is the parsed payload. Mirrored on the wire as the
// metadata of an `inner_job_registered` native event.
type Registration struct {
	Namespace string `json:"namespace"`
	JobName   string `json:"job_name"`
	Intent    Intent `json:"intent,omitempty"`
	Label     string `json:"label,omitempty"`
	Selector  string `json:"selector,omitempty"`
}

// ErrNoMarker means the line does not start with the registration
// marker. Callers in the hot path should treat this as "not for me,
// continue" rather than an error.
var ErrNoMarker = errors.New("line does not start with inner-job marker")

// Parse extracts a Registration from one log line. Returns ErrNoMarker
// when the line is not a registration marker at all; returns a
// validation error when the line is malformed (the script meant to
// register but the payload is wrong, which is operator-actionable).
func Parse(line string) (Registration, error) {
	if !strings.HasPrefix(line, Marker) {
		return Registration{}, ErrNoMarker
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, Marker))
	if payload == "" {
		return Registration{}, errors.New("inner-job marker has no payload")
	}
	var reg Registration
	if err := json.Unmarshal([]byte(payload), &reg); err != nil {
		return Registration{}, errAtMarker("invalid JSON payload", err)
	}
	if err := reg.normalize(); err != nil {
		return Registration{}, err
	}
	return reg, nil
}

func (r *Registration) normalize() error {
	r.Namespace = strings.TrimSpace(r.Namespace)
	r.JobName = strings.TrimSpace(r.JobName)
	r.Label = strings.TrimSpace(r.Label)
	r.Selector = strings.TrimSpace(r.Selector)
	if r.Namespace == "" {
		return errors.New("inner-job marker missing required field namespace")
	}
	if r.JobName == "" {
		return errors.New("inner-job marker missing required field job_name")
	}
	switch Intent(strings.TrimSpace(string(r.Intent))) {
	case "":
		r.Intent = IntentUnknown
	case IntentVerificationAgent, IntentHelper, IntentTooling, IntentUnknown:
		// closed enum membership
	default:
		// Out-of-enum intents collapse to Unknown rather than fail —
		// the registration itself is more useful than rejecting it,
		// and the metric still surfaces "unknown" as a flag the phase
		// script should fix.
		r.Intent = IntentUnknown
	}
	return nil
}

// Metadata renders the registration as the metadata payload of an
// `inner_job_registered` native event. The runner sends this verbatim
// to glimmung's event endpoint.
func (r Registration) Metadata() map[string]any {
	out := map[string]any{
		"namespace": r.Namespace,
		"job_name":  r.JobName,
		"intent":    string(r.Intent),
	}
	if r.Label != "" {
		out["label"] = r.Label
	}
	if r.Selector != "" {
		out["selector"] = r.Selector
	}
	return out
}

func errAtMarker(prefix string, err error) error {
	if err == nil {
		return errors.New(prefix)
	}
	return errors.New(prefix + ": " + err.Error())
}
