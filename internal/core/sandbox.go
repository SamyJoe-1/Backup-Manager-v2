package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// A sandbox is a throwaway MariaDB container holding (part of) one backup, so its rows can be
// searched or a restore can be proven, without touching any real database. It listens on this
// machine only, has its own random passwords, a memory limit, and is removed when idle.
type Sandbox struct {
	BackupID  string
	Purpose   string // explore | test
	Container string
	Port      int
	Full      bool            // the whole backup is loaded (physical backups, restore tests)
	Loaded    map[string]bool // "db.table" loaded so far (logical backups)
	LastUsed  int64
	pass      string
}

const sandboxIdle = 30 * time.Minute

var reMariaVersion = regexp.MustCompile(`^(\d+\.\d+)\.\d+-MariaDB`)

func (a *App) sandboxImage(serverVersion string) (string, error) {
	if img := os.Getenv("BM_SANDBOX_IMAGE"); img != "" {
		return img, nil
	}
	if img := a.Store.Setting("sandbox_image"); img != "" {
		return img, nil
	}
	m := reMariaVersion.FindStringSubmatch(serverVersion)
	if m == nil {
		return "", fmt.Errorf("the sandbox supports MariaDB backups only for now (this backup is from %q)", serverVersion)
	}
	return "mariadb:" + m[1], nil
}

func (a *App) sandboxMemory() string {
	if v := os.Getenv("BM_SANDBOX_MEMORY"); v != "" {
		return v
	}
	if v := a.Store.Setting("sandbox_memory"); v != "" {
		return v
	}
	return "512m"
}

func (a *App) Sandbox(backupID, purpose string) *Sandbox {
	var sb Sandbox
	var sealed []byte
	var loaded string
	err := a.Store.DB.QueryRow(`SELECT backup_id,purpose,container,port,secret,full,loaded,last_used FROM sandboxes WHERE backup_id=? AND purpose=?`, backupID, purpose).
		Scan(&sb.BackupID, &sb.Purpose, &sb.Container, &sb.Port, &sealed, &sb.Full, &loaded, &sb.LastUsed)
	if err != nil {
		return nil
	}
	pw, err := a.Keys.Open(sealed)
	if err != nil {
		return nil
	}
	sb.pass = string(pw)
	sb.Loaded = map[string]bool{}
	json.Unmarshal([]byte(loaded), &sb.Loaded)
	return &sb
}

func (a *App) saveSandbox(sb *Sandbox) error {
	sealed, err := a.Keys.Seal([]byte(sb.pass))
	if err != nil {
		return err
	}
	loaded, _ := json.Marshal(sb.Loaded)
	_, err = a.Store.DB.Exec(`INSERT INTO sandboxes(backup_id,purpose,container,port,secret,full,loaded,created_at,last_used) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(backup_id,purpose) DO UPDATE SET container=excluded.container, port=excluded.port, secret=excluded.secret, full=excluded.full, loaded=excluded.loaded, last_used=excluded.last_used`,
		sb.BackupID, sb.Purpose, sb.Container, sb.Port, sealed, sb.Full, string(loaded), now(), now())
	return err
}

