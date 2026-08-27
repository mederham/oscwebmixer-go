package admin

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
	"github.com/mederham/oscwebmixer-go/web"
)

// fakeDesk is a minimal hub.DeskSender that answers just enough of the
// startup sequence (1 channel, 1 aux, 0 groups) for hub to leave Starting.
// These tests exercise the admin HTTP layer's persistence/apply flow, not
// hub's startup sequencer — see internal/hub's own tests for that.
type fakeDesk struct {
	inbound chan desk.InboundMessage
}

func (d *fakeDesk) reply(address string, args ...any) {
	if args == nil {
		args = []any{}
	}
	d.inbound <- desk.InboundMessage{Message: oscmsg.Message{Address: address, Args: args}}
}

func (d *fakeDesk) Send(msg oscmsg.Message) error {
	switch msg.Address {
	case "/Console/Channels/?":
		d.reply("/Console/Input_Channels", int32(1))
	case "/Console/Aux_Outputs/modes/?":
		d.reply("/Console/Aux_Outputs/modes", int32(1))
	case "/Console/Group_Outputs/modes/?":
		d.reply("/Console/Group_Outputs/modes")
	case "/Aux_Outputs/1/Buss_Trim/name/?":
		d.reply("/Aux_Outputs/1/Buss_Trim/name", "Mon 1")
	case "/Input_Channels/1/Channel_Input/name/?":
		d.reply("/Input_Channels/1/Channel_Input/name", "Kick")
	case "/Input_Channels/1/Channel_Input/stereo_mode/?":
		d.reply("/Input_Channels/1/Channel_Input/stereo_mode", int32(1))
	}
	return nil
}

func (d *fakeDesk) SendTo(oscmsg.Message, *net.UDPAddr) error { return nil }

// newTestHandler builds a Handler backed by a real (but desk-less) hub and
// a config.Manager rooted at a temp dir — the same request/response-over-
// channel path production code uses, just with a scripted desk standing in
// for hardware.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	fd := &fakeDesk{inbound: make(chan desk.InboundMessage, 64)}
	h := hub.New(fd, nil, log.New(io.Discard, "", 0), hub.Options{StartupRetryInterval: 50 * time.Millisecond})
	adminEvents := make(chan hub.AdminEvent, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go h.Run(ctx, fd.inbound, nil, adminEvents)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("hub startup did not complete")
	}

	tmplFS, err := fs.Sub(web.AdminFS, "admin")
	if err != nil {
		t.Fatalf("admin assets: %v", err)
	}
	cfg := config.New(t.TempDir())
	if err := cfg.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	handler, err := New(tmplFS, cfg, adminEvents, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

func postForm(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// postSourcesSave posts to /admin/sources/save the same way admin.js's
// htmx:configRequest listener does in the browser: one "layout" form field
// carrying a JSON-encoded array — see internal/admin's package doc comment
// for why a plain named-field form can't represent an ordered, mixed-kind
// list.
func postSourcesSave(t *testing.T, mux *http.ServeMux, items []oscmsg.LayoutItem) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal layout: %v", err)
	}
	return postForm(t, mux, "/admin/sources/save", url.Values{"layout": {string(data)}}.Encode())
}

// TestIndexRenders is a smoke test for the full page — layout.html.tmpl
// composing all five section templates (tabs, Network's status-dot
// fragment, Sources' card grid, Auxes, External devices, Presets) at once.
// The other tests here exercise individual section fragments directly;
// this is the one path that renders every template file in this package
// together, catching a field-access typo none of the narrower tests would.
func TestIndexRenders(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Network", "Sources", "Auxes", "External devices", "Presets", "status-dot"} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing expected content %q", want)
		}
	}
}

