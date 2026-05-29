package server

import "fmt"

const (
	DefaultNativeLeaseTTLSeconds = defaultIssueLockTTLSeconds

	nativeRunLeaseDefaultJobTimeoutSeconds = 60 * 60
	nativeRunLeaseWorkflowOverheadSeconds  = 30 * 60
	nativeRunLeasePhaseOverheadSeconds     = 5 * 60
	nativeRunLeaseMaxTTLSeconds            = 24 * 60 * 60
)

func nativeRunLeaseTTLSeconds(wf *Workflow) int {
	if wf == nil {
		return DefaultNativeLeaseTTLSeconds
	}
	total := nativeRunLeaseWorkflowOverheadSeconds
	for _, phase := range wf.Phases {
		total += nativePhaseLeaseSeconds(phase) + nativeRunLeasePhaseOverheadSeconds
	}
	if total < DefaultNativeLeaseTTLSeconds {
		return DefaultNativeLeaseTTLSeconds
	}
	if total > nativeRunLeaseMaxTTLSeconds {
		return nativeRunLeaseMaxTTLSeconds
	}
	return total
}

func nativePhaseLeaseSeconds(phase PhaseSpec) int {
	jobs := CanonicalNativePhaseJobs(phase)
	if len(jobs) == 0 {
		return nativeRunLeaseDefaultJobTimeoutSeconds
	}
	phaseSeconds := 0
	for _, job := range jobs {
		jobSeconds := nativeRunLeaseDefaultJobTimeoutSeconds
		if job.TimeoutSeconds != nil && *job.TimeoutSeconds > 0 {
			jobSeconds = *job.TimeoutSeconds
		}
		if jobSeconds > phaseSeconds {
			phaseSeconds = jobSeconds
		}
	}
	return phaseSeconds
}

func nativeLeaseTTLP(value int) *int {
	return &value
}

func nativeLeaseNotClaimedError(lease Lease) error {
	ref := LeasePublicRefFromLease(lease)
	if ref == "" {
		ref = "<unknown>"
	}
	return fmt.Errorf("native lease state is %q for %s, want claimed", lease.State, ref)
}