func (a *App) touchSandbox(sb *Sandbox) {
	a.Store.DB.Exec(`UPDATE sandboxes SET last_used=? WHERE backup_id=? AND purpose=?`, now(), sb.BackupID, sb.Purpose)
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", args[0], err, lastLines(string(out), 4))
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoveSandbox deletes the container with its data and forgets it.
func (a *App) RemoveSandbox(backupID, purpose string) {
	name := sandboxName(backupID, purpose)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	docker(ctx, "rm", "-fv", name)
	a.Store.DB.Exec(`DELETE FROM sandboxes WHERE backup_id=? AND purpose=?`, backupID, purpose)
}

func sandboxName(backupID, purpose string) string {
	return "bm-sandbox-" + purpose + "-" + strings.ToLower(backupID)
}

// SweepSandboxes removes sandboxes nobody used for a while, and records whose container is gone.
func (a *App) SweepSandboxes() {
	rows, err := a.Store.DB.Query(`SELECT backup_id,purpose,last_used,container FROM sandboxes`)
	if err != nil {
		return
	}
	type rec struct {
		id, purpose, container string
		last                   int64
	}
	var list []rec
	for rows.Next() {
		var r rec
		rows.Scan(&r.id, &r.purpose, &r.last, &r.container)
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := docker(ctx, "inspect", "-f", "{{.State.Running}}", r.container)
		cancel()
		if err != nil || r.last < time.Now().Add(-sandboxIdle).Unix() {
			a.RemoveSandbox(r.id, r.purpose)
		}
	}
}

func randomPassword() string { b := make([]byte, 18); rand.Read(b); return hex.EncodeToString(b) }

func (sb *Sandbox) open(user string) (*sql.DB, error) {
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net, c.Addr = user, sb.pass, "tcp", fmt.Sprintf("127.0.0.1:%d", sb.Port)
	c.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	c.Timeout = 5 * time.Second
	db, err := sql.Open("mysql", c.FormatDSN())
	if err == nil {
		db.SetMaxOpenConns(2)
	}
	return db, err
}

// startSandbox creates the container and a server in it. For a physical backup the data folder is
// rebuilt from the backup chain first; otherwise it starts empty.
func (a *App) startSandbox(ctx context.Context, b *Backup, purpose string, report func(any)) (*Sandbox, error) {
	m, _, err := a.loadManifest(b)
	if err != nil {
		return nil, err
	}
	image, err := a.sandboxImage(m.ServerVersion)
	if err != nil {
		return nil, err
	}
	physical := b.Method == "physical" || b.Method == "incremental"
	a.RemoveSandbox(b.ID, purpose)
	name := sandboxName(b.ID, purpose)
	mem := a.sandboxMemory()
	report(Progress{Phase: "starting the sandbox"})
	if _, err := docker(ctx, "run", "-d", "--name", name, "--label", "bm-sandbox=1", "--memory", mem, "--memory-swap", mem, "--cpus", "1",
		"-p", "127.0.0.1::3306", "--entrypoint", "sh", image, "-c", "sleep infinity"); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			a.RemoveSandbox(b.ID, purpose)
		}
	}()
	run := &Runner{container: name}
	// One level below the volume root: the temporary folders of an incremental restore are created
	// next to the data folder and must be on the same disk volume (files are moved, not copied).
	const datadir = "/var/lib/mysql/data"
	if _, err := run.Output(ctx, "", "sh", "-c", "mkdir -p /var/lib/mysql && chown mysql:mysql /var/lib/mysql"); err != nil {
		return nil, err
	}
	if physical {
		links, err := a.chain(b)
		if err != nil {
			return nil, err
		}
		if err := a.restoreChain(ctx, run, b, links, datadir, report); err != nil {
			return nil, err
		}
	} else if _, err := run.Output(ctx, "", "mariadb-install-db", "--user=mysql", "--datadir="+datadir, "--skip-test-db"); err != nil {
		return nil, err
	}
	sb := &Sandbox{BackupID: b.ID, Purpose: purpose, Container: name, Full: physical, Loaded: map[string]bool{}, pass: randomPassword()}
	// Two accounts made at start: one to load data, one that can only read (used for every search).
	init := fmt.Sprintf("CREATE USER IF NOT EXISTS 'bm_admin'@'%%' IDENTIFIED BY '%[1]s';\nGRANT ALL ON *.* TO 'bm_admin'@'%%';\nCREATE USER IF NOT EXISTS 'bm_read'@'%%' IDENTIFIED BY '%[1]s';\nGRANT SELECT ON *.* TO 'bm_read'@'%%';\nFLUSH PRIVILEGES;\n", sb.pass)
	c := run.Cmd(ctx, "", true, "sh", "-c", `umask 077; cat > /tmp/bm-init.sql && chown mysql /tmp/bm-init.sql && chown -R mysql:mysql "$1"`, "sh", datadir)
	c.Stdin = strings.NewReader(init)
	if out, err := c.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("preparing the sandbox: %w: %s", err, lastLines(string(out), 3))
	}
	if _, err := docker(ctx, "exec", "-d", name, "mariadbd", "--user=mysql", "--datadir="+datadir, "--init-file=/tmp/bm-init.sql", "--innodb-buffer-pool-size=64M",
		"--skip-log-bin", "--skip-slave-start", "--performance-schema=OFF", "--skip-name-resolve", "--bind-address=0.0.0.0", "--max-connections=20", "--log-error=/tmp/bm-mariadbd.log"); err != nil {
		return nil, err
	}
	port, err := docker(ctx, "port", name, "3306/tcp")
	if err != nil {
		return nil, err
	}
	if sb.Port, err = strconv.Atoi(port[strings.LastIndex(port, ":")+1:]); err != nil {
		return nil, fmt.Errorf("unexpected sandbox port %q", port)
	}
	report(Progress{Phase: "waiting for the sandbox server"})
	db, err := sb.open("bm_admin")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err = db.PingContext(ctx); err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			logTail, _ := run.Output(context.Background(), "", "tail", "-n", "5", "/tmp/bm-mariadbd.log")
			return nil, fmt.Errorf("the sandbox server did not start: %v: %s", err, lastLines(logTail, 5))
		}
		time.Sleep(time.Second)
	}
	if err := a.saveSandbox(sb); err != nil {
		return nil, err
	}
	ok = true
	return sb, nil
}

