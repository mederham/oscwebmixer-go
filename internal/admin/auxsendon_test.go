package admin

import (
	"reflect"
	"testing"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func TestBuildAuxSendOnMessagesFiltersDisabledAuxesAndSources(t *testing.T) {
	cfg := oscmsg.ConfigPayload{
		Auxes: []oscmsg.AuxConfig{
			{Channel: 1, Enabled: true},
			{Channel: 2, Enabled: false},
		},
		MixerLayouts: map[string]oscmsg.NamedLayout{
			oscmsg.DefaultLayoutID: {Items: []oscmsg.LayoutItem{
				{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
				{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 2, Enabled: false},
				{Kind: oscmsg.LayoutKindSource, Type: "group", Number: 3, Enabled: true},
				{Kind: oscmsg.LayoutKindSection, SectionName: "Drums"},
			}},
		},
	}

	got := buildAuxSendOnMessages(cfg)
	want := []oscmsg.Message{
		{Address: "/Input_Channels/1/Aux_Send/1/send_on", Args: []any{float32(1)}},
		{Address: "/Group_Outputs/3/Aux_Send/1/send_on", Args: []any{float32(1)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildAuxSendOnMessages() = %+v, want %+v (disabled aux 2 and disabled channel 2 excluded)", got, want)
	}
}

func TestBuildAuxSendOnMessagesUsesAuxLayoutID(t *testing.T) {
	cfg := oscmsg.ConfigPayload{
		Auxes: []oscmsg.AuxConfig{
			{Channel: 1, Enabled: true, LayoutID: "monitor-1"},
		},
		MixerLayouts: map[string]oscmsg.NamedLayout{
			oscmsg.DefaultLayoutID: {Items: []oscmsg.LayoutItem{
				{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
			}},
			"monitor-1": {Items: []oscmsg.LayoutItem{
				{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 5, Enabled: true},
			}},
		},
	}

	got := buildAuxSendOnMessages(cfg)
	want := []oscmsg.Message{
		{Address: "/Input_Channels/5/Aux_Send/1/send_on", Args: []any{float32(1)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildAuxSendOnMessages() = %+v, want %+v (aux's own layout, not default, should be used)", got, want)
	}
}
