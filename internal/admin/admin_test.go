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
	"sync"
	"testing"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
	"github.com/mederham/oscwebmixer-go/web"
)

const testVersion = "1.2.3-test"

// fakeDesk is a minimal hub.DeskSender that answers just enough of the
// startup sequence (1 channel, 1 aux, 0 groups) for hub to leave Starting.
// These tests exercise the admin HTTP layer's persistence/apply flow, not
// hub's startup sequencer — see internal/hub's own tests for that.
// remoteCalls/rebindCalls record every SetRemote/Rebind call, guarded by
// mu since they're written from hub's own goroutine (via handleAdminEvent)
// concurrently with a test goroutine's assertions.
type fakeDesk struct {
	inbound chan desk.InboundMessage

	mu          sync.Mutex
	remoteCalls []remoteCall
	rebindCalls []int
}

type remoteCall struct {
	ip   string
	port int
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

func (d *fakeDesk) SetRemote(ip string, port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.remoteCalls = append(d.remoteCalls, remoteCall{ip: ip, port: port})
	return nil
}

func (d *fakeDesk) Rebind(port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rebindCalls = append(d.rebindCalls, port)
	return nil
}

func (d *fakeDesk) remoteCallsSnapshot() []remoteCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]remoteCall(nil), d.remoteCalls...)
}

func (d *fakeDesk) rebindCallsSnapshot() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.rebindCalls...)
}

// newTestHandler builds a Handler backed by a real (but desk-less) hub and
// a config.Manager rooted at a temp dir — the same request/response-over-
// channel path production code uses, just with a scripted desk standing in
// for hardware.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, _ := newTestHandlerWithDesk(t)
	return handler
}

// newTestHandlerWithDesk is newTestHandler plus the *fakeDesk backing it,
// for tests that need to assert on SetRemote/Rebind calls (Network tab
// live-apply) — most tests only need the handler, hence the thin
// newTestHandler wrapper above rather than changing every existing caller.
func newTestHandlerWithDesk(t *testing.T) (*Handler, *fakeDesk) {
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
	handler, err := New(tmplFS, cfg, adminEvents, log.New(io.Discard, "", 0), testVersion)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler, fd
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
	for _, want := range []string{"Network", "Sources", "Auxes", "External devices", "Presets", "status-dot", "v" + testVersion} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing expected content %q", want)
		}
	}
}

// TestAdminResponsesAreNotCached is the regression test for a live bug: a
// plain browser reload of /admin (not a hard reload) served a stale
// Sources tab — a cached response from before the last edit — because
// nothing told the browser this page can change from any other request.
// Cache-Control: no-store on every admin route (see Register's doc
// comment) is what makes a plain reload always hit the server fresh.
func TestAdminResponsesAreNotCached(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET /admin Cache-Control = %q, want %q", got, "no-store")
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
	for _, it := range cfg.MixerLayouts["default"].Items {
		if it.Kind == oscmsg.LayoutKindSection && it.SectionName == "Drums" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mixer layout = %+v, want a Drums section marker", cfg.MixerLayouts["default"].Items)
	}
}

// TestSectionAddInsertsAtGivenPosition proves the hover "+" button's
// beforeKey (see admin.js/layoutItemKey) actually places the new section
// where clicked, not just at the end — first before a source, then before
// another section, covering both branches of layoutItemKey's key format.
func TestSectionAddInsertsAtGivenPosition(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
	})
	postForm(t, mux, "/admin/sections", "name=Drums&beforeKey="+url.QueryEscape("source:channel:1"))

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts["default"].Items) != 2 ||
		cfg.MixerLayouts["default"].Items[0].Kind != oscmsg.LayoutKindSection || cfg.MixerLayouts["default"].Items[0].SectionName != "Drums" ||
		cfg.MixerLayouts["default"].Items[1].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[1].Number != 1 {
		t.Fatalf("mixer layout = %+v, want [section Drums, channel 1] — Drums inserted before channel 1", cfg.MixerLayouts["default"].Items)
	}

	postForm(t, mux, "/admin/sections", "name=Vocals&beforeKey="+url.QueryEscape("section:"+cfg.MixerLayouts["default"].Items[0].SectionID))

	cfg, err = h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts["default"].Items) != 3 ||
		cfg.MixerLayouts["default"].Items[0].SectionName != "Vocals" ||
		cfg.MixerLayouts["default"].Items[1].SectionName != "Drums" ||
		cfg.MixerLayouts["default"].Items[2].Number != 1 {
		t.Fatalf("mixer layout = %+v, want [section Vocals, section Drums, channel 1] — Vocals inserted before Drums", cfg.MixerLayouts["default"].Items)
	}
}

