package hub

import (
	"fmt"
	"strings"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// buildConfig assembles what a mixer client needs to render itself: live
// desk-reported data (names, aux stereo/mono, counts) as the base, with the
// admin-editable overlay (h.layouts/h.auxOverrides, from config/state.json)
// layered on top for enabled/icon/colour and — for each entry in h.layouts
// — the actual display sequence itself. A source not otherwise mentioned in
// a given layout defaults to enabled (unless its live name matches that
// type's auto-hide patterns — see matchesAny and config.Global's
// AutoHide*Patterns fields), no icon, and is appended at the end in
// natural desk-reported order (channels then groups) — see buildLayout
// below, run once per named layout.
func (h *Hub) buildConfig() oscmsg.ConfigPayload {
	// matchesAny is the fallback-only "should a not-yet-customized entry
	// start visible" check — see config.Global's AutoHide*Patterns doc
	// comment. Never consulted for an entry already present in
	// h.layouts/h.auxOverrides; those always keep their stored Enabled.
	// Patterns are checked per source type (not one shared list) because
	// names collide by convention across types — an aux bus "Hall" and its
	// return channel "Hall Ret" both contain "Hall", but only one should
	// actually be hidden.
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

	// liveSource looks up live desk-reported label/stereo for one
	// channel/group — shared by both the laid-out walk below and the
	// natural-order fallback for anything a layout doesn't mention.
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

	// buildLayout runs the merge (live label/stereo overlay, natural-
	// order fallback for anything items doesn't mention) for one named
	// layout's Items — called once per entry in h.layouts below, so every
	// layout is independently sparse and gets the same fallback treatment
	// the single global layout got before named layouts existed.
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
				// Session changed to fewer channels/groups than this entry
				// references (confirmed live: sessions can differ in channel
				// count) — drop it rather than render a source that no longer
				// exists on the desk. It simply reappears if a future session
				// has that channel again and the admin re-adds it.
				continue
			}
			label, stereo := liveSource(item.Type, item.Number)
			layout = append(layout, oscmsg.LayoutItem{
				Kind: oscmsg.LayoutKindSource, Type: item.Type, Number: item.Number,
				Label: label, Enabled: item.Enabled, Stereo: stereo, Icon: item.Icon,
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
