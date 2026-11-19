package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the manager's own catalog (SQLite file).
type Store struct{ DB *sql.DB }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, pass TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'owner', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS targets(id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, cfg TEXT NOT NULL, secret BLOB, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS destinations(id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, cfg TEXT NOT NULL, secret BLOB, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS plans(id INTEGER PRIMARY KEY, name TEXT NOT NULL, target_id INTEGER NOT NULL, destination_id INTEGER NOT NULL,
  spec TEXT NOT NULL, schedule TEXT NOT NULL DEFAULT '', keep_days INTEGER NOT NULL DEFAULT 0, keep_last INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1, next_run_at INTEGER NOT NULL DEFAULT 0, last_run_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS backups(id TEXT PRIMARY KEY, plan_id INTEGER NOT NULL, target_id INTEGER NOT NULL, destination_id INTEGER NOT NULL,
  plan_name TEXT NOT NULL, target_name TEXT NOT NULL, destination_name TEXT NOT NULL, content TEXT NOT NULL, scope TEXT NOT NULL,
  status TEXT NOT NULL, trigger TEXT NOT NULL DEFAULT 'manual', created_at INTEGER NOT NULL, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
  raw_bytes INTEGER NOT NULL DEFAULT 0, stored_bytes INTEGER NOT NULL DEFAULT 0, tables INTEGER NOT NULL DEFAULT 0, row_count INTEGER NOT NULL DEFAULT 0,
  encrypted INTEGER NOT NULL DEFAULT 0, wrapped_key BLOB, manifest TEXT NOT NULL DEFAULT '', pinned INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', deleted_at INTEGER NOT NULL DEFAULT 0, delete_reason TEXT NOT NULL DEFAULT '',
  verified_at INTEGER NOT NULL DEFAULT 0, verify_error TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS backups_plan ON backups(plan_id, created_at);
CREATE INDEX IF NOT EXISTS backups_created ON backups(created_at);
CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, state TEXT NOT NULL, target_id INTEGER NOT NULL DEFAULT 0,
  plan_id INTEGER NOT NULL DEFAULT 0, backup_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', priority INTEGER NOT NULL DEFAULT 0,
  attempt INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 3, run_after INTEGER NOT NULL DEFAULT 0, progress TEXT NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '', cancel INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, started_at INTEGER NOT NULL DEFAULT 0,
  heartbeat_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state, run_after);
CREATE TABLE IF NOT EXISTS binlogs(target_id INTEGER NOT NULL, destination_id INTEGER NOT NULL, name TEXT NOT NULL, size INTEGER NOT NULL, closed INTEGER NOT NULL,
  obj_key TEXT NOT NULL, wrapped_key BLOB, archived_at INTEGER NOT NULL, PRIMARY KEY(target_id, destination_id, name));
CREATE TABLE IF NOT EXISTS sandboxes(backup_id TEXT NOT NULL, purpose TEXT NOT NULL, container TEXT NOT NULL, port INTEGER NOT NULL, secret BLOB, full INTEGER NOT NULL DEFAULT 0,
  loaded TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL, last_used INTEGER NOT NULL, PRIMARY KEY(backup_id, purpose));
CREATE TABLE IF NOT EXISTS plan_sets(id INTEGER PRIMARY KEY, name TEXT NOT NULL, plan_ids TEXT NOT NULL, schedule TEXT NOT NULL DEFAULT '', next_run_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tokens(id INTEGER PRIMARY KEY, name TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, role TEXT NOT NULL, created_at INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS agents(id INTEGER PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, hostname TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '',
  last_seen INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL, ip TEXT NOT NULL);
`

func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("catalog schema: %w", err)
	}
	// columns added after the first release; "duplicate column" just means it is already there
	for _, col := range []string{"method TEXT NOT NULL DEFAULT 'logical'", "parent_id TEXT NOT NULL DEFAULT ''", "chain_id TEXT NOT NULL DEFAULT ''", "lsn_to TEXT NOT NULL DEFAULT ''",
		"tested_at INTEGER NOT NULL DEFAULT 0", "test_error TEXT NOT NULL DEFAULT ''", "test_seconds INTEGER NOT NULL DEFAULT 0", "test_detail TEXT NOT NULL DEFAULT ''", "set_run TEXT NOT NULL DEFAULT ''"} {
		if _, err := db.Exec(`ALTER TABLE backups ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("catalog upgrade: %w", err)
		}
	}
	for _, col := range []string{"totp_secret BLOB", "totp_on INTEGER NOT NULL DEFAULT 0", "recovery TEXT NOT NULL DEFAULT ''"} {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("catalog upgrade: %w", err)
		}
	}
	return &Store{db}, nil
}

func now() int64 { return time.Now().Unix() }

func (s *Store) Setting(k string) string {
	var v string
	s.DB.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) SetSetting(k, v string) error {
	_, err := s.DB.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) Audit(actor, action, detail, ip string) {
	s.DB.Exec(`INSERT INTO audit(at,actor,action,detail,ip) VALUES(?,?,?,?,?)`, now(), actor, action, detail, ip)
}

// ---- targets / destinations (same shape) ----

type Endpoint struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Cfg       json.RawMessage `json:"cfg"`
	HasSecret bool            `json:"hasSecret"`
	CreatedAt int64           `json:"createdAt"`
	secret    []byte
}

