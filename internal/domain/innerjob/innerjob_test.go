package innerjob

import (
	"errors"
	"testing"
)

func TestParseHappyPath(t *testing.T) {
	line := Marker + ` {"namespace":"ambience-slot-3","job_name":"agent-ve-2","intent":"verification_agent","label":"verify-agent"}`
	reg, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if reg.Namespace != "ambience-slot-3" || reg.JobName != "agent-ve-2" {
		t.Fatalf("reg=%#v", reg)
	}
	if reg.Intent != IntentVerificationAgent {
		t.Fatalf("intent=%q", reg.Intent)
	}
	if reg.Label != "verify-agent" {
		t.Fatalf("label=%q", reg.Label)
	}
}

func TestParseRejectsNonMarkerLine(t *testing.T) {
	if _, err := Parse(`{"type":"result","total_cost_usd":1.25}`); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("want ErrNoMarker, got %v", err)
	}
	if _, err := Parse("just some log output"); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("want ErrNoMarker, got %v", err)
	}
}

func TestParseRejectsMissingPayload(t *testing.T) {
	if _, err := Parse(Marker); err == nil || errors.Is(err, ErrNoMarker) {
		t.Fatalf("expected payload error, got %v", err)
	}
	if _, err := Parse(Marker + "   "); err == nil || errors.Is(err, ErrNoMarker) {
		t.Fatalf("expected payload error, got %v", err)
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	line := Marker + ` not-json`
	if _, err := Parse(line); err == nil {
		t.Fatal("expected JSON error")
	}
}

func TestParseRejectsMissingRequiredFields(t *testing.T) {
	cases := map[string]string{
		"no namespace": `{"job_name":"agent-x"}`,
		"no job_name":  `{"namespace":"ns"}`,
		"empty fields": `{"namespace":"","job_name":""}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(Marker + " " + payload); err == nil {
				t.Fatalf("expected validation error for %s", payload)
			}
		})
	}
}

func TestParseDefaultsIntentToUnknown(t *testing.T) {
	line := Marker + ` {"namespace":"ns","job_name":"x"}`
	reg, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if reg.Intent != IntentUnknown {
		t.Fatalf("intent=%q, want %q", reg.Intent, IntentUnknown)
	}
}

func TestParseCollapsesUnknownIntentEnum(t *testing.T) {
	// Out-of-enum intents collapse to Unknown — the registration is
	// still useful and the metric label stays bounded.
	line := Marker + ` {"namespace":"ns","job_name":"x","intent":"shenanigans"}`
	reg, err := Parse(line)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if reg.Intent != IntentUnknown {
		t.Fatalf("intent=%q, want %q", reg.Intent, IntentUnknown)
	}
}

func TestMetadataPayload(t *testing.T) {
	reg := Registration{
		Namespace: "ns",
		JobName:   "j",
		Intent:    IntentVerificationAgent,
		Label:     "lab",
		Selector:  "k=v",
	}
	meta := reg.Metadata()
	for _, key := range []string{"namespace", "job_name", "intent", "label", "selector"} {
		if _, ok := meta[key]; !ok {
			t.Fatalf("metadata missing %q: %#v", key, meta)
		}
	}

	reg2 := Registration{Namespace: "ns", JobName: "j", Intent: IntentUnknown}
	meta2 := reg2.Metadata()
	if _, ok := meta2["label"]; ok {
		t.Fatalf("empty label should be omitted: %#v", meta2)
	}
	if _, ok := meta2["selector"]; ok {
		t.Fatalf("empty selector should be omitted: %#v", meta2)
	}
}
