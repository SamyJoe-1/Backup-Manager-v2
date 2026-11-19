package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"backupmanager/internal/core"

	"golang.org/x/crypto/bcrypt"
)

// Roles: viewer sees, operator runs and downloads, owner changes things.
var rank = map[string]int{"viewer": 1, "operator": 2, "owner": 3}

// operatorRoutes may be used by operators (and owners). Every other GET needs a viewer, every
// other changing request needs an owner.
var operatorRoutes = map[string]bool{
	"POST /api/plans/{id}/run": true, "POST /api/sets/{id}/run": true, "POST /api/backups/{id}/verify": true, "POST /api/backups/{id}/test": true,
	"POST /api/backups/{id}/explore": true, "DELETE /api/backups/{id}/explore": true, "GET /api/backups/{id}/rows": true, "GET /api/backups/{id}/download": true,
	"GET /api/backups/{id}/files": true, "POST /api/jobs/{id}/cancel": true, "POST /api/hold": true, "POST /api/backups/{id}/pin": true, "POST /api/backups/{id}/note": true,
}

var viewerRoutes = map[string]bool{"POST /api/logout": true, "POST /api/me/password": true, "POST /api/me/totp/start": true, "POST /api/me/totp/enable": true, "POST /api/me/totp/disable": true, "GET /api/me": true}

var ownerGets = map[string]bool{"GET /api/audit": true, "GET /api/settings": true, "GET /api/users": true, "GET /api/tokens": true, "GET /api/agents": true}

func levelFor(pattern string) int {
	switch {
	case viewerRoutes[pattern]:
		return 1
	case operatorRoutes[pattern]:
		return 2
	case ownerGets[pattern]:
		return 3
	case strings.HasPrefix(pattern, "GET "):
		return 1
	}
	return 3
}

// identify finds who is calling: a browser session, or an API token (Authorization: Bearer …).
func (s *Server) identify(r *http.Request) (u *user, viaToken bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		var t user
		var id int64
		err := s.App.Store.DB.QueryRow(`SELECT id,name,role FROM tokens WHERE hash=?`, hashToken(strings.TrimSpace(h[7:]))).Scan(&id, &t.Email, &t.Role)
		if err != nil {
			return nil, true
		}
		t.Email = "token:" + t.Email
		s.App.Store.DB.Exec(`UPDATE tokens SET last_used=? WHERE id=? AND last_used<?`, time.Now().Unix(), id, time.Now().Unix()-60)
		return &t, true
	}
	return s.currentUser(r), false
}

func (s *Server) guard(h handler, level int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, viaToken := s.identify(r)
		// Browser sessions must send a header a foreign site cannot send (CSRF). Tokens are not sent by browsers on their own.
		if !viaToken && r.Method != http.MethodGet && r.Header.Get("X-BM") != "1" {
			writeErr(w, &httpError{403, "missing request header"})
			return
		}
		if level > 0 {
			if u == nil {
				writeErr(w, &httpError{401, "please sign in"})
				return
			}
			if rank[u.Role] < level {
				writeErr(w, &httpError{403, "your role (" + u.Role + ") is not allowed to do this"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
		}
		out, err := h(w, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if out != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		}
	})
}

// ---- two-factor (TOTP, RFC 6238: 6 digits, 30 seconds, SHA-1; works with every authenticator app) ----

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpAt(secret []byte, step int64) string {
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg)
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code)
}

func totpValid(secret []byte, code string, now time.Time) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return false
	}
	ok := false
	for d := int64(-1); d <= 1; d++ { // one step of clock drift either way
		if hmac.Equal([]byte(totpAt(secret, now.Unix()/30+d)), []byte(code)) {
			ok = true
		}
	}
	return ok
}

func (s *Server) totpSecret(uid int64) (secret []byte, on bool) {
	var sealed []byte
	s.App.Store.DB.QueryRow(`SELECT totp_secret, totp_on FROM users WHERE id=?`, uid).Scan(&sealed, &on)
	secret, _ = s.App.Keys.Open(sealed)
	return secret, on && len(secret) > 0
}

// secondFactor checks a login code: the authenticator code, or a one-time recovery code.
func (s *Server) secondFactor(uid int64, code string) bool {
	secret, on := s.totpSecret(uid)
	if !on {
		return true
	}
	if totpValid(secret, code, time.Now()) {
		return true
	}
	var stored string
	s.App.Store.DB.QueryRow(`SELECT recovery FROM users WHERE id=?`, uid).Scan(&stored)
	var hashes []string
	json.Unmarshal([]byte(stored), &hashes)
	want := hashToken(strings.ToLower(strings.TrimSpace(code)))
	for i, h := range hashes {
		if hmac.Equal([]byte(h), []byte(want)) && code != "" {
			hashes = append(hashes[:i], hashes[i+1:]...)
			left, _ := json.Marshal(hashes)
			s.App.Store.DB.Exec(`UPDATE users SET recovery=? WHERE id=?`, string(left), uid)
			return true
		}
	}
	return false
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) (any, error) {
	u := me(r)
	_, on := s.totpSecret(u.ID)
	return map[string]any{"user": u, "totp": on}, nil
}

