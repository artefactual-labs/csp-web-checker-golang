package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"embed"
	"io/fs"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Profile struct {
	ID         int64
	Name       string
	ConfigJSON string
	CreatedAt  string
}

type Run struct {
	ID          int64
	ProfileID   sql.NullInt64
	CreatedAt   string
	URLsText    string
	SummaryJSON string
	ResultsJSON string
	ExitCode    int
	ElapsedMs   int64
}

type Report struct {
	GeneratedAt string                 `json:"generatedAt"`
	Config      map[string]any         `json:"config"`
	Totals      ReportTotals           `json:"totals"`
	Results     []ReportPageResult     `json:"results"`
	BaseURL     string                 `json:"baseUrl"`
}

type ReportTotals struct {
	Pages      int `json:"pages"`
	Violations int `json:"violations"`
}

type ReportPageResult struct {
	URL        string       `json:"url"`
	Status     *int         `json:"status"`
	OK         bool         `json:"ok"`
	Error      string       `json:"error"`
	DurationMs int64        `json:"durationMs"`
	Violations []Violation  `json:"violations"`
}

type Violation struct {
	DocumentURI       string `json:"documentURI"`
	Referrer          string `json:"referrer"`
	BlockedURI        string `json:"blockedURI"`
	BlockedOrigin     string `json:"blockedOrigin"`
	EffectiveDirective string `json:"effectiveDirective"`
	ViolatedDirective string `json:"violatedDirective"`
	OriginalPolicy    string `json:"originalPolicy"`
	Disposition       string `json:"disposition"`
	StatusCode        *int   `json:"statusCode"`
	SourceFile        string `json:"sourceFile"`
	LineNumber        *int   `json:"lineNumber"`
	ColumnNumber      *int   `json:"columnNumber"`
	Sample            string `json:"sample"`
}

type GroupedViolation struct {
	Key               string
	EffectiveDirective string
	BlockedOrigin     string
	Count             int
	Pages             map[string][]Violation
}

type MergedGroup struct {
	Group    GroupedViolation
	Browsers []string
}

type Server struct {
	db   *sql.DB
	tmpl *template.Template
}

type CSPConfig struct {
	WaitUntil      string `json:"waitUntil"`
	NavTimeoutMs   int    `json:"navTimeoutMs"`
	SettleWaitMs   int    `json:"settleWaitMs"`
	Concurrency    int    `json:"concurrency"`
	BetweenURLMs   int    `json:"betweenUrlMs"`
	UserAgent      string `json:"userAgent"`
	AcceptLanguage string `json:"acceptLanguage"`
	Browser        string `json:"browser"`
}

type BrowserReport struct {
	Name   string
	Report Report
	Groups []GroupedViolation
	Warns  []GroupedViolation
}

type ProfileView struct {
	ID        int64
	Name      string
	CreatedAt string
	Config    CSPConfig
}

type MultiReport struct {
	GeneratedAt string             `json:"generatedAt"`
	Config      map[string]any     `json:"config"`
	Browsers    map[string]Report  `json:"browsers"`
}

type RunSummary struct {
	Pages      int                    `json:"pages"`
	Violations int                    `json:"violations"`
	Browsers   map[string]ReportTotals `json:"browsers"`
}

var version = "dev"
const defaultProfileName = "Default"
var browsers = []string{"chromium", "firefox", "webkit"}

