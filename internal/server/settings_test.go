package server

import "testing"

// TestSettingsFromEnv_ControlPlaneLoopsEnabled pins the gate that protects
// test slots from running the prod control plane.
//
// Default = true so the prod glimmung deployment keeps running every
// background reconciler when the env var is unset. The k8s/issue chart
// sets CONTROL_PLANE_LOOPS_ENABLED=false on per-issue (test-slot) releases
// so a hot-swapped binary cannot race the prod control plane on shared
// Postgres rows or Kubernetes Jobs.
func TestSettingsFromEnv_ControlPlaneLoopsEnabled(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "default unset", set: false, want: true},
		{name: "false disables", set: true, value: "false", want: false},
		{name: "0 disables", set: true, value: "0", want: false},
		{name: "no disables", set: true, value: "no", want: false},
		{name: "off disables", set: true, value: "off", want: false},
		{name: "true enables", set: true, value: "true", want: true},
		{name: "1 enables", set: true, value: "1", want: true},
		{name: "yes enables", set: true, value: "yes", want: true},
		{name: "garbage falls back to default", set: true, value: "maybe", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("CONTROL_PLANE_LOOPS_ENABLED", tc.value)
			} else {
				// t.Setenv with empty string still sets the variable to
				// "" which envBoolOrDefault treats as unset; that matches
				// what we want here.
				t.Setenv("CONTROL_PLANE_LOOPS_ENABLED", "")
			}
			got := SettingsFromEnv().ControlPlaneLoopsEnabled
			if got != tc.want {
				t.Fatalf("ControlPlaneLoopsEnabled = %v, want %v (value=%q set=%v)", got, tc.want, tc.value, tc.set)
			}
		})
	}
}