func (s *Server) checkOwnPassword(r *http.Request, password string) error {
	var hash string
	s.App.Store.DB.QueryRow(`SELECT pass FROM users WHERE id=?`, me(r).ID).Scan(&hash)
	if me(r).ID == 0 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return &httpError{403, "your current password is wrong"}
	}
	return nil
}

func (s *Server) mePassword(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Current, New string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	if err := s.checkOwnPassword(r, in.Current); err != nil {
		return nil, err
	}
	if err := checkPassword(in.New); err != nil {
		return nil, err
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(in.New), 12)
	s.App.Store.DB.Exec(`UPDATE users SET pass=? WHERE id=?`, string(h), me(r).ID)
	// every other session of this account ends
	if c, err := r.Cookie("bm_session"); err == nil {
		s.App.Store.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, me(r).ID, hashToken(c.Value))
	}
	s.log(r, "password.change", "")
	return map[string]bool{"ok": true}, nil
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) (any, error) {
	u := me(r)
	if u.ID == 0 {
		return nil, bad("API tokens have no two-factor sign-in")
	}
	if _, on := s.totpSecret(u.ID); on {
		return nil, bad("two-factor sign-in is already on")
	}
	secret := make([]byte, 20)
	rand.Read(secret)
	sealed, err := s.App.Keys.Seal(secret)
	if err != nil {
		return nil, err
	}
	s.App.Store.DB.Exec(`UPDATE users SET totp_secret=?, totp_on=0 WHERE id=?`, sealed, u.ID)
	text := b32.EncodeToString(secret)
	uri := "otpauth://totp/" + url.PathEscape("Backup Manager:"+u.Email) + "?secret=" + text + "&issuer=" + url.QueryEscape("Backup Manager")
	return map[string]string{"secret": text, "uri": uri}, nil
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Code string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	u := me(r)
	var sealed []byte
	s.App.Store.DB.QueryRow(`SELECT totp_secret FROM users WHERE id=?`, u.ID).Scan(&sealed)
	secret, _ := s.App.Keys.Open(sealed)
	if len(secret) == 0 || !totpValid(secret, in.Code, time.Now()) {
		return nil, bad("that code is not right; check the clock of your phone and try the next code")
	}
	codes := make([]string, 8)
	hashes := make([]string, 8)
	for i := range codes {
		b := make([]byte, 5)
		rand.Read(b)
		codes[i] = hex.EncodeToString(b)
		hashes[i] = hashToken(codes[i])
	}
	hj, _ := json.Marshal(hashes)
	s.App.Store.DB.Exec(`UPDATE users SET totp_on=1, recovery=? WHERE id=?`, string(hj), u.ID)
	s.log(r, "totp.enable", "")
	return map[string]any{"recovery": codes}, nil
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Password string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	if err := s.checkOwnPassword(r, in.Password); err != nil {
		return nil, err
	}
	s.App.Store.DB.Exec(`UPDATE users SET totp_on=0, totp_secret=NULL, recovery='' WHERE id=?`, me(r).ID)
	s.log(r, "totp.disable", "")
	return map[string]bool{"ok": true}, nil
}

// ---- accounts ----

func (s *Server) owners(except int64) int {
	var n int
	s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE role='owner' AND id<>?`, except).Scan(&n)
	return n
}

func (s *Server) userList(w http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.App.Store.DB.Query(`SELECT id,email,role,totp_on,created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var email, role string
		var totp bool
		rows.Scan(&id, &email, &role, &totp, &created)
		out = append(out, map[string]any{"id": id, "email": email, "role": role, "totp": totp, "createdAt": created})
	}
	return out, nil
}

func (s *Server) userSave(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Email, Password, Role string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	if rank[in.Role] == 0 {
		return nil, bad("role must be owner, operator or viewer")
	}
	id := pathID(r)
	if id == 0 {
		nid, err := CreateUser(s.App.Store, in.Email, in.Password)
		if err != nil {
			return nil, err
		}
		s.App.Store.DB.Exec(`UPDATE users SET role=? WHERE id=?`, in.Role, nid)
		s.log(r, "user.create", in.Email+" as "+in.Role)
		return map[string]int64{"id": nid}, nil
	}
	if in.Role != "owner" && s.owners(id) == 0 {
		return nil, bad("this is the only owner; make someone else an owner first")
	}
	s.App.Store.DB.Exec(`UPDATE users SET role=? WHERE id=?`, in.Role, id)
	if in.Password != "" {
		if err := checkPassword(in.Password); err != nil {
			return nil, err
		}
		h, _ := bcrypt.GenerateFromPassword([]byte(in.Password), 12)
		s.App.Store.DB.Exec(`UPDATE users SET pass=? WHERE id=?`, string(h), id)
		s.App.Store.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	}
	s.log(r, "user.update", strconv.FormatInt(id, 10)+" role "+in.Role)
	return map[string]int64{"id": id}, nil
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	id := pathID(r)
	if id == me(r).ID {
		return nil, bad("you cannot delete your own account")
	}
	if s.owners(id) == 0 {
		return nil, bad("this is the only owner")
	}
	s.App.Store.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	s.App.Store.DB.Exec(`DELETE FROM users WHERE id=?`, id)
	s.log(r, "user.delete", strconv.FormatInt(id, 10))
	return map[string]bool{"ok": true}, nil
}

