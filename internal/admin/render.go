package admin

import (
	"io"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func (h *Handler) renderSourcesSection(w io.Writer, cfg oscmsg.ConfigPayload, layoutID string, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "sources-section", sourcesSectionData(cfg, layoutID, oob))
}

func (h *Handler) renderAuxesSection(w io.Writer, cfg oscmsg.ConfigPayload, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "auxes-section", auxesSectionData(cfg, h.icons, oob))
}

func (h *Handler) renderPresetsSection(w io.Writer) error {
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		return err
	}
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		return err
	}
	return h.tmpl.ExecuteTemplate(w, "presets-section", toPresetRows(presets, state.UUID))
}
