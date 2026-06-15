package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gopkg.in/yaml.v3"

	"github.com/barry/AzureDNSForwarder/internal/audit"
	"github.com/barry/AzureDNSForwarder/internal/config"
	peersync "github.com/barry/AzureDNSForwarder/internal/sync"
	"github.com/barry/AzureDNSForwarder/internal/unbound"
	"github.com/barry/AzureDNSForwarder/internal/watcher"
)

type Server struct {
	store    *config.Store
	watcher  *watcher.Watcher
	audit    *audit.Log
	logger   *slog.Logger
	instance string
	pages    map[string]*template.Template
	staticFS fs.FS
}

func New(store *config.Store, w *watcher.Watcher, a *audit.Log, logger *slog.Logger, instance string, assets fs.FS) (*Server, error) {
	staticFS, err := fs.Sub(assets, "web/static")
	if err != nil {
		return nil, err
	}
	tplFS, err := fs.Sub(assets, "web/templates")
	if err != nil {
		return nil, err
	}
	pages := map[string][]string{
		"dashboard.html":     {"base.html", "dashboard.html"},
		"forwarders.html":    {"base.html", "forwarders.html"},
		"records.html":       {"base.html", "records.html"},
		"settings.html":      {"base.html", "settings.html"},
		"sync_partners.html": {"base.html", "sync_partners.html"},
		"import_export.html": {"base.html", "import_export.html"},
		"audit.html":         {"base.html", "audit.html"},
	}
	parsed := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		// Each page is parsed with base.html so its `content` block overrides
		// base.html's default empty block. ParseFS returns templates named
		// after the file basename, so a second parse of a file with the
		// same basename into the same set would clobber the prior template
		// — that's why we keep page+base as its own template set.
		parsed[name], err = template.ParseFS(tplFS, files...)
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		store:    store,
		watcher:  w,
		audit:    a,
		logger:   logger,
		instance: instance,
		pages:    parsed,
		staticFS: staticFS,
	}, nil
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))

	r.Get("/", s.handleDashboard)
	r.Get("/forwarders", s.handleForwardersPage)
	r.Get("/records", s.handleRecordsPage)
	r.Get("/settings", s.handleSettingsPage)
	r.Get("/sync-partners", s.handleSyncPartnersPage)
	r.Get("/import-export", s.handleImportExportPage)
	r.Get("/audit", s.handleAuditPage)

	r.Route("/api", func(r chi.Router) {
		r.Get("/status", s.apiStatus)
		r.Get("/config", s.apiGetConfig)
		r.Put("/config", s.apiPutConfig)

		r.Get("/forwarders", s.apiListForwarders)
		r.Post("/forwarders", s.apiAddForwarder)
		r.Put("/forwarders/{id}", s.apiUpdateForwarder)
		r.Delete("/forwarders/{id}", s.apiDeleteForwarder)

		r.Get("/defaults", s.apiListDefaults)
		r.Post("/defaults", s.apiAddDefault)
		r.Patch("/defaults/{addr}/{port}", s.apiUpdateDefault)
		r.Delete("/defaults/{addr}/{port}", s.apiDeleteDefault)

		r.Get("/records", s.apiListRecords)
		r.Post("/records", s.apiAddRecord)
		r.Put("/records/{id}", s.apiUpdateRecord)
		r.Delete("/records/{id}", s.apiDeleteRecord)

		r.Get("/settings", s.apiGetSettings)
		r.Put("/settings", s.apiPutSettings)

		r.Get("/peers", s.apiListPeers)
		r.Post("/peers", s.apiAddPeer)
		r.Put("/peers/{name}", s.apiUpdatePeer)
		r.Delete("/peers/{name}", s.apiDeletePeer)
		r.Post("/peers/{name}/sync", s.apiSyncPeer)
		r.Get("/peers/status", s.apiPeersStatus)

		r.Get("/export", s.apiExport)
		r.Post("/import", s.apiImport)
		r.Get("/audit", s.apiAudit)
		r.Post("/test", s.apiTest)
		r.Get("/healthz", s.apiHealthz)
	})
	return r
}

// ----- page rendering -----

type pageData struct {
	Title    string
	Instance string
	Config   *config.File
	Status   statusView
	Active   string
}

type statusView struct {
	LastOK  time.Time `json:"last_ok"`
	LastErr string    `json:"last_error"`
	Hash    string    `json:"hash"`
	Version int       `json:"version"`
}

