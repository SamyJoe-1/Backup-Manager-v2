// Package api is the HTTP side: JSON API, live events and the embedded web panel.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"backupmanager/internal/core"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web
var webFS embed.FS

type Server struct {
	App   *core.App
	mu    sync.Mutex
	fails map[string][]time.Time
}

type user struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type ctxKey struct{}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string   { return e.msg }
func bad(msg string, a ...any) error { return &httpError{400, fmt.Sprintf(msg, a...)} }

type handler func(w http.ResponseWriter, r *http.Request) (any, error)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	pub := func(pattern string, h handler) { mux.Handle(pattern, s.guard(h, 0)) }
	api := func(pattern string, h handler) { mux.Handle(pattern, s.guard(h, levelFor(pattern))) }
	api("GET /api/me", s.me)
	api("POST /api/me/password", s.mePassword)
	api("POST /api/me/totp/start", s.totpStart)
	api("POST /api/me/totp/enable", s.totpEnable)
	api("POST /api/me/totp/disable", s.totpDisable)
	api("GET /api/users", s.userList)
	api("POST /api/users", s.userSave)
	api("PUT /api/users/{id}", s.userSave)
	api("DELETE /api/users/{id}", s.userDelete)
	api("GET /api/tokens", s.tokenList)
	api("POST /api/tokens", s.tokenCreate)
	api("DELETE /api/tokens/{id}", s.tokenDelete)
	api("GET /metrics", s.metrics)
	api("GET /api/agents", s.agentList)
	api("POST /api/agents", s.agentCreate)
	api("DELETE /api/agents/{id}", s.agentDelete)
	mux.HandleFunc("POST /api/agent/poll", s.agentPoll)
	mux.HandleFunc("POST /api/agent/progress", s.agentProgress)
	mux.HandleFunc("POST /api/agent/result", s.agentResult)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.App.Store.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "catalog not reachable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})

	pub("GET /api/state", s.state)
	pub("POST /api/setup", s.setup)
	pub("POST /api/login", s.login)
	api("POST /api/logout", s.logout)
	api("GET /api/dashboard", s.dashboard)
	api("GET /api/events", s.events)

	for _, t := range []string{"targets", "destinations"} {
		api("GET /api/"+t, s.endpointList(t))
		api("POST /api/"+t, s.endpointSave(t))
		api("PUT /api/"+t+"/{id}", s.endpointSave(t))
		api("DELETE /api/"+t+"/{id}", s.endpointDelete(t))
		api("POST /api/"+t+"/{id}/test", s.endpointTest(t))
	}
	api("GET /api/targets/{id}/tables", s.targetTables)

	api("GET /api/plans", s.planList)
	api("POST /api/plans", s.planSave)
	api("PUT /api/plans/{id}", s.planSave)
	api("DELETE /api/plans/{id}", s.planDelete)
	api("POST /api/plans/{id}/run", s.planRun)

	api("GET /api/backups", s.backupList)
	api("GET /api/backups/{id}", s.backupGet)
	api("POST /api/backups/{id}/pin", s.backupPin)
	api("POST /api/backups/{id}/note", s.backupNote)
	api("POST /api/backups/{id}/verify", s.backupVerify)
	api("DELETE /api/backups/{id}", s.backupDelete)
	api("GET /api/backups/{id}/download", s.backupDownload)
	api("POST /api/backups/{id}/restore", s.backupRestore)
	api("GET /api/targets/{id}/binlogs", s.targetBinlogs)
	api("GET /api/backups/{id}/explore", s.exploreState)
	api("POST /api/backups/{id}/explore", s.exploreStart)
	api("DELETE /api/backups/{id}/explore", s.exploreStop)
	api("GET /api/backups/{id}/rows", s.backupRows)
	api("POST /api/backups/{id}/test", s.backupTest)
	api("POST /api/backups/{id}/restore-table", s.backupRestoreTable)
	api("GET /api/backups/{id}/files", s.backupFiles)
	api("GET /api/sets", s.setList)
	api("POST /api/sets", s.setSave)
	api("PUT /api/sets/{id}", s.setSave)
	api("DELETE /api/sets/{id}", s.setDelete)
	api("POST /api/sets/{id}/run", s.setRun)
	api("GET /api/sets/{id}/runs", s.setRuns)

	api("GET /api/jobs", s.jobList)
	api("POST /api/jobs/{id}/cancel", s.jobCancel)
	api("POST /api/hold", s.hold)
	api("GET /api/settings", s.settingsGet)
	api("PUT /api/settings", s.settingsPut)
	api("GET /api/audit", s.audit)

	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return mux
}