// loadSQL pipes an export of the backup into the sandbox through the mariadb client (which also
// understands DELIMITER, so views, routines and triggers load too).
func (a *App) loadSQL(ctx context.Context, sb *Sandbox, b *Backup, opt ExportOptions, database string, onBytes func(int)) error {
	args := []string{"mariadb", "--user=bm_admin", "--default-character-set=utf8mb4"}
	if database != "" {
		db, err := sb.open("bm_admin")
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+ident(database))
		db.Close()
		if err != nil {
			return err
		}
		args = append(args, "--database="+database)
	}
	cmd := (&Runner{container: sb.Container}).Cmd(ctx, sb.pass, true, args...)
	pr, pw := io.Pipe()
	logTail := &tailBuffer{}
	cmd.Stdin, cmd.Stderr = pr, logTail
	if err := cmd.Start(); err != nil {
		return err
	}
	eerr := a.ExportSQL(ctx, b, opt, &countWriter{w: pw, f: onBytes})
	pw.CloseWithError(eerr)
	werr := cmd.Wait()
	pr.Close()
	if eerr != nil {
		return eerr
	}
	if werr != nil {
		return fmt.Errorf("loading into the sandbox failed: %v: %s", werr, lastLines(logTail.String(), 3))
	}
	return nil
}

type ExploreRequest struct {
	BackupID string `json:"backupId"`
	DB       string `json:"db"`
	Table    string `json:"table"`
}

// ExploreReady tells whether a table of a backup can be searched right now.
func (a *App) ExploreReady(b *Backup, db, table string) bool {
	sb := a.Sandbox(b.ID, "explore")
	return sb != nil && (sb.Full || sb.Loaded[db+"."+table])
}

// RunExplore is the queue handler for kind "explore": it makes one table of a backup searchable.
func (a *App) RunExplore(ctx context.Context, j *Job, report func(any)) error {
	var req ExploreRequest
	json.Unmarshal(j.Payload, &req)
	fail := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	b, err := a.Store.Backup(req.BackupID)
	if err != nil || b.Status != "done" || b.DeletedAt > 0 {
		return fail(errors.New("this backup is not available"))
	}
	sb := a.Sandbox(b.ID, "explore")
	if sb != nil {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		db, _ := sb.open("bm_read")
		perr := db.PingContext(c)
		db.Close()
		cancel()
		if perr != nil {
			sb = nil
		}
	}
	if sb == nil {
		if sb, err = a.startSandbox(ctx, b, "explore", report); err != nil {
			return fail(err)
		}
	}
	if !sb.Full && req.Table != "" && !sb.Loaded[req.DB+"."+req.Table] {
		prog := Progress{Phase: "loading the table", Table: req.DB + "." + req.Table, TablesTotal: 1}
		if err := a.loadSQL(ctx, sb, b, ExportOptions{DB: req.DB, Table: req.Table, NoDB: true}, req.DB, func(n int) { prog.RawBytes += int64(n); report(prog) }); err != nil {
			return fail(err)
		}
		sb.Loaded[req.DB+"."+req.Table] = true
		if err := a.saveSandbox(sb); err != nil {
			return fail(err)
		}
	}
	a.touchSandbox(sb)
	a.Hub.Publish("backup", b.ID)
	return nil
}