func (s *Server) statusView() statusView {
	cur := s.store.Current()
	lastOK, lastErr := s.watcher.Status()
	hash := s.store.Hash()
	ver := 0
	if cur != nil {
		ver = cur.Version
	}
	return statusView{LastOK: lastOK, LastErr: lastErr, Hash: hash, Version: ver}
}

func (s *Server) render(w http.ResponseWriter, name string, pd pageData) {
	if pd.Instance == "" {
		pd.Instance = s.instance
	}
	if pd.Status.Hash == "" && s.store.Hash() != "" {
		pd.Status = s.statusView()
	}
	if pd.Config == nil {
		cur := s.store.Current()
		if cur != nil {
			cp := cur.Snapshot()
			pd.Config = &cp
		}
	}
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template for "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, pd); err != nil {
		s.logger.Error("template", "err", err)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard.html", pageData{Title: "Dashboard", Active: "dashboard"})
}
func (s *Server) handleForwardersPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "forwarders.html", pageData{Title: "Forwarders", Active: "forwarders"})
}
func (s *Server) handleRecordsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "records.html", pageData{Title: "Static Records", Active: "records"})
}
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings.html", pageData{Title: "Settings", Active: "settings"})
}
func (s *Server) handleImportExportPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "import_export.html", pageData{Title: "Import / Export", Active: "import"})
}
func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "audit.html", pageData{Title: "Audit", Active: "audit"})
}
func (s *Server) handleSyncPartnersPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "sync_partners.html", pageData{Title: "Sync Partners", Active: "sync"})
}

// ----- API -----

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	lastOK, lastErr := s.watcher.Status()
	hash := s.store.Hash()
	ver := 0
	var updated time.Time
	var updatedBy string
	if cur != nil {
		ver = cur.Version
		updated = cur.Updated
		updatedBy = cur.UpdatedBy
	}
	writeJSON(w, map[string]any{
		"instance":   s.instance,
		"hash":       hash,
		"last_ok":    lastOK,
		"last_error": lastErr,
		"version":    ver,
		"updated":    updated,
		"updated_by": updatedBy,
	})
}

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config loaded", http.StatusServiceUnavailable)
		return
	}
	snap := cur.Snapshot()
	writeJSON(w, snap)
}

func (s *Server) apiPutConfig(w http.ResponseWriter, r *http.Request) {
	var body config.File
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			f.Settings = body.Settings
			f.Settings = f.Settings.WithDefaults()
			f.UpstreamDefaults = body.UpstreamDefaults
			f.ForwardZones = body.ForwardZones
			f.StaticRecords = body.StaticRecords
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "config-replace"})
	s.afterSave(w, r, "config-replace")
}

func (s *Server) apiListForwarders(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	zones := cur.Snapshot().ForwardZones
	out := make([]config.ForwardZone, 0, len(zones))
	for _, z := range zones {
		if !z.Deleted {
			out = append(out, z)
		}
	}
	writeJSON(w, out)
}

type forwarderBody struct {
	Name                 string   `json:"name"`
	Wildcard             bool     `json:"wildcard"`
	Upstreams            []string `json:"upstreams"`
	LeastLatency         bool     `json:"least_latency"`
	LatencyTestFrequency int      `json:"latency_test_frequency"`
	DoNotSync            bool     `json:"do_not_sync"`
	SyncPeers            []string `json:"sync_peers,omitempty"`
}

func (s *Server) apiAddForwarder(w http.ResponseWriter, r *http.Request) {
	var b forwarderBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	z := config.ForwardZone{
		ID:                   newID(),
		Name:                 b.Name,
		Wildcard:             b.Wildcard,
		Upstreams:            b.Upstreams,
		LeastLatency:         b.LeastLatency,
		LatencyTestFrequency: b.LatencyTestFrequency,
		DoNotSync:            b.DoNotSync,
		SyncPeers:            normalizePeerList(b.SyncPeers),
	}
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			f.ForwardZones = append(f.ForwardZones, z)
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "forwarder-add", Details: z.Name})
	s.afterSave(w, r, "forwarder-add")
}

