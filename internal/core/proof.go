package core

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// ---- restore test: prove a backup by really restoring it into a sandbox ----

// RunRestoreTest is the queue handler for kind "restore-test". A backup is marked tested only
// after it was loaded into a fresh server and its contents were checked there.
func (a *App) RunRestoreTest(ctx context.Context, j *Job, report func(any)) error {
	b, err := a.Store.Backup(j.BackupID)
	if err != nil || b.Status != "done" || b.DeletedAt > 0 {
		return fmt.Errorf("this backup is not available: %w", ErrNoRetry)
	}
	started := time.Now()
	detail, terr := a.restoreTest(ctx, b, report)
	a.RemoveSandbox(b.ID, "test")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := ""
	if terr != nil {
		msg = terr.Error()
		a.Notify("failure", fmt.Sprintf("Restore test FAILED for backup %s (%s): %v", b.ID, b.PlanName, terr))
	}
	a.Store.DB.Exec(`UPDATE backups SET tested_at=?, test_error=?, test_seconds=?, test_detail=? WHERE id=?`, now(), msg, int(time.Since(started).Seconds()), detail, b.ID)
	a.Hub.Publish("backup", b.ID)
	if terr != nil {
		return fmt.Errorf("%w: %w", terr, ErrNoRetry)
	}
	return nil
}

func (a *App) restoreTest(ctx context.Context, b *Backup, report func(any)) (string, error) {
	m, _, err := a.loadManifest(b)
	if err != nil {
		return "", err
	}
	sb, err := a.startSandbox(ctx, b, "test", report)
	if err != nil {
		return "", err
	}
	physical := m.Method != ""
	if !physical {
		prog := Progress{Phase: "loading the backup", EstBytes: b.RawBytes}
		if err := a.loadSQL(ctx, sb, b, ExportOptions{}, "", func(n int) { prog.RawBytes += int64(n); report(prog) }); err != nil {
			return "", err
		}
	}
	db, err := sb.open("bm_admin")
	if err != nil {
		return "", err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if physical {
		tables, err := ListTables(ctx, conn)
		if err != nil {
			return "", err
		}
		n := 0
		for _, t := range tables {
			if t.Type != "BASE TABLE" {
				continue
			}
			report(Progress{Phase: "checking tables", Table: t.DB + "." + t.Name, TablesDone: n, TablesTotal: len(tables)})
			rows, err := conn.QueryContext(ctx, "CHECK TABLE "+ident(t.DB)+"."+ident(t.Name))
			if err != nil {
				return "", fmt.Errorf("%s.%s: %w", t.DB, t.Name, err)
			}
			for rows.Next() {
				var tbl, op, typ, text string
				rows.Scan(&tbl, &op, &typ, &text)
				if typ == "error" || (typ == "status" && text != "OK") {
					rows.Close()
					return "", fmt.Errorf("table %s is damaged in the restored copy: %s", tbl, text)
				}
			}
			rows.Close()
			n++
		}
		if n == 0 {
			return "", errors.New("the restored server has no tables")
		}
		return fmt.Sprintf("server started on the restored files; %d tables checked", n), nil
	}
	var tables, rows int64
	for _, o := range m.Objects {
		if o.Table == "" {
			continue
		}
		var n int64
		switch o.Kind {
		case "data":
			report(Progress{Phase: "counting rows", Table: o.DB + "." + o.Table})
			if m.Spec.Content == "data" {
				continue // rows-only backups carry no structure to load into
			}
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+ident(o.DB)+"."+ident(o.Table)).Scan(&n); err != nil {
				return "", fmt.Errorf("%s.%s: %w", o.DB, o.Table, err)
			}
			if n != o.Rows {
				return "", fmt.Errorf("table %s.%s has %d rows after restore, the backup recorded %d", o.DB, o.Table, n, o.Rows)
			}
			rows += n
		case "schema":
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", o.DB, o.Table).Scan(&n); err != nil || n != 1 {
				return "", fmt.Errorf("table %s.%s is missing after restore", o.DB, o.Table)
			}
			tables++
		}
	}
	return fmt.Sprintf("loaded into a fresh server; %d tables present, %d rows counted and matching", tables, rows), nil
}

// ---- restore one table of a logical backup into a live database, under a new name ----

type TableRestoreRequest struct {
	BackupID string `json:"backupId"`
	DB       string `json:"db"`
	Table    string `json:"table"`
	TargetID int64  `json:"targetId"`
	User     string `json:"user"`
	Secret   string `json:"secret,omitempty"` // password sealed with the master key; never stored in clear
	ToDB     string `json:"toDb"`
	ToTable  string `json:"toTable"`
}

var reConstraint = regexp.MustCompile("^(\\s*CONSTRAINT `)((?:[^`]|``)+)(`)")

// rewriteTableSQL adapts the exported statements of one table to a new table name. The export puts
// every statement head at the start of a line and never lets row data start a line with a keyword
// (new lines inside values are escaped), so working line by line is safe.
func rewriteTableSQL(line, table, newTable, suffix string) (out string, skip bool) {
	old := ident(table)
	switch {
	case strings.HasPrefix(line, "DROP TABLE IF EXISTS "), strings.HasPrefix(line, "--"), strings.HasPrefix(line, "/*!"):
		return "", true // never drop anything; session settings are applied separately
	case strings.HasPrefix(line, "CREATE TABLE "+old+" "):
		return "CREATE TABLE " + ident(newTable) + line[len("CREATE TABLE "+old):], false
	case strings.HasPrefix(line, "INSERT INTO "+old+" "):
		return "INSERT INTO " + ident(newTable) + line[len("INSERT INTO "+old):], false
	}
	if m := reConstraint.FindStringSubmatch(line); m != nil {
		// foreign-key names are unique per database, the original table still owns the old names
		name := m[2]
		if len(name)+len(suffix) > 64 {
			name = name[:64-len(suffix)]
		}
		return m[1] + name + suffix + m[3] + line[len(m[0]):], false
	}
	return line, false
}