// TestSectionAddFallsBackToAppendOnUnmatchedBeforeKey proves a beforeKey
// naming a row that no longer exists (e.g. a stale key from a page the
// admin hasn't refreshed) doesn't error or silently drop the new
// section — it falls back to the same append-at-the-end behavior as
// posting no beforeKey at all.
func TestSectionAddFallsBackToAppendOnUnmatchedBeforeKey(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
	})
	postForm(t, mux, "/admin/sections", "name=Drums&beforeKey=source:channel:99")

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts["default"].Items) != 2 ||
		cfg.MixerLayouts["default"].Items[0].Kind != oscmsg.LayoutKindSource ||
		cfg.MixerLayouts["default"].Items[1].Kind != oscmsg.LayoutKindSection || cfg.MixerLayouts["default"].Items[1].SectionName != "Drums" {
		t.Fatalf("mixer layout = %+v, want [channel 1, section Drums] — an unmatched beforeKey should fall back to appending", cfg.MixerLayouts["default"].Items)
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
	if len(cfg.MixerLayouts["default"].Items) != 2 ||
		cfg.MixerLayouts["default"].Items[0].Kind != oscmsg.LayoutKindSection || cfg.MixerLayouts["default"].Items[0].SectionName != "Drums" ||
		cfg.MixerLayouts["default"].Items[1].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[1].Number != 1 {
		t.Fatalf("mixer layout = %+v, want [section Drums, channel 1] in exactly that order", cfg.MixerLayouts["default"].Items)
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
	if len(cfg.MixerLayouts["default"].Items) != 1 || cfg.MixerLayouts["default"].Items[0].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[0].Number != 1 {
		t.Fatalf("mixer layout after section delete = %+v, want just channel 1 (marker removed, source kept)", cfg.MixerLayouts["default"].Items)
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
	for _, it := range cfg.MixerLayouts["default"].Items {
		if it.Kind == oscmsg.LayoutKindSection {
			t.Fatalf("mixer layout after reset = %+v, want no section markers", cfg.MixerLayouts["default"].Items)
		}
	}
	if len(cfg.MixerLayouts["default"].Items) != 1 || cfg.MixerLayouts["default"].Items[0].Number != 1 || !cfg.MixerLayouts["default"].Items[0].Enabled || cfg.MixerLayouts["default"].Items[0].Icon != "" {
		t.Fatalf("mixer layout after reset = %+v, want channel 1 back to defaults (enabled, no icon)", cfg.MixerLayouts["default"].Items)
	}
}

func TestSourcesSaveRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false, Icon: "https://example.com/icon.svg"},
	})

	// The redesigned source-card markup (badge derived via the template
	// func, not a stored field — see SourceRow) renders correctly with a
	// real, populated card.
	respBody := rec.Body.String()
	for _, want := range []string{`class="source-card channel`, `class="enabled-field eye"`, "Ch 01"} {
		if !strings.Contains(respBody, want) {
			t.Errorf("source card response missing %q: %s", want, respBody)
		}
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts["default"].Items) != 1 {
		t.Fatalf("mixer layout = %+v, want 1", cfg.MixerLayouts["default"].Items)
	}
	s := cfg.MixerLayouts["default"].Items[0]
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
	rec := postForm(t, mux, "/admin/auxes", body)

	// The redesigned aux-card markup renders correctly with a real,
	// populated row — not just an empty list — since ExecuteTemplate
	// writes to the response as it goes and a mid-render error wouldn't
	// change the HTTP status this test already checks in postForm.
	respBody := rec.Body.String()
	for _, want := range []string{`class="aux-card`, `class="eye"`, "Aux 1"} {
		if !strings.Contains(respBody, want) {
			t.Errorf("aux card response missing %q: %s", want, respBody)
		}
	}

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
	for _, it := range cfg.MixerLayouts["default"].Items {
		if it.Kind == oscmsg.LayoutKindSection {
			t.Fatalf("mixer layout after reset = %+v, want no section markers", cfg.MixerLayouts["default"].Items)
		}
	}
	if len(cfg.MixerLayouts["default"].Items) != 1 || cfg.MixerLayouts["default"].Items[0].Number != 1 || !cfg.MixerLayouts["default"].Items[0].Enabled {
		t.Fatalf("mixer layout after reset = %+v, want channel 1 back to defaults (enabled)", cfg.MixerLayouts["default"].Items)
	}
	if len(cfg.Auxes) != 1 || !cfg.Auxes[0].Enabled || cfg.Auxes[0].Icon != "" {
		t.Fatalf("auxes after reset = %+v, want aux 1 back to defaults (enabled, no icon)", cfg.Auxes)
	}
}

