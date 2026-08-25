package hub

import (
	"fmt"
	"sort"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// buildConfig assembles what a mixer client needs to render itself: live
// desk-reported data (names, aux stereo/mono, counts) as the base, with the
// admin-editable overlay (sourceOverrides/auxOverrides, from
// config/state.json) layered on top for enabled/order/title/icon/colour.
// A source or aux with no override yet defaults to enabled, natural
// desk-reported order, no title/icon. Groups default to appearing after
// all channels — per the plan's "Group buses" section — but an override's
// DisplayOrder can interleave them anywhere.
func (h *Hub) buildConfig() oscmsg.ConfigPayload {
	var auxes []oscmsg.AuxConfig
	if modes, ok := h.cache["/Console/Aux_Outputs/modes"]; ok {
		auxes = make([]oscmsg.AuxConfig, 0, len(modes.Args))
		for i, m := range modes.Args {
			channel := i + 1
			label := ""
			if nameMsg, ok := h.cache[fmt.Sprintf("/Aux_Outputs/%d/Buss_Trim/name", channel)]; ok && len(nameMsg.Args) > 0 {
				label = argAsString(nameMsg.Args[0])
			}

			enabled := true
			colour := generateColour(len(modes.Args), i)
			icon := ""
			if i < len(h.auxOverrides) {
				ov := h.auxOverrides[i]
				enabled = ov.Enabled
				if ov.Colour != "" {
					colour = ov.Colour
				}
				icon = ov.Icon
			}

			auxes = append(auxes, oscmsg.AuxConfig{
				Channel: channel,
				Label:   label,
				Enabled: enabled,
				Stereo:  argAsInt(m) == 2,
				Colour:  colour,
				Icon:    icon,
			})
		}
	}

	groupModes, haveGroupModes := h.cache["/Console/Group_Outputs/modes"]

	sources := make([]oscmsg.SourceConfig, 0, h.channelCount+h.groupCount)
	order := 0
	for i := 1; i <= h.channelCount; i++ {
		label := ""
		if nameMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/name", i)]; ok && len(nameMsg.Args) > 0 {
			label = argAsString(nameMsg.Args[0])
		}
		stereo := false
		if modeMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", i)]; ok && len(modeMsg.Args) > 0 {
			stereo = argAsInt(modeMsg.Args[0]) == 2
		}
		sources = append(sources, h.sourceConfigFor("channel", i, label, stereo, order))
		order++
	}
	for i := 1; i <= h.groupCount; i++ {
		label := ""
		if nameMsg, ok := h.cache[fmt.Sprintf("/Group_Outputs/%d/Buss_Trim/name", i)]; ok && len(nameMsg.Args) > 0 {
			label = argAsString(nameMsg.Args[0])
		}
		stereo := false
		if haveGroupModes && i-1 < len(groupModes.Args) {
			stereo = argAsInt(groupModes.Args[i-1]) == 2
		}
		sources = append(sources, h.sourceConfigFor("group", i, label, stereo, order))
		order++
	}

	// An override's DisplayOrder can place a group between two channels
	// (or anywhere else) — the loops above just establish a stable default
	// order for anything not yet overridden, so a final sort by the actual
	// Order field is what determines display order, not insertion order.
	sort.SliceStable(sources, func(a, b int) bool { return sources[a].Order < sources[b].Order })

	return oscmsg.ConfigPayload{
		Sources:  sources,
		Auxes:    auxes,
		Snapshot: h.snapshotName,
	}
}

func (h *Hub) sourceConfigFor(sourceType string, number int, label string, stereo bool, defaultOrder int) oscmsg.SourceConfig {
	cfg := oscmsg.SourceConfig{
		Type:    sourceType,
		Number:  number,
		Label:   label,
		Enabled: true,
		Stereo:  stereo,
		Order:   defaultOrder,
	}
	if ov, ok := h.sourceOverrides[sourceKey(sourceType, number)]; ok {
		cfg.Enabled = ov.Enabled
		cfg.Order = ov.DisplayOrder
		cfg.Title = ov.Title
		cfg.Icon = ov.Icon
	}
	return cfg
}