func writeErr(w http.ResponseWriter, err error) {
	code := 500
	var he *httpError
	if errors.As(err, &he) {
		code = he.code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func me(r *http.Request) *user { u, _ := r.Context().Value(ctxKey{}).(*user); return u }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) log(r *http.Request, action, detail string) {
	actor := "anonymous"
	if u := me(r); u != nil {
		actor = u.Email
	}
	s.App.Store.Audit(actor, action, detail, clientIP(r))
}

func body(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := d.Decode(v); err != nil {
		return bad("the request body is not valid JSON")
	}
	return nil
}

func pathID(r *http.Request) int64 { n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64); return n }
func qInt(r *http.Request, k string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return n
}

// ---- sessions ----

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

func randomToken() string { b := make([]byte, 32); rand.Read(b); return hex.EncodeToString(b) }

func (s *Server) currentUser(r *http.Request) *user {
	c, err := r.Cookie("bm_session")
	if err != nil || c.Value == "" {
		return nil
	}
	var u user
	err = s.App.Store.DB.QueryRow(`SELECT u.id,u.email,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`,
		hashToken(c.Value), time.Now().Unix()).Scan(&u.ID, &u.Email, &u.Role)
	if err != nil {
		return nil
	}
	return &u
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64) error {
	tok := randomToken()
	exp := time.Now().Add(7 * 24 * time.Hour)
	if _, err := s.App.Store.DB.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, hashToken(tok), uid, exp.Unix()); err != nil {
		return err
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{Name: "bm_session", Value: tok, Path: "/", Expires: exp, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure})
	return nil
}

func (s *Server) userCount() int {
	var n int
	s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{"setupNeeded": s.userCount() == 0, "user": s.currentUser(r), "version": core.Version, "held": s.App.Queue.Held()}, nil
}

func checkPassword(p string) error {
	if len(p) < 10 {
		return bad("the password needs at least 10 characters")
	}
	return nil
}

// CreateUser is shared by the setup page and the CLI.
func CreateUser(st *core.Store, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return 0, bad("enter a valid e-mail address")
	}
	if err := checkPassword(password); err != nil {
		return 0, err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return 0, err
	}
	res, err := st.DB.Exec(`INSERT INTO users(email,pass,role,created_at) VALUES(?,?,?,?)`, email, string(h), "owner", time.Now().Unix())
	if err != nil {
		return 0, bad("this e-mail already has an account")
	}
	return res.LastInsertId()
}

// NewSetupToken stores a one-time token for the first-run page and returns it.
func NewSetupToken(st *core.Store) (string, error) {
	t := randomToken()[:32]
	return t, st.SetSetting("setup_token", hashToken(t))
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) (any, error) {
	if s.userCount() > 0 {
		return nil, &httpError{409, "setup is already done; sign in instead"}
	}
	var in struct{ Token, Email, Password string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	want := s.App.Store.Setting("setup_token")
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(hashToken(strings.TrimSpace(in.Token)))) != 1 {
		return nil, &httpError{403, "the setup code is wrong. Run `backup-manager setup-link` on the server to get a new one"}
	}
	id, err := CreateUser(s.App.Store, in.Email, in.Password)
	if err != nil {
		return nil, err
	}
	s.App.Store.SetSetting("setup_token", "")
	s.App.Store.Audit(in.Email, "setup", "first account created", clientIP(r))
	return map[string]bool{"ok": true}, s.startSession(w, r, id)
}

