package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
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

type fakeDesk struct {
	inbound chan desk.InboundMessage

	mu           sync.Mutex
	remoteCalls  []remoteCall
	rebindCalls  []int
	sentMessages []oscmsg.Message
	sentTo       []sentToCall
}

type remoteCall struct {
	ip   string
	port int
}

type sentToCall struct {
	msg  oscmsg.Message
	addr *net.UDPAddr
}

func (d *fakeDesk) reply(address string, args ...any) {
	if args == nil {
		args = []any{}
	}
	d.inbound <- desk.InboundMessage{Message: oscmsg.Message{Address: address, Args: args}}
}

func (d *fakeDesk) Send(msg oscmsg.Message) error {
	d.mu.Lock()
	d.sentMessages = append(d.sentMessages, msg)
	d.mu.Unlock()
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

func (d *fakeDesk) SendTo(msg oscmsg.Message, addr *net.UDPAddr) error {
	d.mu.Lock()
	d.sentTo = append(d.sentTo, sentToCall{msg: msg, addr: addr})
	d.mu.Unlock()
	return nil
}

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

func (d *fakeDesk) sentMessagesSnapshot() []oscmsg.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]oscmsg.Message(nil), d.sentMessages...)
}

func (d *fakeDesk) sentToSnapshot() []sentToCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sentToCall(nil), d.sentTo...)
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, _ := newTestHandlerWithDesk(t)
	return handler
}

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

func postFormExpectStatus(t *testing.T, mux *http.ServeMux, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("POST %s: status = %d, want %d, body = %s", path, rec.Code, want, rec.Body.String())
	}
	return rec
}

func postMultipartFile(t *testing.T, mux *http.ServeMux, path, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func postSourcesSave(t *testing.T, mux *http.ServeMux, items []oscmsg.LayoutItem) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal layout: %v", err)
	}
	return postForm(t, mux, "/admin/sources/save", url.Values{"layout": {string(data)}}.Encode())
}

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

func TestSourcesReset(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
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
	if len(cfg.MixerLayouts["default"].Items) != 1 || cfg.MixerLayouts["default"].Items[0].Number != 1 || !cfg.MixerLayouts["default"].Items[0].Enabled {
		t.Fatalf("mixer layout after reset = %+v, want channel 1 back to defaults (enabled)", cfg.MixerLayouts["default"].Items)
	}
}

func TestSourcesSaveRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})

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
}

func TestAuxesSaveRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	body := "colour[1]=%23ff0000&icon[1]=mic.svg"
	rec := postForm(t, mux, "/admin/auxes", body)

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

func TestPresetsReset(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSection, SectionID: "s1", SectionName: "Drums"},
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})
	postForm(t, mux, "/admin/auxes", "colour[1]=%23ff0000&icon[1]=mic.svg")

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

	if strings.Contains(indexBody(), `class="preset-card active"`) {
		t.Fatal("a preset is marked active before any preset was loaded")
	}

	postForm(t, mux, "/admin/presets/"+uuid+"/load", "")
	if !strings.Contains(indexBody(), `class="preset-card active"`) {
		t.Fatal("the loaded preset is not marked active")
	}

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

func TestPresetDownload(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	uuid := presets[0].UUID

	req := httptest.NewRequest(http.MethodGet, "/admin/presets/"+uuid+"/download", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET download: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "Show 1.json") {
		t.Fatalf("Content-Disposition = %q, want it to name the preset", rec.Header().Get("Content-Disposition"))
	}
	var got config.Preset
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got.Name != "Show 1" || got.UUID != uuid {
		t.Fatalf("downloaded preset = %+v, want Name=Show 1 UUID=%s", got, uuid)
	}
}

func TestPresetDownloadUnknownUUID(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin/presets/does-not-exist/download", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPresetsDownloadAll(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))
	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 2"))

	req := httptest.NewRequest(http.MethodGet, "/admin/presets/download", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET download all: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []config.Preset
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("downloaded presets = %+v, want 2", got)
	}
}

func TestPresetsUpload(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))

	payload := `[{"name":"Show 1","mixerLayouts":{"default":{"name":"Default","items":[]}}}]`
	rec := postMultipartFile(t, mux, "/admin/presets/upload", "presets.json", []byte(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Show 1 (2)") {
		t.Fatalf("response = %s, want the auto-renamed imported preset", rec.Body.String())
	}

	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if len(presets) != 2 {
		t.Fatalf("presets = %+v, want 2 (original + imported)", presets)
	}
}

