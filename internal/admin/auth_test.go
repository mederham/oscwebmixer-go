package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mederham/oscwebmixer-go/internal/config"
)

func newAuthTestMux(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()
	old := loginFailDelay
	loginFailDelay = 0
	t.Cleanup(func() { loginFailDelay = old })
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func setConfigPassword(t *testing.T, h *Handler, pw string) {
	t.Helper()
	if err := h.cfg.SaveGlobal(config.Global{AdminPassword: pw}); err != nil {
		t.Fatalf("SaveGlobal: %v", err)
	}
}

func get(mux *http.ServeMux, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func postLogin(mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, mux *http.ServeMux, pw string) *http.Cookie {
	t.Helper()
	rec := postLogin(mux, "password="+pw)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("login: no session cookie, status = %d, body = %s", rec.Code, rec.Body.String())
	return nil
}

func expectRedirect(t *testing.T, rec *httptest.ResponseRecorder, loc string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != loc {
		t.Fatalf("status = %d, Location = %q, want 303 %q", rec.Code, rec.Header().Get("Location"), loc)
	}
}

func TestNoPasswordLeavesAdminOpen(t *testing.T) {
	_, mux := newAuthTestMux(t)
	if rec := get(mux, "/admin", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /admin: status = %d", rec.Code)
	}
	expectRedirect(t, get(mux, "/admin/login", nil), "/admin")
}

func TestGuardRedirectsPlainRequest(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	rec := get(mux, "/admin", nil)
	expectRedirect(t, rec, "/admin/login")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestGuardHTMXGets401WithRedirect(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	req := httptest.NewRequest(http.MethodGet, "/admin/network/status", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/admin/login" {
		t.Errorf("HX-Redirect = %q", got)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	rec := postLogin(mux, "password=nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); sc != "" {
		t.Errorf("Set-Cookie = %q, want none", sc)
	}
	if !strings.Contains(rec.Body.String(), "Wrong password") {
		t.Errorf("body missing error: %s", rec.Body.String())
	}
}

func TestLoginSuccess(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	expectRedirect(t, postLogin(mux, "password=pw"), "/admin")
	c := login(t, mux, "pw")
	if c.Name != "oscwebmixer_admin" || c.Path != "/admin" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v", c)
	}
	rec := get(mux, "/admin", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin with cookie: status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/admin/logout"`) {
		t.Error("index missing logout form")
	}
}

func TestLoginComparesExactly(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, " a b ")
	if rec := postLogin(mux, "password=a+b"); rec.Code != http.StatusUnauthorized {
		t.Errorf("trimmed password: status = %d, want 401", rec.Code)
	}
	expectRedirect(t, postLogin(mux, "password=+a+b+"), "/admin")
}

func TestGuardRejectsUnknownToken(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	expectRedirect(t, get(mux, "/admin", &http.Cookie{Name: sessionCookie, Value: "deadbeef"}), "/admin/login")
}

func TestLogoutInvalidatesToken(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	c := login(t, mux, "pw")

	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	expectRedirect(t, rec, "/admin/login")
	cleared := false
	for _, rc := range rec.Result().Cookies() {
		if rc.Name == sessionCookie && rc.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the cookie")
	}

	expectRedirect(t, get(mux, "/admin", c), "/admin/login")
}

func TestEnvPasswordWinsOverConfig(t *testing.T) {
	h, mux := newAuthTestMux(t)
	h.envPassword = "envpw"
	setConfigPassword(t, h, "cfgpw")
	if rec := postLogin(mux, "password=cfgpw"); rec.Code != http.StatusUnauthorized {
		t.Errorf("config password: status = %d, want 401", rec.Code)
	}
	expectRedirect(t, postLogin(mux, "password=envpw"), "/admin")
}

func TestGuardReadsConfigPerRequest(t *testing.T) {
	h, mux := newAuthTestMux(t)
	if rec := get(mux, "/admin", nil); rec.Code != http.StatusOK {
		t.Fatalf("no password: status = %d", rec.Code)
	}
	setConfigPassword(t, h, "pw")
	expectRedirect(t, get(mux, "/admin", nil), "/admin/login")
	setConfigPassword(t, h, "")
	if rec := get(mux, "/admin", nil); rec.Code != http.StatusOK {
		t.Fatalf("password removed: status = %d", rec.Code)
	}
}

func TestEnvPasswordNotPersisted(t *testing.T) {
	h, mux := newAuthTestMux(t)
	h.envPassword = "envpw"
	c := login(t, mux, "envpw")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings/debug", strings.NewReader("debug=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/settings/debug: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if global.AdminPassword != "" {
		t.Errorf("AdminPassword = %q, want empty", global.AdminPassword)
	}
}

func TestLoginPageIndexHidesLogoutWithoutPassword(t *testing.T) {
	_, mux := newAuthTestMux(t)
	if strings.Contains(get(mux, "/admin", nil).Body.String(), "/admin/logout") {
		t.Error("index shows logout without a password")
	}
}

func postPassword(mux *http.ServeMux, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func expectConfigPassword(t *testing.T, h *Handler, want string) {
	t.Helper()
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if global.AdminPassword != want {
		t.Errorf("AdminPassword = %q, want %q", global.AdminPassword, want)
	}
}

func TestPasswordSetFromOpenAdmin(t *testing.T) {
	h, mux := newAuthTestMux(t)
	rec := postPassword(mux, "action=set&password=pw&confirm=pw", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var c *http.Cookie
	for _, rc := range rec.Result().Cookies() {
		if rc.Name == sessionCookie {
			c = rc
		}
	}
	if c == nil {
		t.Fatal("no session cookie set")
	}
	expectConfigPassword(t, h, "pw")
	if rec := get(mux, "/admin", c); rec.Code != http.StatusOK {
		t.Fatalf("GET /admin with new cookie: status = %d", rec.Code)
	}
}

func TestPasswordMismatch(t *testing.T) {
	h, mux := newAuthTestMux(t)
	rec := postPassword(mux, "action=set&password=a&confirm=b", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Passwords don&#39;t match") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	expectConfigPassword(t, h, "")
}

func TestPasswordEmptyRejected(t *testing.T) {
	h, mux := newAuthTestMux(t)
	rec := postPassword(mux, "action=set&password=&confirm=", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Password can&#39;t be empty") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	expectConfigPassword(t, h, "")
}

func TestPasswordChangeDropsOtherSessions(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	a := login(t, mux, "pw")
	b := login(t, mux, "pw")
	if rec := postPassword(mux, "action=set&password=pw2&confirm=pw2", a); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := get(mux, "/admin", a); rec.Code != http.StatusOK {
		t.Errorf("current session: status = %d", rec.Code)
	}
	expectRedirect(t, get(mux, "/admin", b), "/admin/login")
}

func TestPasswordRemove(t *testing.T) {
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	c := login(t, mux, "pw")
	rec := postPassword(mux, "action=remove", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	expectConfigPassword(t, h, "")
	cleared := false
	for _, rc := range rec.Result().Cookies() {
		if rc.Name == sessionCookie && rc.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("remove did not clear the session cookie")
	}
	if rec := get(mux, "/admin", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /admin without cookie: status = %d", rec.Code)
	}
}

func TestPasswordBlockedWhenEnvSet(t *testing.T) {
	h, mux := newAuthTestMux(t)
	h.envPassword = "envpw"
	c := login(t, mux, "envpw")
	if rec := postPassword(mux, "action=set&password=x&confirm=x", c); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	expectConfigPassword(t, h, "")
	body := get(mux, "/admin", c).Body.String()
	if strings.Contains(body, `name="confirm"`) {
		t.Error("index shows password form while env is set")
	}
	if !strings.Contains(body, `action="/admin/logout"`) {
		t.Error("index missing logout form while env is set")
	}
}

// Keep in sync with Register: every route except the login pair must be guarded.
func TestEveryAdminRouteIsGuarded(t *testing.T) {
	routes := []string{
		"POST /admin/logout",
		"GET /admin",
		"POST /admin/sources/save",
		"POST /admin/sources/reset",
		"GET /admin/sources",
		"POST /admin/layouts",
		"POST /admin/layouts/x/rename",
		"POST /admin/layouts/x/delete",
		"POST /admin/sections",
		"POST /admin/sections/x/delete",
		"POST /admin/auxes",
		"POST /admin/auxes/send-on",
		"GET /admin/auxes/send-on/reset",
		"POST /admin/network",
		"GET /admin/network/status",
		"POST /admin/settings/auto-hide",
		"POST /admin/settings/debug",
		"POST /admin/password",
		"POST /admin/lock",
		"POST /admin/external-devices",
		"POST /admin/external-devices/x",
		"POST /admin/external-devices/x/delete",
		"POST /admin/presets/save",
		"POST /admin/presets/reset",
		"POST /admin/presets/upload",
		"GET /admin/presets/download",
		"GET /admin/presets/x/download",
		"POST /admin/presets/x/load",
		"POST /admin/presets/x/update",
		"POST /admin/presets/x/delete",
	}
	h, mux := newAuthTestMux(t)
	setConfigPassword(t, h, "pw")
	for _, route := range routes {
		method, path, _ := strings.Cut(route, " ")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/login" {
			t.Errorf("%s: status = %d, Location = %q, want 303 /admin/login", route, rec.Code, rec.Header().Get("Location"))
		}
	}
}