// TestPresetActiveAndUpdate proves loading a preset marks it Active in
// the rendered list (see PresetRow.Active's doc comment for how that's
// derived purely from state.UUID, no new stored field), and that Update
// overwrites the preset's saved content with whatever the live state
// currently is.
func TestPresetActiveAndUpdate(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	indexBody := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin: status = %d", rec.Code)
		}
		return rec.Body.String()
	}

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
	})
	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))

	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if len(presets) != 1 {
		t.Fatalf("presets = %+v, want 1", presets)
	}
	uuid := presets[0].UUID

	// Saving a preset doesn't load it — nothing should be active yet.
	if strings.Contains(indexBody(), `class="preset-card active"`) {
		t.Fatal("a preset is marked active before any preset was loaded")
	}

	postForm(t, mux, "/admin/presets/"+uuid+"/load", "")
	if !strings.Contains(indexBody(), `class="preset-card active"`) {
		t.Fatal("the loaded preset is not marked active")
	}

	// Edit the now-active live state, then Update the preset with it.
	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})
	postForm(t, mux, "/admin/presets/"+uuid+"/update", "")

	updated, err := h.cfg.GetPresetByID(uuid)
	if err != nil {
		t.Fatalf("GetPresetByID: %v", err)
	}
	if len(updated.MixerLayouts["default"].Items) != 1 || updated.MixerLayouts["default"].Items[0].Enabled {
		t.Fatalf("preset after update = %+v, want channel 1 disabled (the live state at update time)", updated.MixerLayouts["default"].Items)
	}
}

// TestLockSurvivesPresetLoad proves the lock is a runtime-only kill switch,
// not part of saved state: it isn't in config.State/config.Preset at all,
// so loading a preset (which overwrites layout/auxes live) must leave an
// active lock untouched rather than silently clearing it.
func TestLockSurvivesPresetLoad(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	uuid := presets[0].UUID

	postForm(t, mux, "/admin/lock", "locked=on")

	postForm(t, mux, "/admin/presets/"+uuid+"/load", "")

	status, err := h.queryStatus(context.Background())
	if err != nil {
		t.Fatalf("queryStatus: %v", err)
	}
	if !status.Locked {
		t.Fatal("Locked = false after loading a preset, want true (lock must survive preset/session changes)")
	}
}

// TestSettingsSaveAppliesLive proves the Settings tab's per-type auto-hide
// patterns (see config.Global's AutoHide*Patterns fields) round-trip to
// disk and reach hub live, with no restart — fakeDesk's one channel is
// named "Kick", so a channel pattern of "Kick" should flip it from the
// usual default-enabled to hidden the moment the setting is saved, while
// the same text in the aux pattern field must not affect it (per-type
// scoping, not one shared list).
func TestSettingsSaveAppliesLive(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postForm(t, mux, "/admin/settings",
		"autoHideChannelPatterns="+url.QueryEscape("Kick, Master")+"&autoHideAuxPatterns="+url.QueryEscape("Kick"))
	if !strings.Contains(rec.Body.String(), `value="Kick, Master"`) {
		t.Fatalf("settings response = %s, want the saved channel patterns echoed back into the field", rec.Body.String())
	}

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if want := []string{"Kick", "Master"}; !equalStrings(global.AutoHideChannelPatterns, want) {
		t.Fatalf("persisted AutoHideChannelPatterns = %v, want %v", global.AutoHideChannelPatterns, want)
	}
	if want := []string{"Kick"}; !equalStrings(global.AutoHideAuxPatterns, want) {
		t.Fatalf("persisted AutoHideAuxPatterns = %v, want %v", global.AutoHideAuxPatterns, want)
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts["default"].Items) != 1 || cfg.MixerLayouts["default"].Items[0].Enabled {
		t.Fatalf("mixer layout after settings save = %+v, want channel 1 (Kick) hidden by the channel pattern, applied live with no restart", cfg.MixerLayouts["default"].Items)
	}
	// fakeDesk's one aux is named "Mon 1" (see fakeDesk.Send) — unaffected
	// by the "Kick" aux pattern, since nothing named "Kick" exists among
	// auxes; this mainly guards against a regression that accidentally
	// wired the channel field into the aux check or vice versa.
	if len(cfg.Auxes) != 1 || !cfg.Auxes[0].Enabled {
		t.Fatalf("auxes after settings save = %+v, want aux 1 (Mon 1) still enabled", cfg.Auxes)
	}
}

