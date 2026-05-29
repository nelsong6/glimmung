package server

import "testing"

func ptrInt(value int) *int {
	return &value
}

func TestNativeRunLeaseTTLSecondsUsesFourHourFloor(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{{
		Name: "quick",
		Jobs: []NativeJobSpec{{ID: "quick", TimeoutSeconds: ptrInt(60)}},
	}}}

	if got := nativeRunLeaseTTLSeconds(wf); got != DefaultNativeLeaseTTLSeconds {
		t.Fatalf("ttl=%d, want floor %d", got, DefaultNativeLeaseTTLSeconds)
	}
}

func TestNativeRunLeaseTTLSecondsSumsPhaseTimeoutsWithOverhead(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{
		{
			Name: "parallel",
			Jobs: []NativeJobSpec{
				{ID: "short", TimeoutSeconds: ptrInt(600)},
				{ID: "long", TimeoutSeconds: ptrInt(7200)},
			},
		},
		{
			Name: "verify",
			Jobs: []NativeJobSpec{{ID: "verify", TimeoutSeconds: ptrInt(5000)}},
		},
		{
			Name: "merge",
			Kind: "touchpoint_gate",
			Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}},
		},
	}}

	want := nativeRunLeaseWorkflowOverheadSeconds +
		7200 + nativeRunLeasePhaseOverheadSeconds +
		5000 + nativeRunLeasePhaseOverheadSeconds +
		120 + nativeRunLeasePhaseOverheadSeconds
	if got := nativeRunLeaseTTLSeconds(wf); got != want {
		t.Fatalf("ttl=%d, want %d", got, want)
	}
}

func TestNativeRunLeaseTTLSecondsCapsOpenEndedWorkflows(t *testing.T) {
	var phases []PhaseSpec
	for i := 0; i < 30; i++ {
		phases = append(phases, PhaseSpec{
			Name: "phase",
			Jobs: []NativeJobSpec{{ID: "job"}},
		})
	}

	if got := nativeRunLeaseTTLSeconds(&Workflow{Phases: phases}); got != nativeRunLeaseMaxTTLSeconds {
		t.Fatalf("ttl=%d, want cap %d", got, nativeRunLeaseMaxTTLSeconds)
	}
}