func endpointTable(t string) string {
	if t == "targets" {
		return "targets"
	}
	return "destinations"
}

func (s *Store) Endpoints(table string) ([]Endpoint, error) {
	rows, err := s.DB.Query(`SELECT id,name,kind,cfg,secret,created_at FROM ` + endpointTable(table) + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		var e Endpoint
		var cfg string
		if err := rows.Scan(&e.ID, &e.Name, &e.Kind, &cfg, &e.secret, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Cfg, e.HasSecret = json.RawMessage(cfg), len(e.secret) > 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Endpoint(table string, id int64) (*Endpoint, error) {
	var e Endpoint
	var cfg string
	err := s.DB.QueryRow(`SELECT id,name,kind,cfg,secret,created_at FROM `+endpointTable(table)+` WHERE id=?`, id).
		Scan(&e.ID, &e.Name, &e.Kind, &cfg, &e.secret, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.Cfg, e.HasSecret = json.RawMessage(cfg), len(e.secret) > 0
	return &e, nil
}

// SaveEndpoint inserts or updates. A nil sealed secret keeps the stored one.
func (s *Store) SaveEndpoint(table string, e *Endpoint, sealed []byte) error {
	t := endpointTable(table)
	if e.ID == 0 {
		r, err := s.DB.Exec(`INSERT INTO `+t+`(name,kind,cfg,secret,created_at) VALUES(?,?,?,?,?)`, e.Name, e.Kind, string(e.Cfg), sealed, now())
		if err != nil {
			return err
		}
		e.ID, _ = r.LastInsertId()
		return nil
	}
	if sealed != nil {
		_, err := s.DB.Exec(`UPDATE `+t+` SET name=?,kind=?,cfg=?,secret=? WHERE id=?`, e.Name, e.Kind, string(e.Cfg), sealed, e.ID)
		return err
	}
	_, err := s.DB.Exec(`UPDATE `+t+` SET name=?,kind=?,cfg=? WHERE id=?`, e.Name, e.Kind, string(e.Cfg), e.ID)
	return err
}

// ---- plans ----

type Plan struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	TargetID      int64  `json:"targetId"`
	DestinationID int64  `json:"destinationId"`
	Spec          Spec   `json:"spec"`
	Schedule      string `json:"schedule"`
	KeepDays      int    `json:"keepDays"`
	KeepLast      int    `json:"keepLast"`
	Enabled       bool   `json:"enabled"`
	NextRunAt     int64  `json:"nextRunAt"`
	LastRunAt     int64  `json:"lastRunAt"`
	CreatedAt     int64  `json:"createdAt"`
}

const planCols = `id,name,target_id,destination_id,spec,schedule,keep_days,keep_last,enabled,next_run_at,last_run_at,created_at`

type scanner interface{ Scan(...any) error }

func scanPlan(r scanner) (*Plan, error) {
	var p Plan
	var spec string
	if err := r.Scan(&p.ID, &p.Name, &p.TargetID, &p.DestinationID, &spec, &p.Schedule, &p.KeepDays, &p.KeepLast, &p.Enabled, &p.NextRunAt, &p.LastRunAt, &p.CreatedAt); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(spec), &p.Spec)
	return &p, nil
}

func (s *Store) Plans(where string, args ...any) ([]*Plan, error) {
	q := `SELECT ` + planCols + ` FROM plans`
	if where != "" {
		q += ` WHERE ` + where
	}
	rows, err := s.DB.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Plan(id int64) (*Plan, error) {
	return scanPlan(s.DB.QueryRow(`SELECT `+planCols+` FROM plans WHERE id=?`, id))
}

func (s *Store) SavePlan(p *Plan) error {
	spec, _ := json.Marshal(p.Spec)
	if p.ID == 0 {
		r, err := s.DB.Exec(`INSERT INTO plans(name,target_id,destination_id,spec,schedule,keep_days,keep_last,enabled,next_run_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.TargetID, p.DestinationID, string(spec), p.Schedule, p.KeepDays, p.KeepLast, p.Enabled, p.NextRunAt, now())
		if err != nil {
			return err
		}
		p.ID, _ = r.LastInsertId()
		return nil
	}
	_, err := s.DB.Exec(`UPDATE plans SET name=?,target_id=?,destination_id=?,spec=?,schedule=?,keep_days=?,keep_last=?,enabled=?,next_run_at=? WHERE id=?`,
		p.Name, p.TargetID, p.DestinationID, string(spec), p.Schedule, p.KeepDays, p.KeepLast, p.Enabled, p.NextRunAt, p.ID)
	return err
}

// ---- backups ----

type Backup struct {
	ID              string `json:"id"`
	PlanID          int64  `json:"planId"`
	TargetID        int64  `json:"targetId"`
	DestinationID   int64  `json:"destinationId"`
	PlanName        string `json:"planName"`
	TargetName      string `json:"targetName"`
	DestinationName string `json:"destinationName"`
	Content         string `json:"content"`
	Scope           string `json:"scope"`
	Status          string `json:"status"`
	Trigger         string `json:"trigger"`
	CreatedAt       int64  `json:"createdAt"`
	StartedAt       int64  `json:"startedAt"`
	FinishedAt      int64  `json:"finishedAt"`
	RawBytes        int64  `json:"rawBytes"`
	StoredBytes     int64  `json:"storedBytes"`
	Tables          int    `json:"tables"`
	Rows            int64  `json:"rows"`
	Encrypted       bool   `json:"encrypted"`
	Pinned          bool   `json:"pinned"`
	Note            string `json:"note"`
	Error           string `json:"error"`
	DeletedAt       int64  `json:"deletedAt"`
	DeleteReason    string `json:"deleteReason"`
	VerifiedAt      int64  `json:"verifiedAt"`
	VerifyError     string `json:"verifyError"`
	Method          string `json:"method"`
	ParentID        string `json:"parentId"`
	ChainID         string `json:"chainId"`
	LSNTo           string `json:"lsnTo"`
	TestedAt        int64  `json:"testedAt"`
	TestError       string `json:"testError"`
	TestSeconds     int    `json:"testSeconds"`
	TestDetail      string `json:"testDetail"`
	SetRun          string `json:"setRun"`
	wrappedKey      []byte
}

const backupCols = `id,plan_id,target_id,destination_id,plan_name,target_name,destination_name,content,scope,status,trigger,created_at,started_at,finished_at,raw_bytes,stored_bytes,tables,row_count,encrypted,pinned,note,error,deleted_at,delete_reason,verified_at,verify_error,method,parent_id,chain_id,lsn_to,tested_at,test_error,test_seconds,test_detail,set_run,wrapped_key`

func scanBackup(r scanner) (*Backup, error) {
	var b Backup
	err := r.Scan(&b.ID, &b.PlanID, &b.TargetID, &b.DestinationID, &b.PlanName, &b.TargetName, &b.DestinationName, &b.Content, &b.Scope, &b.Status, &b.Trigger,
		&b.CreatedAt, &b.StartedAt, &b.FinishedAt, &b.RawBytes, &b.StoredBytes, &b.Tables, &b.Rows, &b.Encrypted, &b.Pinned, &b.Note, &b.Error, &b.DeletedAt, &b.DeleteReason, &b.VerifiedAt, &b.VerifyError, &b.Method, &b.ParentID, &b.ChainID, &b.LSNTo, &b.TestedAt, &b.TestError, &b.TestSeconds, &b.TestDetail, &b.SetRun, &b.wrappedKey)
	return &b, err
}

func (s *Store) Backup(id string) (*Backup, error) {
	return scanBackup(s.DB.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id=?`, id))
}

func (s *Store) BackupManifest(id string) (string, error) {
	var m string
	err := s.DB.QueryRow(`SELECT manifest FROM backups WHERE id=?`, id).Scan(&m)
	return m, err
}

type BackupFilter struct {
	Page, PageSize         int
	Search, Status         string
	PlanID, TargetID       int64
	From, To               int64
	Sort, Dir              string
	Pinned, IncludeDeleted bool
}

var backupSorts = map[string]string{"created": "created_at", "size": "stored_bytes", "rows": "row_count", "duration": "(finished_at-started_at)", "status": "status"}

// ListBackups returns one page and the total count for the same filter.
func (s *Store) ListBackups(f BackupFilter) ([]*Backup, int, error) {
	var w []string
	var a []any
	if !f.IncludeDeleted {
		w = append(w, "deleted_at=0")
	}
	if f.Status != "" {
		w, a = append(w, "status=?"), append(a, f.Status)
	}
	if f.PlanID > 0 {
		w, a = append(w, "plan_id=?"), append(a, f.PlanID)
	}
	if f.TargetID > 0 {
		w, a = append(w, "target_id=?"), append(a, f.TargetID)
	}
	if f.From > 0 {
		w, a = append(w, "created_at>=?"), append(a, f.From)
	}
	if f.To > 0 {
		w, a = append(w, "created_at<=?"), append(a, f.To)
	}
	if f.Pinned {
		w = append(w, "pinned=1")
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		w = append(w, `(id LIKE ? ESCAPE '\' OR plan_name LIKE ? ESCAPE '\' OR target_name LIKE ? ESCAPE '\' OR note LIKE ? ESCAPE '\' OR scope LIKE ? ESCAPE '\')`)
		a = append(a, like, like, like, like, like)
	}
	where := ""
	if len(w) > 0 {
		where = " WHERE " + strings.Join(w, " AND ")
	}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM backups`+where, a...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 20
	}
	if f.Page < 1 {
		f.Page = 1
	}
	col, ok := backupSorts[f.Sort]
	if !ok {
		col = "created_at"
	}
	dir := "DESC"
	if strings.EqualFold(f.Dir, "asc") {
		dir = "ASC"
	}
	rows, err := s.DB.Query(`SELECT `+backupCols+` FROM backups`+where+` ORDER BY `+col+` `+dir+`, id DESC LIMIT ? OFFSET ?`,
		append(a, f.PageSize, (f.Page-1)*f.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// PlanDoneBackups returns every finished, not deleted backup of a plan (for retention).
func (s *Store) PlanDoneBackups(planID int64) ([]*Backup, error) {
	rows, err := s.DB.Query(`SELECT `+backupCols+` FROM backups WHERE plan_id=? AND status='done' AND deleted_at=0 ORDER BY created_at DESC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SnapshotCatalog writes a consistent copy of the catalog to path while it is in use.
func SnapshotCatalog(st *Store, path string) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	if _, err := st.DB.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("copying the catalog: %w", err)
	}
	os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}
