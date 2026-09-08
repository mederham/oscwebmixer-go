package hub

import (
	"fmt"
	"strings"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func (h *Hub) buildConfig() oscmsg.ConfigPayload {
	matchesAny := func(label string, patterns []string) bool {
		if label == "" {
			return false
		}
		lower := strings.ToLower(label)
		for _, p := range patterns {
			if p != "" && strings.Contains(lower, strings.ToLower(p)) {
				return true
			}
		}
		return false
	}

	var auxes []oscmsg.AuxConfig
	if modes, ok := h.cache["/Console/Aux_Outputs/modes"]; ok {
		auxes = make([]oscmsg.AuxConfig, 0, len(modes.Args))
		for i, m := range modes.Args {
			channel := i + 1
			label := ""
			if nameMsg, ok := h.cache[fmt.Sprintf("/Aux_Outputs/%d/Buss_Trim/name", channel)]; ok && len(nameMsg.Args) > 0 {
				label = argAsString(nameMsg.Args[0])
			}

			enabled := !matchesAny(label, h.autoHideAuxPatterns)
			colour := generateColour(len(modes.Args), i)
			icon := ""
			layoutID := ""
			if i < len(h.auxOverrides) {
				ov := h.auxOverrides[i]
				enabled = ov.Enabled
				if ov.Colour != "" {
					colour = ov.Colour
				}
				icon = ov.Icon
				layoutID = ov.LayoutID
			}

			auxes = append(auxes, oscmsg.AuxConfig{
				Channel:  channel,
				Label:    label,
				Enabled:  enabled,
				Stereo:   argAsInt(m) == 2,
				Colour:   colour,
				Icon:     icon,
				LayoutID: layoutID,
			})
		}
	}

	groupModes, haveGroupModes := h.cache["/Console/Group_Outputs/modes"]

	liveSource := func(sourceType string, number int) (label string, stereo bool) {
		if sourceType == "group" {
			if nameMsg, ok := h.cache[fmt.Sprintf("/Group_Outputs/%d/Buss_Trim/name", number)]; ok && len(nameMsg.Args) > 0 {
				label = argAsString(nameMsg.Args[0])
			}
			if haveGroupModes && number-1 < len(groupModes.Args) {
				stereo = argAsInt(groupModes.Args[number-1]) == 2
			}
			return
		}
		if nameMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/name", number)]; ok && len(nameMsg.Args) > 0 {
			label = argAsString(nameMsg.Args[0])
		}
		if modeMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", number)]; ok && len(modeMsg.Args) > 0 {
			stereo = argAsInt(modeMsg.Args[0]) == 2
		}
		return
	}

	exists := func(sourceType string, number int) bool {
		if sourceType == "group" {
			return number >= 1 && number <= h.groupCount
		}
		return number >= 1 && number <= h.channelCount
	}

	buildLayout := func(items []config.LayoutItem) []oscmsg.LayoutItem {
		layout := make([]oscmsg.LayoutItem, 0, len(items)+h.channelCount+h.groupCount)
		placed := make(map[string]bool, len(items))
		for _, item := range items {
			if item.Kind == config.LayoutKindSection {
				layout = append(layout, oscmsg.LayoutItem{
					Kind: oscmsg.LayoutKindSection, SectionID: item.SectionID, SectionName: item.SectionName,
				})
				continue
			}
			if !exists(item.Type, item.Number) {
				continue
			}
			label, stereo := liveSource(item.Type, item.Number)
			layout = append(layout, oscmsg.LayoutItem{
				Kind: oscmsg.LayoutKindSource, Type: item.Type, Number: item.Number,
				Label: label, Enabled: item.Enabled, Stereo: stereo,
			})
			placed[sourceKey(item.Type, item.Number)] = true
		}
		for i := 1; i <= h.channelCount; i++ {
			if placed[sourceKey("channel", i)] {
				continue
			}
			label, stereo := liveSource("channel", i)
			layout = append(layout, oscmsg.LayoutItem{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: i, Label: label, Enabled: !matchesAny(label, h.autoHideChannelPatterns), Stereo: stereo})
		}
		for i := 1; i <= h.groupCount; i++ {
			if placed[sourceKey("group", i)] {
				continue
			}
			label, stereo := liveSource("group", i)
			layout = append(layout, oscmsg.LayoutItem{Kind: oscmsg.LayoutKindSource, Type: "group", Number: i, Label: label, Enabled: !matchesAny(label, h.autoHideGroupPatterns), Stereo: stereo})
		}
		return layout
	}

	layouts := make(map[string]oscmsg.NamedLayout, len(h.layouts))
	for id, nl := range h.layouts {
		layouts[id] = oscmsg.NamedLayout{Name: nl.Name, Items: buildLayout(nl.Items)}
	}

	return oscmsg.ConfigPayload{
		MixerLayouts: layouts,
		Auxes:        auxes,
		Snapshot:     h.snapshotName,
	}
}