func (s *Server) apiUpdateForwarder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var b forwarderBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	now := time.Now().UTC()
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.ForwardZones {
				if f.ForwardZones[i].ID == id {
					f.ForwardZones[i].Name = b.Name
					f.ForwardZones[i].Wildcard = b.Wildcard
					f.ForwardZones[i].Upstreams = b.Upstreams
					f.ForwardZones[i].LeastLatency = b.LeastLatency
					f.ForwardZones[i].LatencyTestFrequency = b.LatencyTestFrequency
					f.ForwardZones[i].DoNotSync = b.DoNotSync
					f.ForwardZones[i].SyncPeers = normalizePeerList(b.SyncPeers)
					if b.DoNotSync {
						f.ForwardZones[i].SyncPeers = nil
					}
					f.ForwardZones[i].UpdatedAt = now
					f.ForwardZones[i].UpdatedBy = actor
					return nil
				}
			}
			return fmt.Errorf("forwarder %s not found", id)
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "forwarder-update", Details: id})
	s.afterSave(w, r, "forwarder-update")
}

func (s *Server) apiDeleteForwarder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := actorOf(r, "api")
	now := time.Now().UTC()

	// First pass: tombstone the item and persist so peer sync propagates the delete.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.ForwardZones {
				if f.ForwardZones[i].ID == id {
					f.ForwardZones[i].Deleted = true
					f.ForwardZones[i].UpdatedAt = now
					f.ForwardZones[i].UpdatedBy = actor
					return nil
				}
			}
			return fmt.Errorf("forwarder %s not found", id)
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Second pass: hard-delete the local tombstone so the same zone can be
	// re-added immediately without triggering a duplicate validation error.
	// Peers already received the tombstone from the save above.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.ForwardZones {
				if f.ForwardZones[i].ID == id {
					f.ForwardZones = append(f.ForwardZones[:i], f.ForwardZones[i+1:]...)
					return nil
				}
			}
			return nil
		},
	}); err != nil {
		s.logger.Warn("forwarder hard-delete failed", "id", id, "err", err)
	}

	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "forwarder-delete", Details: id})
	// Do not call afterSave again; the first tombstone save already triggered
	// the on-save/on-apply hooks and applied unbound. We write the success
	// response directly so we don't fire a duplicate push or reload.
	writeJSON(w, map[string]any{"ok": true, "reason": "forwarder-delete"})
}

func (s *Server) apiListDefaults(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	ups := cur.Snapshot().UpstreamDefaults
	out := make([]config.UpstreamDefault, 0, len(ups))
	for _, u := range ups {
		if !u.Deleted {
			out = append(out, u)
		}
	}
	writeJSON(w, out)
}

type defaultBody struct {
	Address   string   `json:"address"`
	Port      int      `json:"port"`
	Enabled   *bool    `json:"enabled,omitempty"`
	Note      string   `json:"note"`
	DoNotSync bool     `json:"do_not_sync"`
	SyncPeers []string `json:"sync_peers,omitempty"`
}

func (s *Server) apiAddDefault(w http.ResponseWriter, r *http.Request) {
	var b defaultBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	u := config.UpstreamDefault{Address: b.Address, Port: b.Port, Note: b.Note, DoNotSync: b.DoNotSync, SyncPeers: normalizePeerList(b.SyncPeers)}
	if b.Enabled != nil {
		u.Enabled = *b.Enabled
	} else {
		u.Enabled = true
	}
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for _, existing := range f.UpstreamDefaults {
				if existing.Address == u.Address && existing.Port == u.Port {
					return fmt.Errorf("upstream %s:%d already exists", u.Address, u.Port)
				}
			}
			f.UpstreamDefaults = append(f.UpstreamDefaults, u)
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "default-add", Details: u.String()})
	s.afterSave(w, r, "default-add")
}

func (s *Server) apiUpdateDefault(w http.ResponseWriter, r *http.Request) {
	addr := chi.URLParam(r, "addr")
	portStr := chi.URLParam(r, "port")
	port, _ := strconv.Atoi(portStr)
	var b defaultBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	now := time.Now().UTC()
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			found := false
			for i := range f.UpstreamDefaults {
				if f.UpstreamDefaults[i].Address == addr && f.UpstreamDefaults[i].Port == port {
					if b.Enabled != nil {
						f.UpstreamDefaults[i].Enabled = *b.Enabled
					}
					if b.Note != "" {
						f.UpstreamDefaults[i].Note = b.Note
					}
					if b.Address != "" {
						f.UpstreamDefaults[i].Address = b.Address
					}
					if b.Port != 0 {
						f.UpstreamDefaults[i].Port = b.Port
					}
					// DoNotSync is a real bool; we always apply the
					// incoming value (true OR false) since the GUI
					// checkbox sends its current state.
					f.UpstreamDefaults[i].DoNotSync = b.DoNotSync
					f.UpstreamDefaults[i].SyncPeers = normalizePeerList(b.SyncPeers)
					if b.DoNotSync {
						f.UpstreamDefaults[i].SyncPeers = nil
					}
					f.UpstreamDefaults[i].UpdatedAt = now
					f.UpstreamDefaults[i].UpdatedBy = actor
					found = true
				}
			}
			if !found {
				return fmt.Errorf("upstream %s:%d not found", addr, port)
			}
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "default-update", Details: fmt.Sprintf("%s:%d peers=%v", addr, port, b.SyncPeers)})
	s.afterSave(w, r, "default-update")
}

