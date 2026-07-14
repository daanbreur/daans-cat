package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxCaption = 280

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	p, ok := a.store.Latest()
	if !ok {
		a.render(w, http.StatusOK, "empty", pageData{Title: a.cfg.SiteTitle})
		return
	}
	_, newer, older, _ := a.store.Get(p.ID)
	a.render(w, http.StatusOK, "post", pageData{
		Title: a.cfg.SiteTitle,
		Post:  &p, Newer: newer, Older: older,
	})
}

func (a *App) handlePost(w http.ResponseWriter, r *http.Request) {
	p, newer, older, ok := a.store.Get(r.PathValue("id"))
	if !ok {
		a.handleNotFound(w, r)
		return
	}
	title := a.cfg.SiteTitle
	if p.Caption != "" {
		title = p.Caption + " — " + a.cfg.SiteTitle
	}
	a.render(w, http.StatusOK, "post", pageData{
		Title: title,
		Post:  &p, Newer: newer, Older: older,
	})
}

func (a *App) handleArchive(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "archive", pageData{
		Title: "archive — " + a.cfg.SiteTitle,
		Nav:   "archive",
		Posts: a.store.List(),
	})
}

func (a *App) handleNotFound(w http.ResponseWriter, r *http.Request) {
	a.renderError(w, http.StatusNotFound, "no cat here")
}

var aiCrawlers = []string{
	"GPTBot", "OAI-SearchBot", "ChatGPT-User",
	"ClaudeBot", "anthropic-ai", "Claude-Web",
	"Google-Extended", "Applebot-Extended", "meta-externalagent",
	"CCBot", "Bytespider", "PerplexityBot", "Amazonbot",
}

func (a *App) handleRobots(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	for _, ua := range aiCrawlers {
		fmt.Fprintf(&b, "User-agent: %s\nDisallow: /\n\n", ua)
	}
	b.WriteString("User-agent: *\nAllow: /\n\n")
	fmt.Fprintf(&b, "Sitemap: %s\n", a.absURL("sitemap.xml"))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, b.String())
}

type urlSet struct {
	XMLName xml.Name  `xml:"urlset"`
	NS      string    `xml:"xmlns,attr"`
	URLs    []sitemap `xml:"url"`
}

type sitemap struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

func (a *App) handleSitemap(w http.ResponseWriter, r *http.Request) {
	posts := a.store.List()

	set := urlSet{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs, sitemap{Loc: a.cfg.SiteURL + "/"})
	set.URLs = append(set.URLs, sitemap{Loc: a.absURL("archive")})
	for _, p := range posts {
		set.URLs = append(set.URLs, sitemap{
			Loc:     a.absURL("p", p.ID),
			LastMod: p.Date.Format("2006-01-02"),
		})
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		slog.Error("sitemap encode failed", "err", err)
	}
}

// handleSecurityTxt points at whoever actually handles security reports.
// 301 rather than 302: this is not going to change, and a permanent redirect
// means scanners and researchers cache it instead of coming back here.
func (a *App) handleSecurityTxt(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, a.cfg.SecurityTxt, http.StatusMovedPermanently)
}

// mediaName is deliberately strict: media filenames are ones we generated
// ourselves (24 hex chars), so anything else is someone probing for a path
// traversal and gets a flat 404.
var mediaName = regexp.MustCompile(`^[a-f0-9]{24}(-t)?\.jpg$`)

func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !mediaName.MatchString(name) {
		a.handleNotFound(w, r)
		return
	}
	// Filenames are random and content is never rewritten, so these can be
	// cached forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(a.cfg.DataDir, "media", name))
}

