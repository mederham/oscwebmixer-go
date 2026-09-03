package oscmsg

import (
	"encoding/json"
	"testing"
)

func TestOSCEnvelopeRoundTrip(t *testing.T) {
	env := NewOSCEnvelope(Message{
		Address: "/Input_Channels/1/Aux_Send/1/send_level",
		Args:    []any{-6.5},
	})

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Kind != KindOSC {
		t.Fatalf("kind = %q, want %q", got.Kind, KindOSC)
	}
	if got.Status != nil {
		t.Fatalf("status = %+v, want nil", got.Status)
	}
	if got.Message == nil || got.Message.Address != env.Message.Address {
		t.Fatalf("message = %+v, want address %q", got.Message, env.Message.Address)
	}
}

func TestStatusEnvelopeRoundTrip(t *testing.T) {
	env := NewStatusEnvelope("degraded", false)

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Kind != KindStatus {
		t.Fatalf("kind = %q, want %q", got.Kind, KindStatus)
	}
	if got.Message != nil {
		t.Fatalf("message = %+v, want nil", got.Message)
	}
	if got.Status == nil || got.Status.Desk != "degraded" {
		t.Fatalf("status = %+v, want desk %q", got.Status, "degraded")
	}
}

// TestConfigPayloadMixerLayoutsRoundTrip guards the wire shape itself:
// MixerLayouts must marshal as a JSON object keyed by layout id (not an
// array), each value carrying both name and items, and AuxConfig must
// carry layoutId — this is what mixer.js's onMessage/admin's queryConfig
// both parse.
func TestConfigPayloadMixerLayoutsRoundTrip(t *testing.T) {
	cfg := ConfigPayload{
		MixerLayouts: map[string]NamedLayout{
			DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
			"mon-1":         {Name: "Monitor Mix", Items: []LayoutItem{{Kind: LayoutKindSection, SectionID: "s1", SectionName: "Drums"}}},
		},
		Auxes:    []AuxConfig{{Channel: 1, LayoutID: "mon-1"}},
		Snapshot: "Show A",
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("marshaled output is not valid JSON: %s", data)
	}

	var got ConfigPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 entries", got.MixerLayouts)
	}
	if got.MixerLayouts["mon-1"].Name != "Monitor Mix" || len(got.MixerLayouts["mon-1"].Items) != 1 {
		t.Fatalf("mon-1 = %+v, want Name=Monitor Mix, 1 item", got.MixerLayouts["mon-1"])
	}
	if len(got.Auxes) != 1 || got.Auxes[0].LayoutID != "mon-1" {
		t.Fatalf("auxes = %+v, want aux 1 layoutId=mon-1", got.Auxes)
	}
}