func (s *Server) tooManyFails(ip string, add bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails == nil {
		s.fails = map[string][]time.Time{}
	}
	cut := time.Now().Add(-10 * time.Minute)
	var keep []time.Time
	for _, t := range s.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if add {
		keep = append(keep, time.Now())
	}
	s.fails[ip] = keep
	return len(keep) >= 8
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) (any, error) {
	ip := clientIP(r)
	if s.tooManyFails(ip, false) {
		return nil, &httpError{429, "too many wrong attempts; wait 10 minutes"}
	}
	var in struct{ Email, Password, Code string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	var id int64
	var hash string
	err := s.App.Store.DB.QueryRow(`SELECT id,pass FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(in.Email))).Scan(&id, &hash)
	if err != nil {
		hash = "$2a$12$0000000000000000000000000000000000000000000000000000." // same work for unknown e-mails
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || err != nil {
		s.tooManyFails(ip, true)
		s.App.Store.Audit(in.Email, "login.failed", "", ip)
		return nil, &httpError{401, "wrong e-mail or password"}
	}
	if _, on := s.totpSecret(id); on {
		if strings.TrimSpace(in.Code) == "" {
			return nil, &httpError{401, "enter the 6-digit code from your authenticator app"}
		}
		if !s.secondFactor(id, in.Code) {
			s.tooManyFails(ip, true)
			s.App.Store.Audit(in.Email, "login.failed", "wrong second-factor code", ip)
			return nil, &httpError{401, "that code is not right"}
		}
	}
	s.App.Store.Audit(in.Email, "login", "", ip)
	return map[string]bool{"ok": true}, s.startSession(w, r, id)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) (any, error) {
	if c, err := r.Cookie("bm_session"); err == nil {
		s.App.Store.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "bm_session", Path: "/", MaxAge: -1})
	return map[string]bool{"ok": true}, nil
}

// ---- targets & destinations ----

func (s *Server) endpointList(table string) handler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) { return s.App.Store.Endpoints(table) }
}

func (s *Server) endpointSave(table string) handler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Name, Kind string
			Cfg        json.RawMessage
			Secret     *string
		}
		if err := body(r, &in); err != nil {
			return nil, err
		}
		e := &core.Endpoint{ID: pathID(r), Name: strings.TrimSpace(in.Name), Kind: in.Kind, Cfg: in.Cfg}
		if e.Name == "" {
			return nil, bad("give it a name")
		}
		if table == "targets" && e.Kind != "mysql" && e.Kind != "files" && e.Kind != "redis" && e.Kind != "postgres" {
			return nil, bad("unknown kind of target")
		}
		if table == "targets" && e.ID != 0 {
			if old, err := s.App.Store.Endpoint(table, e.ID); err == nil && old.Kind != e.Kind {
				return nil, bad("the kind of an existing target cannot be changed; add a new one")
			}
		}
		if table == "destinations" && e.Kind != "local" && e.Kind != "s3" {
			return nil, bad("destination kind must be local or s3")
		}
		if !json.Valid(e.Cfg) {
			return nil, bad("settings are not valid")
		}
		if table == "destinations" {
			if _, err := core.OpenStorage(e.Kind, string(e.Cfg), ""); err != nil {
				return nil, bad("%v", err)
			}
		}
		var sealed []byte
		if in.Secret != nil && (*in.Secret != "" || e.ID == 0) {
			var err error
			if sealed, err = s.App.Keys.Seal([]byte(*in.Secret)); err != nil {
				return nil, err
			}
		}
		if err := s.App.Store.SaveEndpoint(table, e, sealed); err != nil {
			return nil, err
		}
		s.log(r, table+".save", e.Name)
		return s.App.Store.Endpoint(table, e.ID)
	}
}

func (s *Server) endpointDelete(table string) handler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		id := pathID(r)
		col := "target_id"
		if table == "destinations" {
			col = "destination_id"
		}
		var n int
		s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM plans WHERE `+col+`=?`, id).Scan(&n)
		if n > 0 {
			return nil, bad("%d plan(s) still use it; delete or change them first", n)
		}
		s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM backups WHERE `+col+`=? AND deleted_at=0 AND status='done'`, id).Scan(&n)
		if n > 0 && table == "destinations" {
			return nil, bad("%d backup(s) are still stored there; delete them first", n)
		}
		_, err := s.App.Store.DB.Exec(`DELETE FROM `+table+` WHERE id=?`, id)
		s.log(r, table+".delete", strconv.FormatInt(id, 10))
		return map[string]bool{"ok": true}, err
	}
}

func (s *Server) endpointTest(table string) handler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if table == "destinations" {
			st, err := s.App.OpenDestination(pathID(r))
			if err == nil {
				err = st.Test(ctx)
			}
			if err != nil {
				return nil, bad("%v", err)
			}
			free := st.Free()
			return map[string]any{"ok": true, "free": free}, nil
		}
		info, err := s.App.TestTarget(ctx, pathID(r))
		if err != nil {
			return nil, bad("%v", err)
		}
		return info, nil
	}
}

func (s *Server) targetTables(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.App.TargetTables(ctx, pathID(r))
	if err != nil {
		return nil, bad("%v", err)
	}
	return list, nil
}

// ---- plans ----

func (s *Server) planList(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.App.Store.Plans("")
}

func (s *Server) planSave(w http.ResponseWriter, r *http.Request) (any, error) {
	var p core.Plan
	if err := body(r, &p); err != nil {
		return nil, err
	}
	p.ID = pathID(r)
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return nil, bad("give the plan a name")
	}
	if _, err := s.App.Store.Endpoint("targets", p.TargetID); err != nil {
		return nil, bad("choose a database target")
	}
	if _, err := s.App.Store.Endpoint("destinations", p.DestinationID); err != nil {
		return nil, bad("choose a destination")
	}
	tk, _ := s.App.Store.Endpoint("targets", p.TargetID)
	if k := core.MethodForKind(tk.Kind); k != "" {
		p.Spec.Method = k
	} else if core.MethodForKind(p.Spec.Method) != "" {
		p.Spec.Method = "logical"
	}
	if err := p.Spec.Normalize(); err != nil {
		return nil, bad("%v", err)
	}
	if core.NeedsRunner(p.Spec.Method) {
		t, _ := s.App.Store.Endpoint("targets", p.TargetID)
		var cfg core.TargetConfig
		json.Unmarshal(t.Cfg, &cfg)
		if _, err := core.NewRunner(cfg.Exec); err != nil {
			return nil, bad("%v", err)
		}
	}
	if p.KeepDays < 0 || p.KeepLast < 0 {
		return nil, bad("retention numbers cannot be negative")
	}
	next, err := core.NextRun(p.Schedule, time.Now())
	if err != nil {
		return nil, bad("%v", err)
	}
	p.NextRunAt = next
	if err := s.App.Store.SavePlan(&p); err != nil {
		return nil, err
	}
	s.log(r, "plan.save", p.Name)
	return s.App.Store.Plan(p.ID)
}

func (s *Server) planDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	id := pathID(r)
	if s.App.Queue.ActiveForPlan(id) {
		return nil, bad("this plan has a job waiting or running; cancel it first")
	}
	_, err := s.App.Store.DB.Exec(`DELETE FROM plans WHERE id=?`, id)
	s.log(r, "plan.delete", strconv.FormatInt(id, 10))
	return map[string]bool{"ok": true}, err
}

func (s *Server) planRun(w http.ResponseWriter, r *http.Request) (any, error) {
	p, err := s.App.Store.Plan(pathID(r))
	if err != nil {
		return nil, &httpError{404, "plan not found"}
	}
	if s.App.Queue.ActiveForPlan(p.ID) {
		return nil, bad("this plan already has a backup waiting or running")
	}
	b, err := s.App.StartBackup(p, "manual")
	if err != nil {
		return nil, bad("%v", err)
	}
	if b == nil { // binary-log plans have no backup record
		s.log(r, "binlog.start", p.Name)
		return map[string]bool{"queued": true}, nil
	}
	s.log(r, "backup.start", p.Name+" -> "+b.ID)
	return b, nil
}

// ---- backups ----

func (s *Server) backupList(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	f := core.BackupFilter{Page: int(qInt(r, "page")), PageSize: int(qInt(r, "pageSize")), Search: q.Get("search"), Status: q.Get("status"),
		PlanID: qInt(r, "plan"), TargetID: qInt(r, "target"), From: qInt(r, "from"), To: qInt(r, "to"), Sort: q.Get("sort"), Dir: q.Get("dir"),
		Pinned: q.Get("pinned") == "1", IncludeDeleted: q.Get("deleted") == "1"}
	list, total, err := s.App.Store.ListBackups(f)
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": list, "total": total}, nil
}

func (s *Server) backup(r *http.Request) (*core.Backup, error) {
	b, err := s.App.Store.Backup(r.PathValue("id"))
	if err != nil {
		return nil, &httpError{404, "backup not found"}
	}
	return b, nil
}

func (s *Server) backupGet(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	mj, _ := s.App.Store.BackupManifest(b.ID)
	var m json.RawMessage
	if mj != "" {
		m = json.RawMessage(mj)
	}
	return map[string]any{"backup": b, "manifest": m}, nil
}

func (s *Server) backupPin(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Pinned bool }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	_, err = s.App.Store.DB.Exec(`UPDATE backups SET pinned=? WHERE id=?`, in.Pinned, b.ID)
	s.log(r, "backup.pin", fmt.Sprintf("%s pinned=%v", b.ID, in.Pinned))
	return map[string]bool{"ok": true}, err
}

func (s *Server) backupNote(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Note string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	if len(in.Note) > 2000 {
		return nil, bad("the note is too long")
	}
	_, err := s.App.Store.DB.Exec(`UPDATE backups SET note=? WHERE id=?`, in.Note, r.PathValue("id"))
	return map[string]bool{"ok": true}, err
}

func (s *Server) backupVerify(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if b.Status != "done" || b.DeletedAt > 0 {
		return nil, bad("only a finished backup can be verified")
	}
	j := &core.Job{Kind: "verify", BackupID: b.ID, Title: "Verify: " + b.ID, MaxAttempts: 1, Priority: 3}
	return j, s.App.Queue.Enqueue(j)
}

func (s *Server) backupDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if b.Pinned {
		return nil, bad("this backup is pinned; unpin it first")
	}
	who := me(r).Email
	if err := s.App.DeleteBackup(r.Context(), b, "deleted by "+who); err != nil {
		return nil, bad("%v", err)
	}
	s.log(r, "backup.delete", b.ID)
	return map[string]bool{"ok": true}, nil
}

func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if b.Status != "done" || b.DeletedAt > 0 {
		return nil, bad("this backup cannot be downloaded (status: %s)", b.Status)
	}
	q := r.URL.Query()
	// raw=<object key>: the stored file as it is (compressed, encrypted). Supports resume (Range).
	if raw := q.Get("raw"); raw != "" {
		rs, name, err := s.App.OpenRawObject(r.Context(), b, raw)
		if err != nil {
			return nil, bad("%v", err)
		}
		defer rs.Close()
		s.log(r, "backup.download", b.ID+" raw "+raw)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, name, time.Unix(b.FinishedAt, 0), rs)
		return nil, nil
	}
	if b.Method == "physical" || b.Method == "incremental" {
		return nil, bad("a physical backup is a copy of the data files, not SQL: use Restore, or download the raw file")
	}
	// obj=<object key>: one stored file, decrypted and decompressed (tar of folders, Redis dump, PostgreSQL dump)
	if obj := q.Get("obj"); obj != "" {
		name := strings.TrimSuffix(strings.TrimSuffix(path.Base(obj), ".enc"), ".zst")
		s.log(r, "backup.download", b.ID+" "+name)
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(s.App.ExportObject(r.Context(), b, obj, pw)) }()
		defer pr.Close()
		first := make([]byte, 64<<10)
		n, err := io.ReadAtLeast(pr, first, 1)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, bad("%v", err)
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, b.ID, strings.Map(safeName, name)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Write(first[:n])
		if _, err := io.Copy(w, pr); err != nil {
			panic(http.ErrAbortHandler)
		}
		return nil, nil
	}
	if b.Method != "" && b.Method != "logical" && b.Method != "physical" && b.Method != "incremental" {
		return nil, bad("download the files of this backup one by one (it has no combined SQL)")
	}
	opt := core.ExportOptions{DB: q.Get("db"), Table: q.Get("table"), Part: q.Get("part"), NoDB: q.Get("nodb") == "1"}
	if opt.Part != "" && opt.Part != "schema" && opt.Part != "data" {
		return nil, bad("part must be schema or data")
	}
	name := b.ID
	if opt.DB != "" {
		name += "." + opt.DB
	}
	if opt.Table != "" {
		name += "." + opt.Table
	}
	if opt.Part != "" {
		name += "." + opt.Part
	}
	s.log(r, "backup.download", name)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.sql"`, strings.Map(safeName, name)))
	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := s.App.ExportSQL(r.Context(), b, opt, w); err != nil {
		// Headers are gone already: break the connection so the browser shows a failed download
		// instead of a file that looks complete.
		fmt.Fprintf(w, "\n-- EXPORT FAILED: %v\n", err)
		panic(http.ErrAbortHandler)
	}
	return nil, nil
}

