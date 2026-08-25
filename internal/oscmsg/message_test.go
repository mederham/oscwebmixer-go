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
	env := NewStatusEnvelope("degraded")

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