// ---- API tokens ----

func (s *Server) tokenList(w http.ResponseWriter, r *http.Request) (any, error) {
	rows, err := s.App.Store.DB.Query(`SELECT id,name,role,created_at,last_used FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created, used int64
		var name, role string
		rows.Scan(&id, &name, &role, &created, &used)
		out = append(out, map[string]any{"id": id, "name": name, "role": role, "createdAt": created, "lastUsed": used})
	}
	return out, nil
}

// NewToken creates an API token and returns it; only its hash is stored.
func NewToken(st *core.Store, name, role string) (string, error) {
	if rank[role] == 0 {
		return "", bad("role must be owner, operator or viewer")
	}
	if strings.TrimSpace(name) == "" {
		return "", bad("give the token a name")
	}
	tok := "bm_" + randomToken()[:40]
	_, err := st.DB.Exec(`INSERT INTO tokens(name,hash,role,created_at) VALUES(?,?,?,?)`, strings.TrimSpace(name), hashToken(tok), role, time.Now().Unix())
	return tok, err
}

func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Name, Role string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	tok, err := NewToken(s.App.Store, in.Name, in.Role)
	if err != nil {
		return nil, err
	}
	s.log(r, "token.create", in.Name+" as "+in.Role)
	return map[string]string{"token": tok}, nil
}

func (s *Server) tokenDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	s.App.Store.DB.Exec(`DELETE FROM tokens WHERE id=?`, pathID(r))
	s.log(r, "token.delete", r.PathValue("id"))
	return map[string]bool{"ok": true}, nil
}

// ---- metrics (Prometheus text format) ----

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) (any, error) {
	db := s.App.Store.DB
	var b strings.Builder
	one := func(name, help, query string, args ...any) {
		var v float64
		db.QueryRow(query, args...).Scan(&v)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	week := time.Now().AddDate(0, 0, -7).Unix()
	one("backup_manager_backups", "Good backups currently stored.", `SELECT COUNT(*) FROM backups WHERE status='done' AND deleted_at=0`)
	one("backup_manager_stored_bytes", "Bytes used by stored backups.", `SELECT COALESCE(SUM(stored_bytes),0) FROM backups WHERE status='done' AND deleted_at=0`)
	one("backup_manager_last_success_timestamp_seconds", "When the newest good backup finished.", `SELECT COALESCE(MAX(finished_at),0) FROM backups WHERE status='done'`)
	one("backup_manager_failed_7d", "Backups that failed in the last 7 days.", `SELECT COUNT(*) FROM backups WHERE status='failed' AND created_at>?`, week)
	one("backup_manager_jobs_running", "Jobs running now.", `SELECT COUNT(*) FROM jobs WHERE state='running'`)
	one("backup_manager_jobs_queued", "Jobs waiting.", `SELECT COUNT(*) FROM jobs WHERE state='queued'`)
	one("backup_manager_restore_tests_failed", "Stored backups whose restore test failed.", `SELECT COUNT(*) FROM backups WHERE deleted_at=0 AND test_error<>''`)
	one("backup_manager_binlog_last_archive_timestamp_seconds", "When binary logs were last archived.", `SELECT COALESCE(MAX(archived_at),0) FROM binlogs`)
	fmt.Fprint(&b, "# HELP backup_manager_plan_last_success_timestamp_seconds When each plan last produced a good backup.\n# TYPE backup_manager_plan_last_success_timestamp_seconds gauge\n")
	rows, err := db.Query(`SELECT p.name, COALESCE(MAX(b.finished_at),0) FROM plans p LEFT JOIN backups b ON b.plan_id=p.id AND b.status='done' GROUP BY p.id ORDER BY p.id`)
	if err == nil {
		for rows.Next() {
			var name string
			var at int64
			rows.Scan(&name, &at)
			fmt.Fprintf(&b, "backup_manager_plan_last_success_timestamp_seconds{plan=%q} %d\n", name, at)
		}
		rows.Close()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
	return nil, nil
}