// backupRestore queues the rebuild of a data folder from a physical backup (and, for a point in
// time, lays the binary logs next to it). It never touches a running database: the folder must be empty.
func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if b.Status != "done" || b.DeletedAt > 0 || (b.Method != "physical" && b.Method != "incremental") {
		return nil, bad("only a finished physical backup can be restored this way")
	}
	var in core.RestoreRequest
	if err := body(r, &in); err != nil {
		return nil, err
	}
	in.BackupID = b.ID
	if !strings.HasPrefix(in.Dir, "/") {
		return nil, bad("give an absolute folder path")
	}
	if in.Exec != "" {
		if _, err := core.NewRunner(in.Exec); err != nil {
			return nil, bad("%v", err)
		}
	}
	if in.StopAt != 0 && in.StopAt < b.FinishedAt {
		return nil, bad("the moment to restore to is before this backup was taken; pick an older backup")
	}
	payload, _ := json.Marshal(in)
	j := &core.Job{Kind: "restore", BackupID: b.ID, Title: "Restore: " + b.ID + " -> " + in.Dir, Payload: payload, MaxAttempts: 1, Priority: 9}
	s.log(r, "restore.start", b.ID+" -> "+in.Dir)
	return j, s.App.Queue.Enqueue(j)
}

func (s *Server) targetBinlogs(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.App.Binlogs(pathID(r))
}