// TestSettingsSaveAppliesDebug proves Debug logging — moved to the
// Settings tab (see handleSettingsUpdate) — round-trips to disk and
// reaches hub live via the same AdminApply path as the auto-hide
// patterns, not a restart.
func TestSettingsSaveAppliesDebug(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postForm(t, mux, "/admin/settings", "debug=on")
	if !strings.Contains(rec.Body.String(), `name="debug" checked`) {
		t.Fatalf("settings response = %s, want the debug checkbox to come back checked", rec.Body.String())
	}

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if !global.Debug {
		t.Fatal("persisted Debug = false, want true")
	}

	// Unchecking (an absent "debug" field, same as an unchecked checkbox
	// submitting nothing) must turn it back off, not leave the old value.
	rec = postForm(t, mux, "/admin/settings", "")
	if strings.Contains(rec.Body.String(), `name="debug" checked`) {
		t.Fatalf("settings response = %s, want the debug checkbox unchecked after omitting the field", rec.Body.String())
	}
	global, err = h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if global.Debug {
		t.Fatal("persisted Debug = true after unchecking, want false")
	}
}

// TestLockToggleAppliesLive proves POST /admin/lock reaches hub live (via
// AdminSetLocked, reflected in queryStatus) with no disk write — locked is
// deliberately runtime-only, see hub.Hub.locked's doc comment.
func TestLockToggleAppliesLive(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postForm(t, mux, "/admin/lock", "locked=on")
	if !strings.Contains(rec.Body.String(), "checked") || !strings.Contains(rec.Body.String(), "locked") {
		t.Fatalf("lock response = %s, want the toggle to come back checked/locked", rec.Body.String())
	}
	status, err := h.queryStatus(context.Background())
	if err != nil {
		t.Fatalf("queryStatus: %v", err)
	}
	if !status.Locked {
		t.Fatal("hub Locked = false after locking, want true")
	}

	rec = postForm(t, mux, "/admin/lock", "")
	if strings.Contains(rec.Body.String(), "checked") {
		t.Fatalf("lock response = %s, want the toggle unchecked after omitting the field", rec.Body.String())
	}
	status, err = h.queryStatus(context.Background())
	if err != nil {
		t.Fatalf("queryStatus: %v", err)
	}
	if status.Locked {
		t.Fatal("hub Locked = true after unlocking, want false")
	}
}

// TestNetworkUpdateAppliesLiveOnlyWhenChanged proves POST /admin/network
// reaches h.desk via SetRemote/Rebind end-to-end through the real HTTP
// handler (internal/hub's own tests already cover applyNetworkChange's
// diffing in isolation — this proves the admin layer actually wires it
// up), and that re-saving identical values doesn't re-trigger either call.
func TestNetworkUpdateAppliesLiveOnlyWhenChanged(t *testing.T) {
	h, fd := newTestHandlerWithDesk(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/network", "deskIP=10.0.0.5&deskPort=8000&oscPort=9000")

	if calls := fd.remoteCallsSnapshot(); len(calls) != 1 || calls[0] != (remoteCall{ip: "10.0.0.5", port: 8000}) {
		t.Fatalf("SetRemote calls = %+v, want exactly one call with the new desk address", calls)
	}
	if calls := fd.rebindCallsSnapshot(); len(calls) != 1 || calls[0] != 9000 {
		t.Fatalf("Rebind calls = %v, want exactly one call with the new OSC port", calls)
	}

	// Same values again — nothing changed from hub's point of view.
	postForm(t, mux, "/admin/network", "deskIP=10.0.0.5&deskPort=8000&oscPort=9000")

	if calls := fd.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after a no-op re-save, want still exactly one", calls)
	}
	if calls := fd.rebindCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("Rebind calls = %v after a no-op re-save, want still exactly one", calls)
	}
}

// TestNetworkFormHasNoServerPortField is a regression guard: the HTTP
// server port was deliberately removed from the admin panel (rebinding
// the very listener serving this request isn't supported — see
// OSCWEBMIXER_HTTP_PORT in main.go instead), and must stay removed.
func TestNetworkFormHasNoServerPortField(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "serverPort") {
		t.Fatalf("index page still contains a serverPort field: %s", rec.Body.String())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
