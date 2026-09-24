package admin

import (
	"fmt"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func buildAuxSendOnMessages(cfg oscmsg.ConfigPayload) []oscmsg.Message {
	var msgs []oscmsg.Message
	for _, aux := range cfg.Auxes {
		if !aux.Enabled {
			continue
		}
		layoutID := aux.LayoutID
		if layoutID == "" {
			layoutID = oscmsg.DefaultLayoutID
		}
		for _, item := range cfg.MixerLayouts[layoutID].Items {
			if item.Kind != oscmsg.LayoutKindSource || !item.Enabled {
				continue
			}
			prefix := "/Input_Channels"
			if item.Type == "group" {
				prefix = "/Group_Outputs"
			}
			msgs = append(msgs, oscmsg.Message{
				Address: fmt.Sprintf("%s/%d/Aux_Send/%d/send_on", prefix, item.Number, aux.Channel),
				Args:    []any{float32(1)},
			})
		}
	}
	return msgs
}