func (s *Server) apiDeleteDefault(w http.ResponseWriter, r *http.Request) {
	addr := chi.URLParam(r, "addr")
	portStr := chi.URLParam(r, "port")
	port, _ := strconv.Atoi(portStr)
	actor := actorOf(r, "api")
	now := time.Now().UTC()

	// First pass: tombstone and persist so peer sync propagates the delete.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.UpstreamDefaults {
				if f.UpstreamDefaults[i].Address == addr && f.UpstreamDefaults[i].Port == port {
					f.UpstreamDefaults[i].Deleted = true
					f.UpstreamDefaults[i].Enabled = false
					f.UpstreamDefaults[i].UpdatedAt = now
					f.UpstreamDefaults[i].UpdatedBy = actor
					return nil
				}
			}
			return fmt.Errorf("upstream %s:%d not found", addr, port)
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Second pass: hard-delete the local tombstone so the same default can be
	// re-added immediately. Peers already received the tombstone above.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.UpstreamDefaults {
				if f.UpstreamDefaults[i].Address == addr && f.UpstreamDefaults[i].Port == port {
					f.UpstreamDefaults = append(f.UpstreamDefaults[:i], f.UpstreamDefaults[i+1:]...)
					return nil
				}
			}
			return nil
		},
	}); err != nil {
		s.logger.Warn("default hard-delete failed", "addr", addr, "port", port, "err", err)
	}

	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "default-delete", Details: fmt.Sprintf("%s:%d", addr, port)})
	writeJSON(w, map[string]any{"ok": true, "reason": "default-delete"})
}

func (s *Server) apiListRecords(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	recs := cur.Snapshot().StaticRecords
	out := make([]config.StaticRecord, 0, len(recs))
	for _, rec := range recs {
		if !rec.Deleted {
			out = append(out, rec)
		}
	}
	writeJSON(w, out)
}

type recordBody struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Value     string   `json:"value"`
	TTL       int      `json:"ttl"`
	DoNotSync bool     `json:"do_not_sync"`
	SyncPeers []string `json:"sync_peers,omitempty"`
}

func (s *Server) apiAddRecord(w http.ResponseWriter, r *http.Request) {
	var b recordBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	rec := config.StaticRecord{
		ID:        newID(),
		Name:      b.Name,
		Type:      strings.ToUpper(b.Type),
		Value:     b.Value,
		TTL:       b.TTL,
		DoNotSync: b.DoNotSync,
		SyncPeers: normalizePeerList(b.SyncPeers),
	}
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			f.StaticRecords = append(f.StaticRecords, rec)
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "record-add", Details: rec.Name + " " + rec.Type})
	s.afterSave(w, r, "record-add")
}

func (s *Server) apiUpdateRecord(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var b recordBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	now := time.Now().UTC()
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.StaticRecords {
				if f.StaticRecords[i].ID == id {
					f.StaticRecords[i].Name = b.Name
					f.StaticRecords[i].Type = strings.ToUpper(b.Type)
					f.StaticRecords[i].Value = b.Value
					f.StaticRecords[i].TTL = b.TTL
					f.StaticRecords[i].DoNotSync = b.DoNotSync
					f.StaticRecords[i].SyncPeers = normalizePeerList(b.SyncPeers)
					if b.DoNotSync {
						f.StaticRecords[i].SyncPeers = nil
					}
					f.StaticRecords[i].UpdatedAt = now
					f.StaticRecords[i].UpdatedBy = actor
					return nil
				}
			}
			return fmt.Errorf("record %s not found", id)
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "record-update", Details: id})
	s.afterSave(w, r, "record-update")
}