func main() {
	addr := envDefault("CSP_WEB_ADDR", "127.0.0.1:8080")
	dbPath := envDefault("CSP_WEB_DB", "data.db")

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath))
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		log.Fatalf("db init: %v", err)
	}
	if err := ensureDefaultProfile(db); err != nil {
		log.Fatalf("default profile: %v", err)
	}

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"jsonPages":      jsonPages,
		"jsonViolations": jsonViolations,
		"groupPolicy":    groupPolicy,
		"groupDirective": groupDirective,
		"jsonPretty":     jsonPretty,
		"toJSON":         toJSON,
		"joinList":       joinList,
	}).ParseFS(templateFS, "web/templates/*.html")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	s := &Server{db: db, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/runs", s.handleRuns)
	mux.HandleFunc("/runs/", s.handleRunDetail)
	mux.HandleFunc("/runs/rerun", s.handleRunRerun)
	mux.HandleFunc("/runs/export", s.handleRunExport)
	mux.HandleFunc("/runs/copy", s.handleRunCopy)
	mux.HandleFunc("/profiles", s.handleProfiles)
	mux.HandleFunc("/profiles/update", s.handleProfileUpdate)
	if staticFS, err := fs.Sub(embeddedFS, "web/static"); err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	}

	log.Printf("csp-web %s listening on %s", version, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func initDB(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			config_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			profile_id INTEGER,
			created_at TEXT NOT NULL,
			urls_text TEXT NOT NULL,
			summary_json TEXT NOT NULL,
			results_json TEXT NOT NULL,
			exit_code INTEGER NOT NULL,
			elapsed_ms INTEGER NOT NULL,
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE SET NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_runs_profile_id ON runs(profile_id);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func ensureDefaultProfile(db *sql.DB) error {
	var id int64
	err := db.QueryRow(`SELECT id FROM profiles WHERE name = ?`, defaultProfileName).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Migrate legacy default name if present.
	var legacyID int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE name = ?`, "Default (Chromium)").Scan(&legacyID); err == nil {
		_, err := db.Exec(`UPDATE profiles SET name = ? WHERE id = ?`, defaultProfileName, legacyID)
		return err
	}
	cfg := defaultConfig()
	cfgJSON, _ := json.Marshal(cfg)
	_, err = db.Exec(`INSERT INTO profiles (name, config_json, created_at) VALUES (?, ?, ?)`,
		defaultProfileName, string(cfgJSON), time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	profiles, err := s.listProfiles(r.Context())
	if err != nil {
		http.Error(w, "profiles load failed", http.StatusInternalServerError)
		return
	}
	prefill := strings.TrimSpace(r.URL.Query().Get("urls"))
	s.render(w, "index.html", map[string]any{
		"Profiles":    profiles,
		"PrefillURLs": prefill,
	})
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		profiles, err := s.listProfiles(r.Context())
		if err != nil {
			http.Error(w, "profiles load failed", http.StatusInternalServerError)
			return
		}
		views := make([]ProfileView, 0, len(profiles))
		for _, p := range profiles {
			cfg, err := parseConfig(p.ConfigJSON)
			if err != nil {
				cfg = defaultConfig()
			}
			views = append(views, ProfileView{
				ID:        p.ID,
				Name:      p.Name,
				CreatedAt: p.CreatedAt,
				Config:    cfg,
			})
		}
		s.render(w, "profiles.html", map[string]any{
			"Profiles": views,
			"Defaults": defaultConfig(),
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		cfg := defaultConfig()
		if v := strings.TrimSpace(r.FormValue("wait_until")); v != "" {
			cfg.WaitUntil = v
		}
		if v := parseIntForm(r.FormValue("nav_timeout_ms")); v > 0 {
			cfg.NavTimeoutMs = v
		}
		if v := parseIntForm(r.FormValue("settle_wait_ms")); v >= 0 {
			cfg.SettleWaitMs = v
		}
		if v := parseIntForm(r.FormValue("concurrency")); v > 0 {
			cfg.Concurrency = v
		}
		if v := parseIntForm(r.FormValue("between_url_ms")); v >= 0 {
			cfg.BetweenURLMs = v
		}
		if v := strings.TrimSpace(r.FormValue("user_agent")); v != "" {
			cfg.UserAgent = v
		}
		if v := strings.TrimSpace(r.FormValue("accept_language")); v != "" {
			cfg.AcceptLanguage = v
		}
		cfgJSON, _ := json.Marshal(cfg)
		if err := s.createProfile(r.Context(), name, string(cfgJSON)); err != nil {
			http.Error(w, "create failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/profiles", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	idStr := strings.TrimSpace(r.FormValue("id"))
	if idStr == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	cfg := defaultConfig()
	if v := strings.TrimSpace(r.FormValue("wait_until")); v != "" {
		cfg.WaitUntil = v
	}
	if v := parseIntForm(r.FormValue("nav_timeout_ms")); v > 0 {
		cfg.NavTimeoutMs = v
	}
	if v := parseIntForm(r.FormValue("settle_wait_ms")); v >= 0 {
		cfg.SettleWaitMs = v
	}
	if v := parseIntForm(r.FormValue("concurrency")); v > 0 {
		cfg.Concurrency = v
	}
	if v := parseIntForm(r.FormValue("between_url_ms")); v >= 0 {
		cfg.BetweenURLMs = v
	}
	if v := strings.TrimSpace(r.FormValue("user_agent")); v != "" {
		cfg.UserAgent = v
	}
	if v := strings.TrimSpace(r.FormValue("accept_language")); v != "" {
		cfg.AcceptLanguage = v
	}
	cfgJSON, _ := json.Marshal(cfg)
	if err := s.updateProfile(r.Context(), id, name, string(cfgJSON)); err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/profiles", http.StatusSeeOther)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runs, err := s.listRuns(r.Context())
		if err != nil {
			http.Error(w, "runs load failed", http.StatusInternalServerError)
			return
		}
		profiles, _ := s.listProfiles(r.Context())
		s.render(w, "runs.html", map[string]any{
			"Runs":     runs,
			"Profiles": profiles,
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		urlsText := strings.TrimSpace(r.FormValue("urls"))
		if urlsText == "" {
			http.Error(w, "urls required", http.StatusBadRequest)
			return
		}
		urls := parseURLList(urlsText)
		if len(urls) == 0 {
			http.Error(w, "no valid urls", http.StatusBadRequest)
			return
		}

		var profileID sql.NullInt64
		cfg := defaultConfig()
		if v := strings.TrimSpace(r.FormValue("profile_id")); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				profileID = sql.NullInt64{Int64: id, Valid: true}
				if p, err := s.getProfile(r.Context(), id); err == nil {
					if parsed, err := parseConfig(p.ConfigJSON); err == nil {
						cfg = parsed
					}
				}
			}
		} else {
			if p, err := s.getProfileByName(r.Context(), defaultProfileName); err == nil {
				profileID = sql.NullInt64{Int64: p.ID, Valid: true}
				if parsed, err := parseConfig(p.ConfigJSON); err == nil {
					cfg = parsed
				}
			}
		}

		start := time.Now()
		report, exitCode, err := runCSPCheck(r.Context(), urls, cfg)
		elapsed := time.Since(start)
		if err != nil {
			http.Error(w, "csp check failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		resultsJSON, err := json.Marshal(report)
		if err != nil {
			http.Error(w, "marshal results failed", http.StatusInternalServerError)
			return
		}
		summary := summarizeMulti(report)
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			http.Error(w, "marshal summary failed", http.StatusInternalServerError)
			return
		}

		runID, err := s.createRun(r.Context(), profileID, urlsText, string(summaryJSON), string(resultsJSON), exitCode, elapsed.Milliseconds())
		if err != nil {
			http.Error(w, "save run failed", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/runs/%d", runID), http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRunRerun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	idStr := strings.TrimSpace(r.FormValue("id"))
	if idStr == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	prev, err := s.getRun(r.Context(), id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	urls := parseURLList(prev.URLsText)
	if len(urls) == 0 {
		http.Error(w, "no valid urls", http.StatusBadRequest)
		return
	}

	cfg := defaultConfig()
	profileID := prev.ProfileID
	if v := strings.TrimSpace(r.FormValue("profile_id")); v != "" {
		if pid, err := strconv.ParseInt(v, 10, 64); err == nil {
			profileID = sql.NullInt64{Int64: pid, Valid: true}
		}
	}
	if profileID.Valid {
		if p, err := s.getProfile(r.Context(), profileID.Int64); err == nil {
			if parsed, err := parseConfig(p.ConfigJSON); err == nil {
				cfg = parsed
			}
		}
	} else {
		if p, err := s.getProfileByName(r.Context(), defaultProfileName); err == nil {
			profileID = sql.NullInt64{Int64: p.ID, Valid: true}
			if parsed, err := parseConfig(p.ConfigJSON); err == nil {
				cfg = parsed
			}
		}
	}

	start := time.Now()
		report, exitCode, err := runCSPCheck(r.Context(), urls, cfg)
		elapsed := time.Since(start)
	if err != nil {
		http.Error(w, "csp check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resultsJSON, err := json.Marshal(report)
	if err != nil {
		http.Error(w, "marshal results failed", http.StatusInternalServerError)
		return
	}
	summary := summarizeMulti(report)
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		http.Error(w, "marshal summary failed", http.StatusInternalServerError)
		return
	}

	runID, err := s.createRun(r.Context(), profileID, prev.URLsText, string(summaryJSON), string(resultsJSON), exitCode, elapsed.Milliseconds())
	if err != nil {
		http.Error(w, "save run failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/runs/%d", runID), http.StatusSeeOther)
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/runs/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.getRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "run load failed", http.StatusInternalServerError)
		return
	}
	var multi MultiReport
	if err := json.Unmarshal([]byte(run.ResultsJSON), &multi); err != nil || len(multi.Browsers) == 0 {
		var single Report
		if err := json.Unmarshal([]byte(run.ResultsJSON), &single); err != nil {
			http.Error(w, "run parse failed", http.StatusInternalServerError)
			return
		}
		multi = MultiReport{
			GeneratedAt: single.GeneratedAt,
			Config:      single.Config,
			Browsers:    map[string]Report{"chromium": single},
		}
	}

	var browserReports []BrowserReport
	for _, name := range browsers {
		if rep, ok := multi.Browsers[name]; ok {
			browserReports = append(browserReports, BrowserReport{
				Name:   name,
				Report: rep,
				Groups: groupViolationsByDisposition(rep.Results, "enforce"),
				Warns:  groupViolationsByDisposition(rep.Results, "report-only"),
			})
		}
	}
	// Include any unexpected browser keys
	for name, rep := range multi.Browsers {
		found := false
		for _, b := range browserReports {
			if b.Name == name {
				found = true
				break
			}
		}
		if !found {
			browserReports = append(browserReports, BrowserReport{
				Name:   name,
				Report: rep,
				Groups: groupViolationsByDisposition(rep.Results, "enforce"),
				Warns:  groupViolationsByDisposition(rep.Results, "report-only"),
			})
		}
	}

	profiles, _ := s.listProfiles(r.Context())

	s.render(w, "run.html", map[string]any{
		"Run":      run,
		"Browsers": browserReports,
		"MergedErr":  groupViolationsMultiByDisposition(browserReports, "enforce"),
		"MergedWarn": groupViolationsMultiByDisposition(browserReports, "report-only"),
		"Profiles": profiles,
	})
}

func (s *Server) handleRunExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	pretty := strings.TrimSpace(r.URL.Query().Get("pretty")) == "1"
	if idStr == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	run, err := s.getRun(r.Context(), id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"csp-run-%d.json\"", run.ID))
	if !pretty {
		_, _ = w.Write([]byte(run.ResultsJSON))
		return
	}
	var obj any
	if err := json.Unmarshal([]byte(run.ResultsJSON), &obj); err != nil {
		_, _ = w.Write([]byte(run.ResultsJSON))
		return
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		_, _ = w.Write([]byte(run.ResultsJSON))
		return
	}
	_, _ = w.Write(b)
}

func (s *Server) handleRunCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	idStr := strings.TrimSpace(r.FormValue("id"))
	if idStr == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	run, err := s.getRun(r.Context(), id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/?urls="+url.QueryEscape(run.URLsText), http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) listProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, config_json, created_at FROM profiles ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.Name, &p.ConfigJSON, &p.CreatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func (s *Server) createProfile(ctx context.Context, name, configJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO profiles (name, config_json, created_at) VALUES (?, ?, ?)`,
		name, configJSON, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s *Server) updateProfile(ctx context.Context, id int64, name, configJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE profiles SET name = ?, config_json = ? WHERE id = ?`,
		name, configJSON, id,
	)
	return err
}
func (s *Server) getProfile(ctx context.Context, id int64) (Profile, error) {
	var p Profile
	row := s.db.QueryRowContext(ctx, `SELECT id, name, config_json, created_at FROM profiles WHERE id = ?`, id)
	if err := row.Scan(&p.ID, &p.Name, &p.ConfigJSON, &p.CreatedAt); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Server) getProfileByName(ctx context.Context, name string) (Profile, error) {
	var p Profile
	row := s.db.QueryRowContext(ctx, `SELECT id, name, config_json, created_at FROM profiles WHERE name = ?`, name)
	if err := row.Scan(&p.ID, &p.Name, &p.ConfigJSON, &p.CreatedAt); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Server) listRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, profile_id, created_at, urls_text, summary_json, results_json, exit_code, elapsed_ms FROM runs ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.ProfileID, &r.CreatedAt, &r.URLsText, &r.SummaryJSON, &r.ResultsJSON, &r.ExitCode, &r.ElapsedMs); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *Server) createRun(ctx context.Context, profileID sql.NullInt64, urlsText, summaryJSON, resultsJSON string, exitCode int, elapsedMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (profile_id, created_at, urls_text, summary_json, results_json, exit_code, elapsed_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		profileID, time.Now().UTC().Format(time.RFC3339), urlsText, summaryJSON, resultsJSON, exitCode, elapsedMs,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Server) getRun(ctx context.Context, id int64) (Run, error) {
	var r Run
	row := s.db.QueryRowContext(ctx, `SELECT id, profile_id, created_at, urls_text, summary_json, results_json, exit_code, elapsed_ms FROM runs WHERE id = ?`, id)
	if err := row.Scan(&r.ID, &r.ProfileID, &r.CreatedAt, &r.URLsText, &r.SummaryJSON, &r.ResultsJSON, &r.ExitCode, &r.ElapsedMs); err != nil {
		return r, err
	}
	return r, nil
}

func runCSPCheck(ctx context.Context, urls []string, cfg CSPConfig) (MultiReport, int, error) {
	nodeBin := envDefault("CSP_NODE_BIN", "node")
	scriptPath := envDefault("CSP_SCRIPT_PATH", "./csp-check.mjs")
	browserReports := make(map[string]Report, len(browsers))
	maxExit := 0

	tmpDir, err := os.MkdirTemp("", "csp-check-")
	if err != nil {
		return MultiReport{}, 0, err
	}
	defer os.RemoveAll(tmpDir)

	urlsFile := filepath.Join(tmpDir, "urls.txt")
	if err := os.WriteFile(urlsFile, []byte(strings.Join(urls, "\n")), 0644); err != nil {
		return MultiReport{}, 0, err
	}

	for _, browser := range browsers {
		jsonFile := filepath.Join(tmpDir, fmt.Sprintf("report-%s.json", browser))
		cmd := exec.CommandContext(ctx, nodeBin, scriptPath, urlsFile)
		cmd.Env = append(os.Environ(),
			"CSP_OUTPUT_JSON=1",
			"CSP_OUTPUT_FILE="+jsonFile,
			"CSP_VERBOSE=0",
			"CSP_WAIT_UNTIL="+cfg.WaitUntil,
			"CSP_NAV_TIMEOUT_MS="+strconv.Itoa(cfg.NavTimeoutMs),
			"CSP_WAIT_MS="+strconv.Itoa(cfg.SettleWaitMs),
			"CSP_CONCURRENCY="+strconv.Itoa(cfg.Concurrency),
			"CSP_BETWEEN_URL_MS="+strconv.Itoa(cfg.BetweenURLMs),
			"CSP_USER_AGENT="+cfg.UserAgent,
			"CSP_ACCEPT_LANGUAGE="+cfg.AcceptLanguage,
			"CSP_BROWSER="+browser,
		)

		stderr, err := cmd.StderrPipe()
		if err != nil {
			return MultiReport{}, 0, err
		}
		if err := cmd.Start(); err != nil {
			return MultiReport{}, 0, err
		}

		stderrData, _ := io.ReadAll(stderr)
		waitErr := cmd.Wait()
		exitCode := exitCodeFromState(cmd.ProcessState, waitErr)
		if exitCode > maxExit {
			maxExit = exitCode
		}
		if waitErr != nil {
			// Still try to parse JSON if it exists.
			if _, statErr := os.Stat(jsonFile); statErr != nil {
				return MultiReport{}, exitCode, fmt.Errorf("node failed (%s): %v: %s", browser, waitErr, strings.TrimSpace(string(stderrData)))
			}
		}

		data, err := os.ReadFile(jsonFile)
		if err != nil {
			return MultiReport{}, exitCode, err
		}

		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			return MultiReport{}, exitCode, err
		}
		browserReports[browser] = report
	}

	multi := MultiReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Config: map[string]any{
			"waitUntil":      cfg.WaitUntil,
			"navTimeoutMs":   cfg.NavTimeoutMs,
			"settleWaitMs":   cfg.SettleWaitMs,
			"concurrency":    cfg.Concurrency,
			"betweenUrlMs":   cfg.BetweenURLMs,
			"userAgent":      cfg.UserAgent,
			"acceptLanguage": cfg.AcceptLanguage,
		},
		Browsers: browserReports,
	}
	return multi, maxExit, nil
}

func exitCodeFromErr(err error) int {
    var exitErr *exec.ExitError
    if err == nil {
        return 0
    }
    if errors.As(err, &exitErr) {
        return exitErr.ExitCode()
    }
    return 1
}

func exitCodeFromState(state *os.ProcessState, err error) int {
    if err != nil {
        return exitCodeFromErr(err)
    }
    if state == nil {
        return 0
    }
    return state.ExitCode()
}

func parseURLList(text string) []string {
	lines := strings.Split(text, "\n")
	var urls []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			if normalized, ok := normalizeURL(line); ok {
				urls = append(urls, normalized)
			}
		}
	}
	return urls
}

func normalizeURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), true
}

func groupViolations(results []ReportPageResult) []GroupedViolation {
	groups := map[string]*GroupedViolation{}
	for _, r := range results {
		for _, v := range r.Violations {
			key := fmt.Sprintf("%s -> %s", v.EffectiveDirective, v.BlockedOrigin)
			g, ok := groups[key]
			if !ok {
				g = &GroupedViolation{
					Key:               key,
					EffectiveDirective: v.EffectiveDirective,
					BlockedOrigin:     v.BlockedOrigin,
					Pages:             map[string][]Violation{},
				}
				groups[key] = g
			}
			g.Count++
			g.Pages[r.URL] = append(g.Pages[r.URL], v)
		}
	}

	ordered := make([]GroupedViolation, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, *g)
	}
	// simple bubble sort for small lists
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].Count > ordered[i].Count {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	return ordered
}

func groupViolationsMulti(browsers []BrowserReport) []MergedGroup {
	var all []ReportPageResult
	groupBrowsers := map[string]map[string]struct{}{}
	for _, b := range browsers {
		for _, r := range b.Report.Results {
			all = append(all, r)
			for _, v := range r.Violations {
				key := fmt.Sprintf("%s -> %s", v.EffectiveDirective, v.BlockedOrigin)
				if _, ok := groupBrowsers[key]; !ok {
					groupBrowsers[key] = map[string]struct{}{}
				}
				groupBrowsers[key][b.Name] = struct{}{}
			}
		}
	}
	grouped := groupViolations(all)
	out := make([]MergedGroup, 0, len(grouped))
	for _, g := range grouped {
		names := make([]string, 0, len(groupBrowsers[g.Key]))
		for name := range groupBrowsers[g.Key] {
			names = append(names, name)
		}
		// simple sort for stable output
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				if names[j] < names[i] {
					names[i], names[j] = names[j], names[i]
				}
			}
		}
		out = append(out, MergedGroup{Group: g, Browsers: names})
	}
	return out
}

func groupViolationsByDisposition(results []ReportPageResult, disposition string) []GroupedViolation {
	filtered := make([]ReportPageResult, 0, len(results))
	for _, r := range results {
		nr := r
		nr.Violations = nil
		for _, v := range r.Violations {
			if isDisposition(v.Disposition, disposition) {
				nr.Violations = append(nr.Violations, v)
			}
		}
		if len(nr.Violations) > 0 {
			filtered = append(filtered, nr)
		}
	}
	return groupViolations(filtered)
}

func groupViolationsMultiByDisposition(browsers []BrowserReport, disposition string) []MergedGroup {
	var all []ReportPageResult
	groupBrowsers := map[string]map[string]struct{}{}
	for _, b := range browsers {
		for _, r := range b.Report.Results {
			var nr ReportPageResult
			nr = r
			nr.Violations = nil
			for _, v := range r.Violations {
				if !isDisposition(v.Disposition, disposition) {
					continue
				}
				nr.Violations = append(nr.Violations, v)
				key := fmt.Sprintf("%s -> %s", v.EffectiveDirective, v.BlockedOrigin)
				if _, ok := groupBrowsers[key]; !ok {
					groupBrowsers[key] = map[string]struct{}{}
				}
				groupBrowsers[key][b.Name] = struct{}{}
			}
			if len(nr.Violations) > 0 {
				all = append(all, nr)
			}
		}
	}
	grouped := groupViolations(all)
	out := make([]MergedGroup, 0, len(grouped))
	for _, g := range grouped {
		names := make([]string, 0, len(groupBrowsers[g.Key]))
		for name := range groupBrowsers[g.Key] {
			names = append(names, name)
		}
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				if names[j] < names[i] {
					names[i], names[j] = names[j], names[i]
				}
			}
		}
		out = append(out, MergedGroup{Group: g, Browsers: names})
	}
	return out
}

func isDisposition(actual, want string) bool {
	a := strings.ToLower(strings.TrimSpace(actual))
	if want == "report-only" {
		return strings.Contains(a, "report")
	}
	return a == "" || a == "enforce"
}

func defaultConfig() CSPConfig {
	return CSPConfig{
		WaitUntil:      "networkidle",
		NavTimeoutMs:   45000,
		SettleWaitMs:   3000,
		Concurrency:    1,
		BetweenURLMs:   600,
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		AcceptLanguage: "en-US,en;q=0.9",
		Browser:        "chromium",
	}
}

func defaultConfigJSON() string {
	cfg := defaultConfig()
	b, _ := json.Marshal(cfg)
	return string(b)
}

func parseConfig(raw string) (CSPConfig, error) {
	cfg := defaultConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	if cfg.WaitUntil == "" {
		cfg.WaitUntil = "domcontentloaded"
	}
	if cfg.NavTimeoutMs <= 0 {
		cfg.NavTimeoutMs = 30000
	}
	if cfg.SettleWaitMs < 0 {
		cfg.SettleWaitMs = 0
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.BetweenURLMs < 0 {
		cfg.BetweenURLMs = 0
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultConfig().UserAgent
	}
	if cfg.AcceptLanguage == "" {
		cfg.AcceptLanguage = defaultConfig().AcceptLanguage
	}
	if cfg.Browser == "" {
		cfg.Browser = defaultConfig().Browser
	}
	return cfg, nil
}

func envDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

//go:embed web/templates/*.html web/static/*
var embeddedFS embed.FS

var templateFS = embeddedFS

func jsonPages(summary string) int {
	var totals ReportTotals
	if err := json.Unmarshal([]byte(summary), &totals); err == nil && totals.Pages > 0 {
		return totals.Pages
	}
	var multi RunSummary
	if err := json.Unmarshal([]byte(summary), &multi); err != nil {
		return 0
	}
	return multi.Pages
}

func jsonViolations(summary string) int {
	var totals ReportTotals
	if err := json.Unmarshal([]byte(summary), &totals); err == nil && totals.Violations > 0 {
		return totals.Violations
	}
	var multi RunSummary
	if err := json.Unmarshal([]byte(summary), &multi); err != nil {
		return 0
	}
	return multi.Violations
}

func groupPolicy(g GroupedViolation) string {
	for _, vs := range g.Pages {
		for _, v := range vs {
			if strings.TrimSpace(v.OriginalPolicy) != "" {
				return v.OriginalPolicy
			}
		}
	}
	return ""
}

func groupDirective(g GroupedViolation) string {
	for _, vs := range g.Pages {
		for _, v := range vs {
			if strings.TrimSpace(v.OriginalPolicy) == "" || strings.TrimSpace(v.EffectiveDirective) == "" {
				continue
			}
			if val := extractDirective(v.OriginalPolicy, v.EffectiveDirective); val != "" {
				return val
			}
		}
	}
	return ""
}

func extractDirective(policy, directive string) string {
	parts := strings.Split(policy, ";")
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, directive+" ") || p == directive {
			return p
		}
	}
	return ""
}

func parseIntForm(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return v
}

func summarizeMulti(m MultiReport) RunSummary {
	summary := RunSummary{
		Pages:    0,
		Browsers: map[string]ReportTotals{},
	}
	for name, rep := range m.Browsers {
		summary.Browsers[name] = rep.Totals
		if rep.Totals.Pages > summary.Pages {
			summary.Pages = rep.Totals.Pages
		}
		summary.Violations += rep.Totals.Violations
	}
	return summary
}

func jsonPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func toJSON(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

func joinList(items []string) string {
	return strings.Join(items, ", ")
}