// ---- searching rows ----

type RowFilter struct {
	Column string `json:"column"`
	Op     string `json:"op"` // eq ne lt gt le ge contains starts null notnull
	Value  string `json:"value"`
}

type RowQuery struct {
	DB, Table string
	Page      int
	PageSize  int
	Sort, Dir string
	Search    string // text looked for in every column
	Filters   []RowFilter
}

type RowColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Key  bool   `json:"key"`
}

type RowPage struct {
	Columns []RowColumn `json:"columns"`
	Rows    [][]any     `json:"rows"`
	Total   int64       `json:"total"`
	Page    int         `json:"page"`
}

var rowOps = map[string]string{"eq": "= ?", "ne": "<> ?", "lt": "< ?", "gt": "> ?", "le": "<= ?", "ge": ">= ?", "contains": "LIKE ?", "starts": "LIKE ?", "null": "IS NULL", "notnull": "IS NOT NULL"}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// buildRowSQL turns a query into SQL. Column names come only from the table's own column list and
// every value is a bound parameter, so nothing typed in the panel can change the statement.
func buildRowSQL(q RowQuery, cols []RowColumn) (where string, args []any, order string, err error) {
	byName := map[string]RowColumn{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	var parts []string
	for _, f := range q.Filters {
		c, ok := byName[f.Column]
		if !ok {
			return "", nil, "", fmt.Errorf("unknown column %q", f.Column)
		}
		op, ok := rowOps[f.Op]
		if !ok {
			return "", nil, "", fmt.Errorf("unknown comparison %q", f.Op)
		}
		parts = append(parts, ident(c.Name)+" "+op)
		switch f.Op {
		case "null", "notnull":
		case "contains":
			args = append(args, "%"+likeEscape(f.Value)+"%")
		case "starts":
			args = append(args, likeEscape(f.Value)+"%")
		default:
			args = append(args, f.Value)
		}
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		var any []string
		for _, c := range cols {
			if !searchable(c.Type) {
				continue
			}
			any = append(any, "CAST("+ident(c.Name)+" AS CHAR) LIKE ?")
			args = append(args, "%"+likeEscape(s)+"%")
		}
		if len(any) > 0 {
			parts = append(parts, "("+strings.Join(any, " OR ")+")")
		}
	}
	if len(parts) > 0 {
		where = " WHERE " + strings.Join(parts, " AND ")
	}
	dir := "ASC"
	if strings.EqualFold(q.Dir, "desc") {
		dir = "DESC"
	}
	if q.Sort != "" {
		if _, ok := byName[q.Sort]; !ok {
			return "", nil, "", fmt.Errorf("unknown column %q", q.Sort)
		}
		order = " ORDER BY " + ident(q.Sort) + " " + dir
	} else {
		var keys []string
		for _, c := range cols {
			if c.Key {
				keys = append(keys, ident(c.Name))
			}
		}
		if len(keys) > 0 {
			order = " ORDER BY " + strings.Join(keys, ",")
		}
	}
	return where, args, order, nil
}

// searchable tells whether a column can be matched as text (binary and geometry columns cannot).
func searchable(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "geometry":
		return false
	}
	return columnKind(dataType) != kindBinary
}

const maxCell = 2000