func TestPresetsUploadRejectsMalformedJSON(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postMultipartFile(t, mux, "/admin/presets/upload", "presets.json", []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

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

	if len(cfg.Auxes) != 1 || !cfg.Auxes[0].Enabled {
		t.Fatalf("auxes after settings save = %+v, want aux 1 (Mon 1) still enabled", cfg.Auxes)
	}
}

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

	postForm(t, mux, "/admin/network", "deskIP=10.0.0.5&deskPort=8000&oscPort=9000")

	if calls := fd.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after a no-op re-save, want still exactly one", calls)
	}
	if calls := fd.rebindCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("Rebind calls = %v after a no-op re-save, want still exactly one", calls)
	}
}

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

	postForm(t, mux, "/admin/external-devices/"+id, "name=iPad Renamed Again&ip=10.0.0.51&port=8001&broadcast=on")
	global, err = h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if global.ExternalDevices[0].Name != "iPad Renamed Again" {
		t.Fatalf("second update (post-rename) did not apply: %+v", global.ExternalDevices[0])
	}
}

func TestLayoutCreateClonesAndSwitches(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})

	rec := postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	if !strings.Contains(rec.Body.String(), `value="Monitor Mix"`) {
		t.Errorf("response does not show the new layout as selected: %s", rec.Body.String())
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 (default + Monitor Mix)", cfg.MixerLayouts)
	}
	var newID string
	for id, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			newID = id
		}
	}
	if newID == "" {
		t.Fatal("no layout named Monitor Mix found")
	}
	items := cfg.MixerLayouts[newID].Items
	if len(items) != 1 || items[0].Number != 1 || items[0].Enabled {
		t.Fatalf("cloned items = %+v, want default's channel 1 (disabled) carried over", items)
	}
}

func TestLayoutRenameAndDeleteDefaultRefused(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postFormExpectStatus(t, mux, "/admin/layouts/default/rename", "name=Nope", http.StatusBadRequest)
	postFormExpectStatus(t, mux, "/admin/layouts/default/delete", "", http.StatusBadRequest)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	if id == "" {
		t.Fatal("Monitor Mix layout not found after create")
	}

	postForm(t, mux, "/admin/layouts/"+id+"/rename", "name="+url.QueryEscape("Renamed"))
	cfg, _ = h.queryConfig(context.Background())
	if cfg.MixerLayouts[id].Name != "Renamed" {
		t.Fatalf("layout name = %q, want Renamed", cfg.MixerLayouts[id].Name)
	}

	postForm(t, mux, "/admin/layouts/"+id+"/delete", "")
	cfg, _ = h.queryConfig(context.Background())
	if _, ok := cfg.MixerLayouts[id]; ok {
		t.Fatalf("MixerLayouts = %+v, want %q gone after delete", cfg.MixerLayouts, id)
	}
}

func TestAuxLayoutFallsBackAfterDelete(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}

	postForm(t, mux, "/admin/auxes", "layout[1]="+id)
	cfg, _ = h.queryConfig(context.Background())
	if cfg.Auxes[0].LayoutID != id {
		t.Fatalf("aux LayoutID = %q, want %q before delete", cfg.Auxes[0].LayoutID, id)
	}

	postForm(t, mux, "/admin/layouts/"+id+"/delete", "")
	cfg, _ = h.queryConfig(context.Background())
	if cfg.Auxes[0].LayoutID != id {
		t.Fatalf("aux LayoutID = %q, want it to still (harmlessly) name the deleted id %q — no rewrite-on-delete", cfg.Auxes[0].LayoutID, id)
	}
	if _, ok := cfg.MixerLayouts[cfg.Auxes[0].LayoutID]; ok {
		t.Fatalf("MixerLayouts still has %q, want it gone (deleted)", cfg.Auxes[0].LayoutID)
	}

}

