package admin

import (
	"io"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// Each render* method executes exactly one named section template — the
// same blocks layout.html.tmpl composes for the full page are reused here
// as htmx partial-swap responses, so there's one source of truth per
// section rather than a full-page template and a set of near-duplicate
// fragment templates.

func (h *Handler) renderSourcesSection(w io.Writer, layout []oscmsg.LayoutItem, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "sources-section", toSourcesSectionData(layout, oob))
}

func (h *Handler) renderAuxesSection(w io.Writer, auxes []oscmsg.AuxConfig, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "auxes-section", AuxesSectionData{
		Auxes: toAuxRows(auxes), Icons: h.icons, OOB: oob,
	})
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