func cellValue(kind int, v sql.RawBytes) any {
	switch {
	case v == nil:
		return nil
	case kind == kindBinary:
		if len(v) > 64 {
			return fmt.Sprintf("0x%s… (%d bytes)", hex.EncodeToString(v[:64]), len(v))
		}
		return "0x" + hex.EncodeToString(v)
	case len(v) > maxCell:
		return strings.ToValidUTF8(string(v[:maxCell]), "?") + fmt.Sprintf("… (%d characters)", len(v))
	}
	return strings.ToValidUTF8(string(v), "?")
}

func (a *App) sandboxFor(b *Backup, db, table string) (*Sandbox, error) {
	sb := a.Sandbox(b.ID, "explore")
	if sb == nil || !(sb.Full || sb.Loaded[db+"."+table]) {
		return nil, errors.New("this table is not loaded in the sandbox yet")
	}
	a.touchSandbox(sb)
	return sb, nil
}

// SandboxTables lists what can be searched in a backup's sandbox right now.
func (a *App) SandboxTables(ctx context.Context, b *Backup) ([]TableInfo, error) {
	sb := a.Sandbox(b.ID, "explore")
	if sb == nil {
		return []TableInfo{}, nil
	}
	db, err := sb.open("bm_read")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	all, err := ListTables(ctx, db)
	if err != nil {
		return nil, err
	}
	out := []TableInfo{}
	for _, t := range all {
		if t.Type == "BASE TABLE" && (sb.Full || sb.Loaded[t.DB+"."+t.Name]) {
			out = append(out, t)
		}
	}
	return out, nil
}

// QueryRows runs one page of a search. onRow, when set, receives every matching row instead (CSV export).
func (a *App) QueryRows(ctx context.Context, b *Backup, q RowQuery, onRow func(cols []RowColumn, row []any) error) (*RowPage, error) {
	sb, err := a.sandboxFor(b, q.DB, q.Table)
	if err != nil {
		return nil, err
	}
	db, err := sb.open("bm_read") // read-only account
	if err != nil {
		return nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("the sandbox is not answering: %w", err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "SET SESSION max_statement_time=30")
	crow, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, COLUMN_KEY='PRI' FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, q.DB, q.Table)
	if err != nil {
		return nil, err
	}
	var cols []RowColumn
	for crow.Next() {
		var c RowColumn
		crow.Scan(&c.Name, &c.Type, &c.Key)
		cols = append(cols, c)
	}
	crow.Close()
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s.%s is not in the sandbox", q.DB, q.Table)
	}
	where, args, order, err := buildRowSQL(q, cols)
	if err != nil {
		return nil, err
	}
	from := " FROM " + ident(q.DB) + "." + ident(q.Table)
	page := &RowPage{Columns: cols, Rows: [][]any{}, Page: q.Page}
	stmt := "SELECT *" + from + where + order
	if onRow == nil {
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*)"+from+where, args...).Scan(&page.Total); err != nil {
			return nil, explainSlow(err)
		}
		if q.PageSize <= 0 || q.PageSize > 200 {
			q.PageSize = 25
		}
		if q.Page < 1 {
			q.Page, page.Page = 1, 1
		}
		stmt += fmt.Sprintf(" LIMIT %d OFFSET %d", q.PageSize, (q.Page-1)*q.PageSize)
	} else {
		stmt += " LIMIT 200000"
	}
	rows, err := conn.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, explainSlow(err)
	}
	defer rows.Close()
	types, _ := rows.ColumnTypes()
	vals := make([]sql.RawBytes, len(types))
	ptrs := make([]any, len(types))
	kinds := make([]int, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
		kinds[i] = columnKind(types[i].DatabaseTypeName())
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = cellValue(kinds[i], v)
		}
		if onRow != nil {
			if err := onRow(cols, row); err != nil {
				return nil, err
			}
			continue
		}
		page.Rows = append(page.Rows, row)
	}
	return page, explainSlow(rows.Err())
}

func explainSlow(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1969 {
		return errors.New("this search took longer than 30 seconds and was stopped; narrow it with a filter on an indexed column")
	}
	return err
}