func (s *Server) apiDeleteRecord(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := actorOf(r, "api")
	now := time.Now().UTC()

	// First pass: tombstone and persist so peer sync propagates the delete.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.StaticRecords {
				if f.StaticRecords[i].ID == id {
					f.StaticRecords[i].Deleted = true
					f.StaticRecords[i].UpdatedAt = now
					f.StaticRecords[i].UpdatedBy = actor
					return nil
				}
			}
			return fmt.Errorf("record %s not found", id)
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Second pass: hard-delete the local tombstone so the same record can be
	// re-added immediately. Peers already received the tombstone above.
	if _, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.StaticRecords {
				if f.StaticRecords[i].ID == id {
					f.StaticRecords = append(f.StaticRecords[:i], f.StaticRecords[i+1:]...)
					return nil
				}
			}
			return nil
		},
	}); err != nil {
		s.logger.Warn("record hard-delete failed", "id", id, "err", err)
	}

	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "record-delete", Details: id})
	writeJSON(w, map[string]any{"ok": true, "reason": "record-delete"})
}

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, cur.Snapshot().Settings)
}

func (s *Server) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var b config.Settings
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			f.Settings = b
			f.Settings = f.Settings.WithDefaults()
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "settings-update", Details: audit.String(b)})
	s.afterSave(w, r, "settings-update")
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	snap := cur.Snapshot()
	includeSecrets := r.URL.Query().Get("secrets") == "1"
	if !includeSecrets {
		snap.Peers.SharedKey = ""
	}
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="dnsforwarder.yaml"`)
	if includeSecrets {
		_, _ = w.Write([]byte("# WARNING: This export contains the peer preshared key and other sensitive data.\n" +
			"# Treat the file as confidential. Do NOT commit to source control.\n"))
	} else {
		_, _ = w.Write([]byte("# NOTE: Sensitive values (peer preshared key) have been redacted.\n" +
			"# Re-importing this file on another instance will require that you set\n" +
			"# peers.shared_key (or PEER_SHARED_KEY / PEER_SHARED_KEY_FILE) there.\n" +
			"# To export with secrets included, append ?secrets=1 to the request.\n"))
	}
	_ = yaml.NewEncoder(w).Encode(snap)
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body config.File
	if err := yaml.Unmarshal(data, &body); err != nil {
		http.Error(w, "yaml parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "import")
	_, err = s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			f.Settings = body.Settings
			f.Settings = f.Settings.WithDefaults()
			f.UpstreamDefaults = body.UpstreamDefaults
			f.ForwardZones = body.ForwardZones
			f.StaticRecords = body.StaticRecords
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "config-import"})
	s.afterSave(w, r, "import")
}

type testBody struct {
	Name     string `json:"name"`
	Qtype    string `json:"qtype"`
	Upstream string `json:"upstream"`
}

func (s *Server) apiTest(w http.ResponseWriter, r *http.Request) {
	var b testBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if b.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if b.Qtype == "" {
		b.Qtype = "A"
	}
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	// Default: query our own unbound (127.0.0.1:<DNSListen port>) so the
	// admin sees exactly what their forwarder would return — local records,
	// forwarded zones, the whole chain.
	upstream := b.Upstream
	if upstream == "" {
		host, port := "127.0.0.1", 53
		if cur.Settings.DNSListen != "" {
			h, p, err := net.SplitHostPort(cur.Settings.DNSListen)
			if err == nil {
				host = h
				fmt.Sscanf(p, "%d", &port)
			}
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		upstream = fmt.Sprintf("%s:%d", host, port)
	}
	res, err := unbound.TestResolve(r.Context(), b.Name, b.Qtype, upstream)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, res)
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.audit.Tail(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entries)
}

func (s *Server) apiHealthz(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	_, lastErr := s.watcher.Status()
	if lastErr != "" {
		http.Error(w, "config error: "+lastErr, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) afterSave(w http.ResponseWriter, r *http.Request, reason string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.watcher.ApplyNow(ctx); err != nil {
		s.logger.Warn("apply after save", "err", err)
		http.Error(w, "saved but reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "reason": reason})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func actorOf(r *http.Request, fallback string) string {
	if a := r.Header.Get("X-Actor"); a != "" {
		return a
	}
	return fallback
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ----- Sync Partners (peers) API -----

func (s *Server) apiListPeers(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	snap := cur.Snapshot()
	// Always mask the shared key in API responses.
	snap.Peers.SharedKey = ""
	writeJSON(w, snap.Peers)
}

type peerBody struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (s *Server) apiAddPeer(w http.ResponseWriter, r *http.Request) {
	var b peerBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, url, err := normalizePeer(b.Name, b.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	actor := actorOf(r, "api")
	_, err = s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for _, p := range f.Peers.List {
				if p.Name == name {
					return fmt.Errorf("peer %q already exists", name)
				}
			}
			f.Peers.List = append(f.Peers.List, config.Peer{Name: name, URL: url, Enabled: enabled})
			return nil
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "peer-add", Details: audit.String(map[string]any{"name": name, "url": url})})
	writeJSON(w, map[string]any{"ok": true, "name": name})
}

func (s *Server) apiUpdatePeer(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var b peerBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	actor := actorOf(r, "api")
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i := range f.Peers.List {
				if f.Peers.List[i].Name == name {
					if b.URL != "" {
						_, _, err := normalizePeer(name, b.URL)
						if err != nil {
							return err
						}
						f.Peers.List[i].URL = b.URL
					}
					if b.Enabled != nil {
						f.Peers.List[i].Enabled = *b.Enabled
					}
					return nil
				}
			}
			return fmt.Errorf("peer %q not found", name)
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "peer-update", Details: name})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiDeletePeer(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	actor := actorOf(r, "api")
	_, err := s.store.Save(config.SaveOptions{
		Actor: actor,
		Mutate: func(f *config.File) error {
			for i, p := range f.Peers.List {
				if p.Name == name {
					f.Peers.List = append(f.Peers.List[:i], f.Peers.List[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("peer %q not found", name)
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = s.audit.Write(audit.Entry{Actor: actor, Action: "peer-delete", Details: name})
	writeJSON(w, map[string]any{"ok": true})
}

// apiSyncPeer is a stub: the actual sync engine is a future step. For now
// it just touches the last_contact time and reports "not implemented" so
// the UI can wire the button up without errors.
func (s *Server) apiSyncPeer(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	for _, p := range cur.Peers.List {
		if p.Name == name {
			_ = s.audit.Write(audit.Entry{Actor: actorOf(r, "api"), Action: "peer-sync-request", Details: name})
			writeJSON(w, map[string]any{
				"ok":      true,
				"message": "sync engine not yet enabled in this build (peer CRUD is wired up; networking will follow)",
			})
			return
		}
	}
	http.Error(w, "peer not found", http.StatusNotFound)
}

// apiPeersStatus returns the live status of every configured peer. The
// Status field on each peer is populated from the in-memory peer status
// map maintained by the sync engine; before the engine has had any
// traffic, the values are zero.
func (s *Server) apiPeersStatus(w http.ResponseWriter, r *http.Request) {
	cur := s.store.Current()
	if cur == nil {
		http.Error(w, "no config", http.StatusServiceUnavailable)
		return
	}
	type row struct {
		Name      string            `json:"name"`
		URL       string            `json:"url"`
		Enabled   bool              `json:"enabled"`
		Status    config.PeerStatus `json:"status"`
		HasPSK    bool              `json:"has_psk"`
		PSKLength int               `json:"psk_length"`
	}
	rows := make([]row, 0, len(cur.Peers.List))
	psk := cur.Peers.SharedKey
	for _, p := range cur.Peers.List {
		st := peersync.GetPeerStatus(p.Name)
		// If the YAML has a stored LastContact and the in-memory one is
		// older, prefer the in-memory one (which is fresher). If the
		// YAML has one and the in-memory one is zero, use the YAML.
		ps := p.Status
		if !st.At.IsZero() {
			ps.LastContact = st.At
			ps.LastError = st.LastError
			ps.LastAction = st.LastAction
		}
		rows = append(rows, row{
			Name:      p.Name,
			URL:       p.URL,
			Enabled:   p.Enabled,
			Status:    ps,
			HasPSK:    psk != "",
			PSKLength: len(psk),
		})
	}
	writeJSON(w, map[string]any{
		"instance":     s.instance,
		"listen":       cur.Peers.Listen,
		"sync_seconds": cur.Peers.SyncIntervalSeconds,
		"peers":        rows,
	})
}

func normalizePeerList(peers []string) []string {
	out := make([]string, 0, len(peers))
	seen := map[string]bool{}
	for _, p := range peers {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

func normalizePeer(name, url string) (string, string, error) {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if url == "" {
		return "", "", errors.New("url is required")
	}
	if !strings.HasPrefix(url, "https://") {
		return "", "", errors.New("peer url must use https://")
	}
	if name == "" {
		name = config.PeerNameFromURL(url)
	}
	return name, url, nil
}