func TestSectionCreateAppearsInResponse(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postForm(t, mux, "/admin/sections", "name=Drums")
	if !strings.Contains(rec.Body.String(), "Drums") {
		t.Fatalf("response does not contain new section name: %s", rec.Body.String())
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var found bool
	for _, it := range cfg.MixerLayout {
		if it.Kind == oscmsg.LayoutKindSection && it.SectionName == "Drums" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mixer layout = %+v, want a Drums section marker", cfg.MixerLayout)
	}
}

// TestSourcesSavePreservesOrderAndSectionPlacement is the core new-model
// regression test: the posted layout's exact order — a section marker
// immediately followed by a source — must come back unchanged, proving
// display order AND section placement both derive purely from this one
// sequence (see oscmsg.LayoutItem's doc comment).
func TestSourcesSavePreservesOrderAndSectionPlacement(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
	})

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayout) != 2 ||
		cfg.MixerLayout[0].Kind != oscmsg.LayoutKindSection || cfg.MixerLayout[0].SectionName != "Drums" ||
		cfg.MixerLayout[1].Kind != oscmsg.LayoutKindSource || cfg.MixerLayout[1].Number != 1 {
		t.Fatalf("mixer layout = %+v, want [section Drums, channel 1] in exactly that order", cfg.MixerLayout)
	}
}

// TestSectionDeleteKeepsSourcesRemovesMarker confirms the plan's explicit
// decision: deleting a section removes only its marker — sources are never
// deleted, and (since membership is derived from position, not a stored
// field) there is nothing else to update on them at all.
func TestSectionDeleteKeepsSourcesRemovesMarker(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
	})

	postForm(t, mux, "/admin/sections/s1/delete", "")

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayout) != 1 || cfg.MixerLayout[0].Kind != oscmsg.LayoutKindSource || cfg.MixerLayout[0].Number != 1 {
		t.Fatalf("mixer layout after section delete = %+v, want just channel 1 (marker removed, source kept)", cfg.MixerLayout)
	}
}

// TestSourcesReset confirms resetting clears every customization/section
// and falls back to buildConfig's own natural-order default — proving
// reset really is just "persist an empty layout," not something that
// needs to reconstruct "channels then groups, ascending" itself.
func TestSourcesReset(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false, Icon: "kick.svg"},
	})

	postForm(t, mux, "/admin/sources/reset", "")

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	for _, it := range cfg.MixerLayout {
		if it.Kind == oscmsg.LayoutKindSection {
			t.Fatalf("mixer layout after reset = %+v, want no section markers", cfg.MixerLayout)
		}
	}
	if len(cfg.MixerLayout) != 1 || cfg.MixerLayout[0].Number != 1 || !cfg.MixerLayout[0].Enabled || cfg.MixerLayout[0].Icon != "" {
		t.Fatalf("mixer layout after reset = %+v, want channel 1 back to defaults (enabled, no icon)", cfg.MixerLayout)
	}
}

func TestSourcesSaveRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false, Icon: "https://example.com/icon.svg"},
	})

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayout) != 1 {
		t.Fatalf("mixer layout = %+v, want 1", cfg.MixerLayout)
	}
	s := cfg.MixerLayout[0]
	if s.Enabled {
		t.Fatal("enabled = true, want false")
	}
	if s.Icon != "https://example.com/icon.svg" {
		t.Fatalf("icon = %q, want the posted URL", s.Icon)
	}
}

func TestAuxesSaveRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := "colour[1]=%23ff0000&icon[1]=mic.svg" // enabled[1] omitted -> disabled
	postForm(t, mux, "/admin/auxes", body)

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.Auxes) != 1 {
		t.Fatalf("auxes = %+v, want 1", cfg.Auxes)
	}
	a := cfg.Auxes[0]
	if a.Enabled {
		t.Fatal("enabled = true, want false (checkbox was omitted from the post)")
	}
	if a.Colour != "#ff0000" {
		t.Fatalf("colour = %q, want #ff0000", a.Colour)
	}
	if a.Icon != "mic.svg" {
		t.Fatalf("icon = %q, want mic.svg", a.Icon)
	}
}