// ---- look inside a backup (sandbox) ----

func (s *Server) usable(r *http.Request) (*core.Backup, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if b.Status != "done" || b.DeletedAt > 0 {
		return nil, bad("this backup is not available (status: %s)", b.Status)
	}
	return b, nil
}

func (s *Server) activeJob(kind, backupID string) int64 {
	var id int64
	s.App.Store.DB.QueryRow(`SELECT id FROM jobs WHERE kind=? AND backup_id=? AND state IN ('queued','running') ORDER BY id DESC LIMIT 1`, kind, backupID).Scan(&id)
	return id
}

func (s *Server) exploreState(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tables, terr := s.App.SandboxTables(ctx, b)
	if terr != nil {
		tables = []core.TableInfo{}
	}
	return map[string]any{"tables": tables, "jobId": s.activeJob("explore", b.ID), "open": s.App.Sandbox(b.ID, "explore") != nil}, nil
}

func (s *Server) exploreStart(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	var in core.ExploreRequest
	if err := body(r, &in); err != nil {
		return nil, err
	}
	in.BackupID = b.ID
	if s.App.ExploreReady(b, in.DB, in.Table) {
		return map[string]any{"ready": true}, nil
	}
	if id := s.activeJob("explore", b.ID); id > 0 {
		return map[string]any{"ready": false, "jobId": id, "busy": true}, nil
	}
	payload, _ := json.Marshal(in)
	title := "Open in sandbox: " + b.ID
	if in.Table != "" {
		title = "Load " + in.DB + "." + in.Table + " into the sandbox"
	}
	j := &core.Job{Kind: "explore", BackupID: b.ID, Title: title, Payload: payload, MaxAttempts: 1, Priority: 8}
	if err := s.App.Queue.Enqueue(j); err != nil {
		return nil, err
	}
	s.log(r, "backup.explore", b.ID+" "+in.DB+"."+in.Table)
	return map[string]any{"ready": false, "jobId": j.ID}, nil
}