// ---------- admin ----------

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	s, ok := a.currentSession(r)
	if !ok {
		a.render(w, http.StatusOK, "login", pageData{Title: "login — " + a.cfg.SiteTitle})
		return
	}
	flash := ""
	switch r.URL.Query().Get("ok") {
	case "posted":
		flash = "posted."
	case "deleted":
		flash = "deleted."
	case "edited":
		flash = "updated."
	}
	a.render(w, http.StatusOK, "admin", pageData{
		Title: "admin — " + a.cfg.SiteTitle,
		Nav:   "admin",
		CSRF:  s.csrf,
		Posts: a.store.List(),
		Flash: flash,
	})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		a.renderError(w, http.StatusBadRequest, "bad request")
		return
	}

	ip := a.clientIP(r)
	if !a.limiter.allow(ip) {
		slog.Warn("login rate limited", "ip", ip)
		a.render(w, http.StatusTooManyRequests, "login", pageData{
			Title: "login — " + a.cfg.SiteTitle,
			Err:   "too many attempts. wait 15 minutes.",
		})
		return
	}

	if !a.checkPassword(r.PostFormValue("password")) {
		slog.Warn("login failed", "ip", ip)
		a.render(w, http.StatusUnauthorized, "login", pageData{
			Title: "login — " + a.cfg.SiteTitle,
			Err:   "wrong password.",
		})
		return
	}

	a.limiter.succeed(ip)
	token, _ := a.sessions.create()
	a.setSessionCookie(w, token, sessionTTL)
	slog.Info("login ok", "ip", ip)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.destroy(c.Value)
	}
	a.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	// requireAuth already parsed the multipart form under a size cap.
	if r.MultipartForm == nil {
		a.adminError(w, r, "pick a photo first.")
		return
	}
	files := r.MultipartForm.File["photo"]
	if len(files) == 0 {
		a.adminError(w, r, "pick a photo first.")
		return
	}

	f, err := files[0].Open()
	if err != nil {
		a.adminError(w, r, "could not read that file.")
		return
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		a.adminError(w, r, "could not read that file.")
		return
	}
	if len(raw) > maxUploadBytes {
		a.adminError(w, r, "that photo is over 25 MB.")
		return
	}

	caption := strings.TrimSpace(r.PostFormValue("caption"))
	caption = strings.Join(strings.Fields(caption), " ") // collapse newlines/runs
	if utf8.RuneCountInString(caption) > maxCaption {
		a.adminError(w, r, "caption is too long.")
		return
	}

	date := time.Now()
	if v := r.PostFormValue("date"); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			a.adminError(w, r, "that date makes no sense.")
			return
		}
		date = d
	}
	date = time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC)

	img, err := processUpload(a.cfg.DataDir, raw)
	if err != nil {
		a.adminError(w, r, err.Error())
		return
	}

	err = a.store.Add(Post{
		Date:     date,
		Caption:  caption,
		Image:    img.Image,
		Thumb:    img.Thumb,
		Original: img.Original,
		Width:    img.Width,
		Height:   img.Height,
		Bytes:    img.Bytes,
		Created:  time.Now().UTC(),
	})
	if err != nil {
		slog.Error("save failed", "err", err)
		a.adminError(w, r, "could not save the post.")
		return
	}

	http.Redirect(w, r, "/admin?ok=posted", http.StatusSeeOther)
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store.Delete(r.PostFormValue("id")); err != nil {
		a.adminError(w, r, "no such post.")
		return
	}
	http.Redirect(w, r, "/admin?ok=deleted", http.StatusSeeOther)
}

// handleEditForm shows the pre-filled edit page. It is a GET and changes
// nothing, so it needs a session but no CSRF token.
func (a *App) handleEditForm(w http.ResponseWriter, r *http.Request) {
	s, ok := a.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	p, _, _, found := a.store.Get(r.PathValue("id"))
	if !found {
		a.renderError(w, http.StatusNotFound, "no such post")
		return
	}
	a.render(w, http.StatusOK, "edit", pageData{
		Title: "edit — " + a.cfg.SiteTitle,
		Nav:   "admin",
		CSRF:  s.csrf,
		Post:  &p,
	})
}

// handleEdit saves a caption/date change. The photo is not editable here — that
// is what a fresh upload is for — so the image, its dimensions, and the
// preserved original are untouched.
func (a *App) handleEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	p, _, _, ok := a.store.Get(id)
	if !ok {
		a.adminError(w, r, "no such post.")
		return
	}

	caption := strings.TrimSpace(r.PostFormValue("caption"))
	caption = strings.Join(strings.Fields(caption), " ")
	if utf8.RuneCountInString(caption) > maxCaption {
		a.editError(w, r, p, "caption is too long.")
		return
	}

	date := p.Date
	if v := r.PostFormValue("date"); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			a.editError(w, r, p, "that date makes no sense.")
			return
		}
		date = time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC)
	}

	if _, err := a.store.Update(id, caption, date); err != nil {
		a.adminError(w, r, "could not save the changes.")
		return
	}
	http.Redirect(w, r, "/admin?ok=edited", http.StatusSeeOther)
}

// editError re-renders the edit page with the problem shown.
func (a *App) editError(w http.ResponseWriter, r *http.Request, p Post, msg string) {
	s, ok := a.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	a.render(w, http.StatusBadRequest, "edit", pageData{
		Title: "edit — " + a.cfg.SiteTitle,
		Nav:   "admin",
		CSRF:  s.csrf,
		Post:  &p,
		Err:   msg,
	})
}

// adminError re-renders the admin page with the problem shown, rather than
// dumping the user on a dead end.
func (a *App) adminError(w http.ResponseWriter, r *http.Request, msg string) {
	s, ok := a.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	a.render(w, http.StatusBadRequest, "admin", pageData{
		Title: "admin — " + a.cfg.SiteTitle,
		Nav:   "admin",
		CSRF:  s.csrf,
		Posts: a.store.List(),
		Err:   msg,
	})
}