// TestIconsListedFromEmbeddedFS is a sanity check that the aux icon
// picker's options actually come from web/admin/icons/*.svg — guards
// against a silently-empty dropdown if that directory or its go:embed
// wiring ever breaks.
func TestIconsListedFromEmbeddedFS(t *testing.T) {
	h := newTestHandler(t)
	if len(h.icons) == 0 {
		t.Fatal("icons list is empty, want the instrument SVG set from web/admin/icons/")
	}
	for _, name := range h.icons {
		if !strings.HasSuffix(name, ".svg") {
			t.Fatalf("icons list contains non-svg entry %q", name)
		}
	}
}

// TestPresetsReset confirms the Presets tab's reset button clears both the
// mixer layout and aux overrides back to defaults, live — the same
// "start over" action as loading a preset, just loading the zero-value
// default instead of a saved one.
func TestPresetsReset(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false, Icon: "kick.svg"},
	})
	postForm(t, mux, "/admin/auxes", "colour[1]=%23ff0000&icon[1]=mic.svg") // enabled[1] omitted -> disabled

	rec := postForm(t, mux, "/admin/presets/reset", "")
	if strings.Contains(rec.Body.String(), "Drums") {
		t.Fatalf("reset response still mentions the old section: %s", rec.Body.String())
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	for _, it := range cfg.MixerLayout {
		if it.Kind == oscmsg.LayoutKindSection {
			t.Fatalf("mixer layout after reset = %+v, want no section markers", cfg.MixerLayout)
		}
	}
	if len(cfg.MixerLayout) != 1 || cfg.MixerLayout[0].Number != 1 || !cfg.MixerLayout[0].Enabled {
		t.Fatalf("mixer layout after reset = %+v, want channel 1 back to defaults (enabled)", cfg.MixerLayout)
	}
	if len(cfg.Auxes) != 1 || !cfg.Auxes[0].Enabled || cfg.Auxes[0].Icon != "" {
		t.Fatalf("auxes after reset = %+v, want aux 1 back to defaults (enabled, no icon)", cfg.Auxes)
	}
}

func TestDotClassAndLabel(t *testing.T) {
	cases := []struct{ status, class, label string }{
		{"healthy", "green", "Online"},
		{"degraded", "red", "Offline"},
		{"starting", "yellow", "Starting…"},
		{"reloading", "yellow", "Reloading…"},
	}
	for _, c := range cases {
		if got := dotClass(c.status); got != c.class {
			t.Errorf("dotClass(%q) = %q, want %q", c.status, got, c.class)
		}
		if got := dotLabel(c.status); got != c.label {
			t.Errorf("dotLabel(%q) = %q, want %q", c.status, got, c.label)
		}
	}
}

// TestExternalDeviceUpdateByID proves identity survives a rename — the
// edit URL is keyed by ID, not Name, so a second edit after renaming must
// still find the same row.
func TestExternalDeviceUpdateByID(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/external-devices", "name=iPad&ip=10.0.0.50&port=8000&broadcast=on")

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if len(global.ExternalDevices) != 1 || global.ExternalDevices[0].ID == "" {
		t.Fatalf("external devices = %+v, want 1 with a non-empty ID", global.ExternalDevices)
	}
	id := global.ExternalDevices[0].ID

	postForm(t, mux, "/admin/external-devices/"+id, "name=iPad Renamed&ip=10.0.0.51&port=8001&broadcast=on&loopback=on")

	global, err = h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	d := global.ExternalDevices[0]
	if d.ID != id || d.Name != "iPad Renamed" || d.IP != "10.0.0.51" || d.Port != 8001 || !d.Loopback {
		t.Fatalf("device after update = %+v, want renamed fields with the same ID %q", d, id)
	}

	// Second update by the SAME ID, post-rename — must still find the row.
	postForm(t, mux, "/admin/external-devices/"+id, "name=iPad Renamed Again&ip=10.0.0.51&port=8001&broadcast=on")
	global, err = h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if global.ExternalDevices[0].Name != "iPad Renamed Again" {
		t.Fatalf("second update (post-rename) did not apply: %+v", global.ExternalDevices[0])
	}
}