func (s *Server) exploreStop(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.backup(r)
	if err != nil {
		return nil, err
	}
	if id := s.activeJob("explore", b.ID); id > 0 {
		return nil, bad("the sandbox is being loaded; cancel that job first")
	}
	s.App.RemoveSandbox(b.ID, "explore")
	return map[string]bool{"ok": true}, nil
}

func csvSafe(v any) string {
	if v == nil {
		return ""
	}
	t := fmt.Sprint(v)
	if t != "" && strings.ContainsRune("=+-@\t\r", rune(t[0])) {
		return "'" + t // a spreadsheet must not run a cell as a formula
	}
	return t
}

func (s *Server) backupRows(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	rq := core.RowQuery{DB: q.Get("db"), Table: q.Get("table"), Page: int(qInt(r, "page")), PageSize: int(qInt(r, "pageSize")), Sort: q.Get("sort"), Dir: q.Get("dir"), Search: q.Get("search")}
	if f := q.Get("filters"); f != "" {
		if err := json.Unmarshal([]byte(f), &rq.Filters); err != nil || len(rq.Filters) > 20 {
			return nil, bad("filters are not valid")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if q.Get("format") != "csv" {
		page, err := s.App.QueryRows(ctx, b, rq, nil)
		if err != nil {
			return nil, bad("%v", err)
		}
		return page, nil
	}
	s.log(r, "backup.rows.export", b.ID+" "+rq.DB+"."+rq.Table)
	var cw *csv.Writer
	_, err = s.App.QueryRows(ctx, b, rq, func(cols []core.RowColumn, row []any) error {
		if cw == nil {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, strings.Map(safeName, rq.Table)))
			w.Write([]byte("\xEF\xBB\xBF"))
			cw = csv.NewWriter(w)
			head := make([]string, len(cols))
			for i, c := range cols {
				head[i] = c.Name
			}
			cw.Write(head)
		}
		rec := make([]string, len(row))
		for i, v := range row {
			rec[i] = csvSafe(v)
		}
		return cw.Write(rec)
	})
	if cw == nil {
		if err != nil {
			return nil, bad("%v", err)
		}
		return nil, bad("no rows match")
	}
	cw.Flush()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil, nil
}

