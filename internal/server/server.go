// Package server implements the ICAAS conference website: page rendering,
// static assets, the proposal submission / contact / newsletter forms and a
// small password-protected admin view of what was submitted.
package server

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"icaas/internal/content"
	"icaas/internal/store"
)

// Config controls a Server.
type Config struct {
	DataDir       string // where form submissions are stored
	BaseURL       string // canonical origin, e.g. https://icaas.example.org; derived from the request when empty
	Dev           bool   // re-read templates and content on every request
	TrustProxy    bool   // honour X-Forwarded-For / X-Forwarded-Proto
	AdminUser     string // admin pages are disabled unless AdminPassword is set
	AdminPassword string
	Logger        *log.Logger
	Now           func() time.Time // test hook; defaults to time.Now
}

// Server is an http.Handler serving the whole site.
type Server struct {
	cfg      Config
	logger   *log.Logger
	webFS    fs.FS
	loadSite func() (*content.Site, error)
	store    *store.Store
	forms    *rateLimiter

	mu           sync.RWMutex
	cachedSite   *content.Site
	cachedTmpl   templateSet
	assetVersion string

	pages   []*Page
	byPath  map[string]*Page
	nav     []navItem
	handler http.Handler
}

// New builds a Server. webFS must contain templates/ and static/;
// loadSite returns the conference content (called once, or per request in
// dev mode).
func New(cfg Config, webFS fs.FS, loadSite func() (*content.Site, error)) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "icaas ", log.LstdFlags)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		logger:   cfg.Logger,
		webFS:    webFS,
		loadSite: loadSite,
		store:    st,
		forms:    newRateLimiter(12, 10*time.Minute),
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	s.pages, s.nav = sitePages(), siteNav()
	s.byPath = make(map[string]*Page, len(s.pages))
	for _, p := range s.pages {
		if _, dup := s.byPath[p.Path]; dup {
			return nil, fmt.Errorf("duplicate page path %s", p.Path)
		}
		s.byPath[p.Path] = p
	}
	if err := s.checkPages(); err != nil {
		return nil, err
	}
	s.handler = s.routes()
	return s, nil
}

func (s *Server) now() time.Time { return s.cfg.Now() }

func (s *Server) site() *content.Site {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedSite
}

func (s *Server) templates() (templateSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cachedTmpl == nil {
		return nil, errors.New("templates not loaded")
	}
	return s.cachedTmpl, nil
}

// reload (re)reads content, templates and the asset fingerprint.
func (s *Server) reload() error {
	site, err := s.loadSite()
	if err != nil {
		return err
	}
	version, err := assetHash(s.webFS)
	if err != nil {
		return fmt.Errorf("hash static assets: %w", err)
	}
	s.mu.Lock()
	s.cachedSite, s.assetVersion = site, version
	s.mu.Unlock()

	tmpl, err := s.loadTemplates(s.webFS)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cachedTmpl = tmpl
	s.mu.Unlock()
	return nil
}

// checkPages makes sure every registered page has a template, so a typo
// fails at startup instead of on the first visit.
func (s *Server) checkPages() error {
	set, err := s.templates()
	if err != nil {
		return err
	}
	var missing []string
	for _, p := range s.pages {
		if _, ok := set[p.Template]; !ok {
			missing = append(missing, p.Template+" ("+p.Path+")")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing page templates: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(s.webFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", s.staticHandler(static)))
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/img/favicon.ico"
		s.staticHandler(static).ServeHTTP(w, r2)
	})
	mux.HandleFunc("/robots.txt", s.handleRobots)
	mux.HandleFunc("/sitemap.xml", s.handleSitemap)
	mux.HandleFunc("/llms.txt", s.handleLLMs)
	mux.HandleFunc("/manifest.webmanifest", s.handleManifest)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/submission", s.handleSubmission)
	mux.HandleFunc("/submission/received", s.handleSubmissionReceived)
	mux.HandleFunc("/contact", s.handleContact)
	mux.HandleFunc("/subscribe", s.handleSubscribe)
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/export", s.handleAdminExport)
	mux.HandleFunc("/admin/paper", s.handleAdminPaper)
	mux.HandleFunc("/admin/papers.zip", s.handleAdminPapersZip)
	mux.HandleFunc("/", s.handlePage)

	var h http.Handler = mux
	if s.cfg.Dev {
		h = s.devReload(h)
	}
	h = gzipResponses(h)
	h = securityHeaders(h)
	h = recoverPanics(s.logger, h)
	h = logRequests(s.logger, h)
	return h
}

// devReload re-reads templates and content before each page request so
// edits show up on refresh without restarting the server.
func (s *Server) devReload(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			if err := s.reload(); err != nil {
				s.serverError(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handlePage serves every registered content page and the 404 page.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		http.Redirect(w, r, strings.TrimRight(p, "/"), http.StatusMovedPermanently)
		return
	}
	page, ok := s.byPath[p]
	if !ok {
		if to, moved := movedPages[p]; moved {
			http.Redirect(w, r, to, http.StatusMovedPermanently)
			return
		}
		s.notFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.render(w, r, http.StatusOK, page, nil, nil)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, notFoundPage, nil, nil)
}

// staticHandler serves embedded assets. Fingerprinted URLs (?v=...) are
// immutable; directory listings are refused.
func (s *Server) staticHandler(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			s.notFound(w, r)
			return
		}
		if fi, err := fs.Stat(static, name); err != nil || fi.IsDir() {
			s.notFound(w, r)
			return
		}
		switch {
		case s.cfg.Dev:
			w.Header().Set("Cache-Control", "no-cache")
		case r.URL.Query().Get("v") != "":
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		if strings.HasSuffix(name, ".pptx") {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name[strings.LastIndex(name, "/")+1:]+`"`)
		}
		files.ServeHTTP(w, r)
	})
}

