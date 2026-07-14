package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed templates/*.html static/*
var assets embed.FS

type App struct {
	cfg      Config
	store    *Store
	sessions *sessionStore
	limiter  *limiter
	tmpl     map[string]*template.Template
	assetVer string // content hash of style.css, appended to its URL
}

func NewApp(cfg Config, store *Store) (*App, error) {
	a := &App{
		cfg:      cfg,
		store:    store,
		sessions: newSessionStore(),
		limiter:  newLimiter(),
		tmpl:     map[string]*template.Template{},
	}

	// Stamp the stylesheet's URL with a hash of its contents. Without this, a
	// CSS change is invisible to anyone who loaded the old one until their
	// cache expires — including you, right after a deploy.
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(css)
	a.assetVer = hex.EncodeToString(sum[:4])

	funcs := template.FuncMap{
		"longDate":  func(t time.Time) string { return t.Format("January 2, 2006") },
		"shortDate": func(t time.Time) string { return t.Format("2006-01-02") },
	}
	for _, name := range []string{"post", "archive", "empty", "login", "admin", "edit", "error"} {
		t, err := template.New("base.html").Funcs(funcs).
			ParseFS(assets, "templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		a.tmpl[name] = t
	}
	return a, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /p/{id}", a.handlePost)
	mux.HandleFunc("GET /archive", a.handleArchive)
	mux.HandleFunc("GET /rss.xml", a.handleRSS)
	mux.HandleFunc("GET /media/{name}", a.handleMedia)
	mux.HandleFunc("GET /robots.txt", a.handleRobots)
	mux.HandleFunc("GET /sitemap.xml", a.handleSitemap)

	// RFC 9116 puts it under /.well-known/; the bare path is the legacy
	// location that scanners still try. Both hand off to the real one.
	if a.cfg.SecurityTxt != "" {
		mux.HandleFunc("GET /security.txt", a.handleSecurityTxt)
		mux.HandleFunc("GET /.well-known/security.txt", a.handleSecurityTxt)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /static/style.css", a.serveAsset("static/style.css"))
	mux.HandleFunc("GET /static/cat.svg", a.serveAsset("static/cat.svg"))

	mux.HandleFunc("GET /admin", a.handleAdmin)
	mux.HandleFunc("GET /admin/edit/{id}", a.handleEditForm)
	mux.HandleFunc("POST /admin/login", a.handleLogin)
	mux.HandleFunc("POST /admin/logout", a.requireAuth(a.handleLogout))
	mux.HandleFunc("POST /admin/upload", a.requireAuth(a.handleUpload))
	mux.HandleFunc("POST /admin/edit", a.requireAuth(a.handleEdit))
	mux.HandleFunc("POST /admin/delete", a.requireAuth(a.handleDelete))

	mux.HandleFunc("/", a.handleNotFound)

	return a.securityHeaders(a.logRequests(mux))
}

func (a *App) serveAsset(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Long-lived cache because the style.css has the URL stamped with a hash of the contents, so it changes when the file does anyways.
		// TODO: Might also wanna do that for the favicon at some point but fuck it for now.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, assets, name)
	}
}

// securityHeaders locks the page down to exactly what this site uses: its own
// stylesheet and its own images. No scripts anywhere, no framing, no
// third-party anything — so even a caption that somehow smuggled markup past
// the escaper would have nothing to execute.
func (a *App) securityHeaders(h http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'none'",
		"img-src 'self'",
		"style-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", csp)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		hdr.Set("Cross-Origin-Resource-Policy", "same-site")
		hdr.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), interest-cohort=()")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			hdr.Set("X-Robots-Tag", "noindex, nofollow")
			hdr.Set("Cache-Control", "no-store")
		}
		if a.cfg.SecureCookie {
			hdr.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		h.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) {
	s.status = c
	s.ResponseWriter.WriteHeader(c)
}

func (a *App) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)

		// Kubernetes probes /healthz every few seconds from two probes. Logging that buries everything else quite quickly.
		// TODO: Think about this more, could be used by someone to DDOS the server without it being logged. Good enough for now.
		if r.URL.Path == "/healthz" && sw.status == http.StatusOK {
			return
		}

		slog.Info("req",
			"method", r.Method,
			"path", r.URL.Path,
			"ip", a.clientIP(r),
			"status", sw.status,
			"ms", time.Since(start).Milliseconds(),
		)
	})
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		// Cap the body before touching the form: parsing is what allocates,
		// so the limit has to be in place first. The slack above the photo
		// limit covers the multipart envelope and the other fields.
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
		// Keep the whole upload in memory: anything above this spills to
		// os.TempDir(), and the container's filesystem is read-only. Only an
		// authenticated admin ever gets this far, so the ceiling is bounded.
		if err := r.ParseMultipartForm(maxUploadBytes + (1 << 20)); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				a.renderError(w, http.StatusRequestEntityTooLarge, "that photo is too big — 25 MB max")
				return
			}
			a.renderError(w, http.StatusBadRequest, "could not read that form")
			return
		}

		// Every state-changing request carries the session's CSRF token in the
		// form body; SameSite=Strict is the belt, this is the braces.
		if !checkCSRF(s, r.PostFormValue("csrf")) {
			a.renderError(w, http.StatusForbidden, "bad csrf token — reload /admin and try again")
			return
		}
		next(w, r)
	}
}

type pageData struct {
	Site    Config
	Asset   string
	Title   string
	Nav     string
	Post    *Post
	Newer   *Post
	Older   *Post
	Posts   []Post
	CSRF    string
	Flash   string
	Err     string
	Message string
}

func (a *App) render(w http.ResponseWriter, status int, name string, d pageData) {
	d.Site = a.cfg
	d.Asset = a.assetVer
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.tmpl[name].ExecuteTemplate(w, "base.html", d); err != nil {
		slog.Error("render failed", "template", name, "err", err)
	}
}

func (a *App) renderError(w http.ResponseWriter, status int, msg string) {
	a.render(w, status, "error", pageData{
		Title:   fmt.Sprintf("%d", status),
		Message: msg,
	})
}

func (a *App) absURL(parts ...string) string {
	return a.cfg.SiteURL + path.Join("/", path.Join(parts...))
}
