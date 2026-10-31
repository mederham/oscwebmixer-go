package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

const sessionCookie = "oscwebmixer_admin"

// loginFailDelay slows down password guessing; tests set it to 0.
var loginFailDelay = time.Second

// password returns the effective admin password: the env value wins over
// global.json. Empty means auth is off.
func (h *Handler) password() (string, error) {
	if h.envPassword != "" {
		return h.envPassword, nil
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		return "", err
	}
	return global.AdminPassword, nil
}

func (h *Handler) sessionToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	_, ok := h.sessions[c.Value]
	return c.Value, ok
}

func (h *Handler) startSession(w http.ResponseWriter, keepOnly bool) string {
	b := make([]byte, 32)
	rand.Read(b) // never fails since Go 1.24
	token := hex.EncodeToString(b)
	h.sessMu.Lock()
	if keepOnly {
		clear(h.sessions)
	}
	h.sessions[token] = struct{}{}
	h.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return token
}

func (h *Handler) guard(fn http.HandlerFunc) http.HandlerFunc {
	return noStore(func(w http.ResponseWriter, r *http.Request) {
		pw, err := h.password()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, ok := h.sessionToken(r); pw == "" || ok {
			fn(w, r)
			return
		}
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/admin/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	pw, err := h.password()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, ok := h.sessionToken(r); pw == "" || ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, http.StatusOK, "")
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	pw, err := h.password()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pw == "" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(pw)) == 1 {
		h.startSession(w, false)
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	time.Sleep(loginFailDelay)
	h.renderLogin(w, http.StatusUnauthorized, "Wrong password")
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := h.sessionToken(r); ok {
		h.sessMu.Lock()
		delete(h.sessions, token)
		h.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/admin", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *Handler) renderLogin(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.tmpl.ExecuteTemplate(w, "login", struct{ Error string }{msg}); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func (h *Handler) handlePasswordUpdate(w http.ResponseWriter, r *http.Request) {
	if h.envPassword != "" {
		http.Error(w, "admin password is set via OSCWEBMIXER_ADMIN_PASSWORD", http.StatusForbidden)
		return
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := PasswordSectionData{Set: global.AdminPassword != ""}
	switch r.FormValue("action") {
	case "set":
		pw := r.FormValue("password")
		if pw == "" {
			data.Error = "Password can't be empty"
		} else if pw != r.FormValue("confirm") {
			data.Error = "Passwords don't match"
		}
		if data.Error != "" {
			h.renderPasswordSection(w, data)
			return
		}
		global.AdminPassword = pw
		data.Message = "Password saved"
	case "remove":
		global.AdminPassword = ""
		data.Message = "Password removed"
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Set = global.AdminPassword != ""

	// Drop every other session; keep (or, from an open admin, start) the
	// current one so the user isn't bounced to the login page.
	token, ok := h.sessionToken(r)
	switch {
	case !data.Set:
		h.sessMu.Lock()
		clear(h.sessions)
		h.sessMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/admin", MaxAge: -1})
	case ok:
		h.sessMu.Lock()
		clear(h.sessions)
		h.sessions[token] = struct{}{}
		h.sessMu.Unlock()
	default:
		h.startSession(w, true)
	}
	h.renderPasswordSection(w, data)
}

func (h *Handler) renderPasswordSection(w http.ResponseWriter, data PasswordSectionData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "password-section", data); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