func (s *Server) backupTest(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	if id := s.activeJob("restore-test", b.ID); id > 0 {
		return nil, bad("a restore test of this backup is already waiting or running")
	}
	j := &core.Job{Kind: "restore-test", BackupID: b.ID, Title: "Restore test: " + b.ID, MaxAttempts: 1, Priority: 2}
	s.log(r, "backup.test", b.ID)
	return j, s.App.Queue.Enqueue(j)
}

var reTableName = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

// backupRestoreTable copies one table of a SQL backup into a live database under a NEW name. It
// refuses an existing name and never drops or replaces anything.
func (s *Server) backupRestoreTable(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	var in struct {
		core.TableRestoreRequest
		Password string
	}
	if err := body(r, &in); err != nil {
		return nil, err
	}
	req := in.TableRestoreRequest
	req.BackupID = b.ID
	if req.ToDB == "" {
		req.ToDB = req.DB
	}
	if req.ToTable == "" {
		req.ToTable = req.Table + "__restored_" + time.Now().UTC().Format("20060102_1504")
	}
	if !reTableName.MatchString(req.ToTable) || !reTableName.MatchString(req.ToDB) {
		return nil, bad("the new table and database names may use letters, digits, _ and $ only (64 at most)")
	}
	if strings.TrimSpace(req.User) == "" {
		return nil, bad("give a database user that may create tables (the backup user is read-only)")
	}
	if _, err := s.App.Store.Endpoint("targets", req.TargetID); err != nil {
		return nil, bad("choose the database to restore into")
	}
	if req.Secret, err = s.App.Keys.SealHex(in.Password); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(req)
	j := &core.Job{Kind: "table-restore", BackupID: b.ID, TargetID: req.TargetID, Title: fmt.Sprintf("Restore table %s.%s as %s.%s", req.DB, req.Table, req.ToDB, req.ToTable), Payload: payload, MaxAttempts: 1, Priority: 9}
	s.log(r, "table.restore.start", fmt.Sprintf("%s %s.%s -> target %d %s.%s as %s", b.ID, req.DB, req.Table, req.TargetID, req.ToDB, req.ToTable, req.User))
	return j, s.App.Queue.Enqueue(j)
}

func (s *Server) backupFiles(w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.usable(r)
	if err != nil {
		return nil, err
	}
	items, total, err := s.App.ListFiles(r.Context(), b, r.URL.Query().Get("search"), int(qInt(r, "page")), int(qInt(r, "pageSize")))
	if err != nil {
		return nil, bad("%v", err)
	}
	return map[string]any{"items": items, "total": total}, nil
}

// ---- backup sets ----

func (s *Server) setList(w http.ResponseWriter, r *http.Request) (any, error) { return s.App.Sets() }

func (s *Server) setSave(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PlanSet
	if err := body(r, &in); err != nil {
		return nil, err
	}
	in.ID = pathID(r)
	in.Name = strings.TrimSpace(in.Name)
	if err := s.App.SaveSet(&in); err != nil {
		return nil, bad("%v", err)
	}
	s.log(r, "set.save", in.Name)
	return in, nil
}

func (s *Server) setDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	_, err := s.App.Store.DB.Exec(`DELETE FROM plan_sets WHERE id=?`, pathID(r))
	s.log(r, "set.delete", r.PathValue("id"))
	return map[string]bool{"ok": true}, err
}

func (s *Server) setRun(w http.ResponseWriter, r *http.Request) (any, error) {
	set, err := s.App.Set(pathID(r))
	if err != nil {
		return nil, &httpError{404, "backup set not found"}
	}
	run, err := s.App.RunSet(set, "manual")
	if err != nil {
		return nil, bad("%v", err)
	}
	s.log(r, "set.run", set.Name+" "+run)
	return map[string]string{"run": run}, nil
}

func (s *Server) setRuns(w http.ResponseWriter, r *http.Request) (any, error) {
	set, err := s.App.Set(pathID(r))
	if err != nil {
		return nil, &httpError{404, "backup set not found"}
	}
	runs, err := s.App.SetRuns(set, 10)
	if runs == nil {
		runs = []core.SetRun{}
	}
	return runs, err
}

func safeName(r rune) rune {
	if r == '"' || r == '/' || r == '\\' || r < 32 {
		return '_'
	}
	return r
}

// ---- jobs, events, settings ----