func TestPresetSaveLoadPreservesMultipleLayouts(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var monID string
	for id, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			monID = id
		}
	}
	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})
	postForm(t, mux, "/admin/auxes", "layout[1]="+monID)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if len(presets) != 1 {
		t.Fatalf("presets = %+v, want 1", presets)
	}
	preset := presets[0]
	if len(preset.MixerLayouts) != 2 {
		t.Fatalf("preset.MixerLayouts = %+v, want 2 (default + Monitor Mix)", preset.MixerLayouts)
	}
	if preset.MixerLayouts[monID].Name != "Monitor Mix" {
		t.Fatalf("preset's monitor layout = %+v, want Name=Monitor Mix", preset.MixerLayouts[monID])
	}
	if preset.Auxes[0].LayoutID != monID {
		t.Fatalf("preset aux LayoutID = %q, want %q", preset.Auxes[0].LayoutID, monID)
	}

	postForm(t, mux, "/admin/presets/reset", "")
	postForm(t, mux, "/admin/presets/"+preset.UUID+"/load", "")
	cfg, err = h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig after load: %v", err)
	}
	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts after load = %+v, want 2", cfg.MixerLayouts)
	}
	if cfg.Auxes[0].LayoutID != monID {
		t.Fatalf("aux LayoutID after load = %q, want %q", cfg.Auxes[0].LayoutID, monID)
	}
}

func TestSourcesTabRendersLayoutPicker(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`class="layout-picker"`, `name="layout"`, `+ New layout`} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Delete layout") {
		t.Fatal("Default layout view should not show Delete layout")
	}

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/sources?layout="+id, nil)
	mux.ServeHTTP(rec, req)
	body = rec.Body.String()
	for _, want := range []string{"Delete layout", `value="Monitor Mix"`} {
		if !strings.Contains(body, want) {
			t.Errorf("custom layout view missing %q: %s", want, body)
		}
	}
}

func TestAuxSendOnReachesDeskAndExternalDevices(t *testing.T) {
	h, fd := newTestHandlerWithDesk(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/external-devices", "name=iPad&ip=10.0.0.50&port=8000&broadcast=on")

	rec := postForm(t, mux, "/admin/auxes/send-on", "")
	if !strings.Contains(rec.Body.String(), `class="success"`) || !strings.Contains(rec.Body.String(), "Successfully set 1 aux send(s) to ON.") {
		t.Fatalf("send-on response = %s, want the button swapped to its green success state", rec.Body.String())
	}

	resetRec := httptest.NewRecorder()
	mux.ServeHTTP(resetRec, httptest.NewRequest(http.MethodGet, "/admin/auxes/send-on/reset", nil))
	resetBody := resetRec.Body.String()
	if strings.Contains(resetBody, "success") || !strings.Contains(resetBody, "Set assigned sends to ON") {
		t.Fatalf("send-on reset response = %s, want the plain button restored", resetBody)
	}

	const wantAddr = "/Input_Channels/1/Aux_Send/1/send_on"
	var foundDeskSend bool
	for _, m := range fd.sentMessagesSnapshot() {
		if m.Address == wantAddr {
			foundDeskSend = true
		}
	}
	if !foundDeskSend {
		t.Fatalf("desk sends = %+v, want %q", fd.sentMessagesSnapshot(), wantAddr)
	}

	sentTo := fd.sentToSnapshot()
	if len(sentTo) != 1 || sentTo[0].msg.Address != wantAddr || sentTo[0].addr.IP.String() != "10.0.0.50" {
		t.Fatalf("external device sends = %+v, want one send of %q to 10.0.0.50", sentTo, wantAddr)
	}
}

func TestAuxSendOnSkipsDisabledAux(t *testing.T) {
	h, fd := newTestHandlerWithDesk(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/auxes", "") // omits enabled[1] => aux 1 disabled

	postForm(t, mux, "/admin/auxes/send-on", "")

	for _, m := range fd.sentMessagesSnapshot() {
		if strings.Contains(m.Address, "send_on") {
			t.Fatalf("desk sends = %+v, want no send_on messages (aux disabled)", fd.sentMessagesSnapshot())
		}
	}
}

func TestAuxesTabRendersLayoutSelect(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	postForm(t, mux, "/admin/auxes", "layout[1]="+id)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `name="layout[1]"`) {
		t.Fatalf("auxes tab missing layout select: %s", body)
	}

	if !strings.Contains(body, id+`" selected`) {
		t.Errorf("aux 1's layout select doesn't preselect %q: %s", id, body)
	}
}