// RunTableRestore is the queue handler for kind "table-restore".
func (a *App) RunTableRestore(ctx context.Context, j *Job, report func(any)) error {
	var req TableRestoreRequest
	json.Unmarshal(j.Payload, &req)
	fail := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	// the sealed password is used once and then wiped from the job record
	sealed, _ := hex.DecodeString(req.Secret)
	pw, perr := a.Keys.Open(sealed)
	clean := req
	clean.Secret = ""
	cj, _ := json.Marshal(clean)
	a.Store.DB.Exec(`UPDATE jobs SET payload=? WHERE id=?`, string(cj), j.ID)
	if perr != nil {
		return fail(errors.New("could not read the password for this restore"))
	}
	b, err := a.Store.Backup(req.BackupID)
	if err != nil || b.Status != "done" || b.DeletedAt > 0 {
		return fail(errors.New("this backup is not available"))
	}
	m, _, err := a.loadManifest(b)
	if err != nil {
		return fail(err)
	}
	if m.Method != "" {
		return fail(errors.New("single-table restore works from SQL (logical) backups; open a physical backup in the sandbox and export the rows instead"))
	}
	var wantRows int64 = -1
	hasSchema := false
	for _, o := range m.Objects {
		if o.DB == req.DB && o.Table == req.Table {
			if o.Kind == "schema" {
				hasSchema = true
			}
			if o.Kind == "data" {
				wantRows = o.Rows
			}
		}
	}
	if !hasSchema {
		return fail(fmt.Errorf("this backup has no structure for %s.%s (rows-only backup?)", req.DB, req.Table))
	}
	t, err := a.Store.Endpoint("targets", req.TargetID)
	if err != nil {
		return fail(errors.New("database target not found"))
	}
	var cfg TargetConfig
	json.Unmarshal(t.Cfg, &cfg)
	cfg.User = req.User
	cj2, _ := json.Marshal(cfg)
	db, err := OpenTarget(string(cj2), string(pw))
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fail(fmt.Errorf("cannot connect to %q as %s: %w", t.Name, req.User, err))
	}
	defer conn.Close()
	full := ident(req.ToDB) + "." + ident(req.ToTable)
	var exists int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", req.ToDB, req.ToTable).Scan(&exists); err != nil {
		return fail(err)
	}
	if exists > 0 {
		return fail(fmt.Errorf("%s.%s already exists; nothing was changed. Choose another name", req.ToDB, req.ToTable))
	}
	for _, q := range []string{"USE " + ident(req.ToDB), "SET NAMES utf8mb4", "SET SESSION FOREIGN_KEY_CHECKS=0, UNIQUE_CHECKS=0, SQL_MODE='NO_AUTO_VALUE_ON_ZERO'"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fail(fmt.Errorf("%s: %w", q, err))
		}
	}
	pr, pw2 := io.Pipe()
	go func() {
		pw2.CloseWithError(a.ExportSQL(ctx, b, ExportOptions{DB: req.DB, Table: req.Table, NoDB: true}, pw2))
	}()
	defer pr.Close()
	prog := Progress{Phase: "restoring table", Table: req.ToDB + "." + req.ToTable, TablesTotal: 1}
	created := false
	undo := func(err error) error {
		if created { // only ever removes the table this job itself just created
			conn.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+full)
		}
		return fail(err)
	}
	suffix := "_r" + time.Now().UTC().Format("0102150405")
	rd := bufio.NewReaderSize(pr, 1<<20)
	var stmt strings.Builder
	for {
		line, rerr := rd.ReadString('\n')
		if line != "" {
			out, skip := rewriteTableSQL(line, req.Table, req.ToTable, suffix)
			if !skip {
				stmt.WriteString(out)
			}
			if !skip && strings.HasSuffix(strings.TrimRight(out, "\r\n"), ";") {
				q := strings.TrimSpace(stmt.String())
				stmt.Reset()
				isCreate := strings.HasPrefix(q, "CREATE TABLE ")
				if !isCreate && !strings.HasPrefix(q, "INSERT INTO "+ident(req.ToTable)+" ") {
					return undo(fmt.Errorf("unexpected statement in the backup of this table: %.60s", q))
				}
				if _, err := conn.ExecContext(ctx, strings.TrimSuffix(q, ";")); err != nil {
					return undo(err)
				}
				if isCreate {
					created = true
				}
				prog.RawBytes += int64(len(q))
				report(prog)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return undo(rerr)
		}
	}
	if !created {
		return undo(errors.New("the backup held no CREATE TABLE for this table"))
	}
	var got int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+full).Scan(&got); err != nil {
		return undo(err)
	}
	if wantRows >= 0 && got != wantRows {
		return undo(fmt.Errorf("the restored table has %d rows, the backup recorded %d; the new table was removed", got, wantRows))
	}
	a.Store.Audit("system", "table.restored", fmt.Sprintf("%s %s.%s -> %s.%s.%s (%d rows)", b.ID, req.DB, req.Table, t.Name, req.ToDB, req.ToTable, got), "")
	return nil
}