func (s *Server) jobList(w http.ResponseWriter, r *http.Request) (any, error) {
	size := int(qInt(r, "pageSize"))
	if size <= 0 || size > 200 {
		size = 20
	}
	page := int(qInt(r, "page"))
	if page < 1 {
		page = 1
	}
	list, total, err := s.App.Queue.List(size, (page-1)*size, r.URL.Query().Get("state"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": list, "total": total}, nil
}

func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) (any, error) {
	if err := s.App.Queue.Cancel(pathID(r)); err != nil {
		return nil, bad("%v", err)
	}
	s.log(r, "job.cancel", r.PathValue("id"))
	return map[string]bool{"ok": true}, nil
}

func (s *Server) hold(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ On bool }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	s.App.Queue.Hold(in.On)
	s.log(r, "queue.hold", fmt.Sprint(in.On))
	return map[string]bool{"held": in.On}, nil
}

// events is one long-lived push connection per open panel (no polling).
func (s *Server) events(w http.ResponseWriter, r *http.Request) (any, error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming is not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := s.App.Hub.Subscribe()
	defer stop()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil, nil
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) (any, error) {
	db := s.App.Store.DB
	out := map[string]any{}
	var total, failed7 int
	var stored int64
	var last int64
	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(stored_bytes),0), COALESCE(MAX(finished_at),0) FROM backups WHERE status='done' AND deleted_at=0`).Scan(&total, &stored, &last)
	db.QueryRow(`SELECT COUNT(*) FROM backups WHERE status='failed' AND created_at>?`, time.Now().AddDate(0, 0, -7).Unix()).Scan(&failed7)
	var plans, targets, running, queued int
	db.QueryRow(`SELECT COUNT(*) FROM plans WHERE enabled=1`).Scan(&plans)
	db.QueryRow(`SELECT COUNT(*) FROM targets`).Scan(&targets)
	db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='running'`).Scan(&running)
	db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='queued'`).Scan(&queued)
	var next int64
	db.QueryRow(`SELECT COALESCE(MIN(next_run_at),0) FROM plans WHERE enabled=1 AND next_run_at>0`).Scan(&next)
	out["backups"], out["storedBytes"], out["lastBackupAt"], out["failed7d"] = total, stored, last, failed7
	out["plans"], out["targets"], out["running"], out["queued"], out["nextRunAt"], out["held"] = plans, targets, running, queued, next, s.App.Queue.Held()
	return out, nil
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) (any, error) {
	st := s.App.Store
	return map[string]any{"webhookUrl": st.Setting("webhook_url"), "telegramChat": st.Setting("telegram_chat"),
		"telegramTokenSet": st.Setting("telegram_token") != "", "notifySuccess": st.Setting("notify_success") == "1"}, nil
}

func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		WebhookURL, TelegramChat string
		TelegramToken            *string
		NotifySuccess            bool
		Test                     bool
	}
	if err := body(r, &in); err != nil {
		return nil, err
	}
	if in.WebhookURL != "" && !strings.HasPrefix(in.WebhookURL, "https://") && !strings.HasPrefix(in.WebhookURL, "http://") {
		return nil, bad("the webhook address must start with http:// or https://")
	}
	st := s.App.Store
	st.SetSetting("webhook_url", strings.TrimSpace(in.WebhookURL))
	st.SetSetting("telegram_chat", strings.TrimSpace(in.TelegramChat))
	ns := ""
	if in.NotifySuccess {
		ns = "1"
	}
	st.SetSetting("notify_success", ns)
	if in.TelegramToken != nil && *in.TelegramToken != "" {
		sealed, err := s.App.Keys.SealHex(strings.TrimSpace(*in.TelegramToken))
		if err != nil {
			return nil, err
		}
		st.SetSetting("telegram_token", sealed)
	}
	s.log(r, "settings.save", "notifications")
	if in.Test {
		s.App.Notify("failure", "Backup Manager: this is a test notification.")
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) (any, error) {
	size, page := 50, int(qInt(r, "page"))
	if page < 1 {
		page = 1
	}
	var total int
	s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&total)
	rows, err := s.App.Store.DB.Query(`SELECT at,actor,action,detail,ip FROM audit ORDER BY id DESC LIMIT ? OFFSET ?`, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var at int64
		var actor, action, detail, ip string
		rows.Scan(&at, &actor, &action, &detail, &ip)
		items = append(items, map[string]any{"at": at, "actor": actor, "action": action, "detail": detail, "ip": ip})
	}
	return map[string]any{"items": items, "total": total}, nil
}