// handleRobots lets every crawler, search engines and AI crawlers alike,
// read the whole site. Pages marked NoIndex stay out of search results
// through their robots meta tag and X-Robots-Tag header, which crawlers
// can only see because they are allowed to fetch them.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	base := s.baseURL(r)
	fmt.Fprintf(w, "User-agent: *\nAllow: /\n\nSitemap: %s/sitemap.xml\n\n# Summary for language models: %s/llms.txt\n", base, base)
}

// handleSitemap lists every page of the registry that is not NoIndex, so a
// page added to sitePages appears here without further work.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	type url struct {
		Loc      string `xml:"loc"`
		Priority string `xml:"priority"`
	}
	type urlset struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []url    `xml:"url"`
	}
	base := s.baseURL(r)
	set := urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, p := range s.pages {
		if p.NoIndex {
			continue
		}
		prio := "0.6"
		switch {
		case p.Path == "/":
			prio = "1.0"
		case p.Parent == "/" || p.Parent == "":
			prio = "0.8"
		}
		set.URLs = append(set.URLs, url{Loc: base + p.Path, Priority: prio})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		s.logger.Printf("sitemap: %v", err)
	}
}

// handleLLMs serves /llms.txt (https://llmstxt.org): the headline facts from
// site.json and every page the sitemap lists, grouped by menu section, so a
// language model can read the site at a glance. Like the sitemap it is built
// from the page registry and the content, so it never needs editing by hand.
func (s *Server) handleLLMs(w http.ResponseWriter, r *http.Request) {
	site, base := s.site(), s.baseURL(r)
	c := site.Conference
	orTBA := func(v string) string {
		if strings.TrimSpace(v) == "" {
			return "TBA"
		}
		return v
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s – %s\n\n", c.FullTitle(), c.Name)
	if c.Description != "" {
		fmt.Fprintf(&b, "> %s\n\n", c.Description)
	}
	if c.Tagline != "" {
		fmt.Fprintf(&b, "%s\n\n", c.Tagline)
	}

	venue := c.Location()
	if c.VenueName != "" {
		venue = strings.TrimSuffix(c.VenueName+", "+venue, ", ")
	}
	submission, registration := "closed", "closed"
	if c.SubmissionOpen {
		submission = "open at " + base + "/submission"
	}
	if c.RegistrationOpen && c.RegistrationURL != "" {
		registration = "open at " + c.RegistrationURL
	}
	fmt.Fprintf(&b, "- Website: %s/\n", base)
	fmt.Fprintf(&b, "- Date: %s\n", orTBA(c.Dates))
	fmt.Fprintf(&b, "- Venue: %s\n", orTBA(venue))
	if c.Format != "" {
		fmt.Fprintf(&b, "- Format: %s\n", c.Format)
	}
	if c.Language != "" {
		fmt.Fprintf(&b, "- Language: %s\n", c.Language)
	}
	fmt.Fprintf(&b, "- Proposal submission: %s\n", submission)
	fmt.Fprintf(&b, "- Registration: %s\n", registration)
	for _, ct := range site.Contacts {
		if ct.Email != "" {
			fmt.Fprintf(&b, "- %s: %s\n", ct.Label, ct.Email)
		}
	}

	if len(site.ImportantDates) > 0 {
		b.WriteString("\nImportant dates:\n\n")
		for _, d := range site.ImportantDates {
			fmt.Fprintf(&b, "- %s: %s", d.Label, orTBA(d.Date))
			if d.Original != "" {
				fmt.Fprintf(&b, " (extended from %s)", d.Original)
			}
			if d.Note != "" {
				fmt.Fprintf(&b, " – %s", d.Note)
			}
			b.WriteString("\n")
		}
	}

	// Pages are grouped under the menu section their sidebar shows; pages
	// without a section menu go to "More".
	groups := map[string][]*Page{}
	for _, p := range s.pages {
		if p.NoIndex || p.Home {
			continue
		}
		section := sidebarSections[p.Sidebar]
		groups[section] = append(groups[section], p)
	}
	writeGroup := func(heading string, pages []*Page) {
		if len(pages) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", heading)
		for _, p := range pages {
			fmt.Fprintf(&b, "- [%s](%s%s)", p.Title, base, p.Path)
			if d := p.MetaDescription(); d != "" {
				fmt.Fprintf(&b, ": %s", d)
			}
			b.WriteString("\n")
		}
	}
	for _, item := range s.nav {
		if item.Path != "" {
			writeGroup(item.Label, groups[item.Path])
		}
	}
	writeGroup("More", groups[""])

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	c := s.site().Conference
	w.Header().Set("Content-Type", "application/manifest+json")
	fmt.Fprintf(w, `{"name":%q,"short_name":%q,"start_url":"/","display":"standalone","background_color":"#0a1628","theme_color":"#142c55","icons":[{"src":"/static/img/icon-192.png","sizes":"192x192","type":"image/png"},{"src":"/static/img/icon-512.png","sizes":"512x512","type":"image/png"}]}`,
		c.FullTitle()+" – "+c.Name, c.FullTitle())
}
