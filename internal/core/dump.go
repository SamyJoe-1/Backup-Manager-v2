package core

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// TargetConfig describes how to reach one MySQL/MariaDB server.
type TargetConfig struct {
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Socket string `json:"socket,omitempty"`
	User   string `json:"user"`
	TLS    bool   `json:"tls,omitempty"`
	// Exec is where the native tools (mariabackup, mariadb-binlog) run: "local" or "docker:<container>".
	// It must be the machine or container that holds the data files. Empty = logical backups only.
	Exec string `json:"exec,omitempty"`
	// MaxLag is how many seconds a replica may be behind before a physical backup is refused (default 300).
	MaxLag int `json:"maxLag,omitempty"`
}

func (c TargetConfig) dsn(password string) string {
	m := mysql.NewConfig()
	m.User, m.Passwd = c.User, password
	if c.Socket != "" {
		m.Net, m.Addr = "unix", c.Socket
	} else {
		port := c.Port
		if port == 0 {
			port = 3306
		}
		m.Net, m.Addr = "tcp", fmt.Sprintf("%s:%d", c.Host, port)
	}
	if c.TLS {
		m.TLSConfig = "preferred"
	}
	// UTC session so TIMESTAMP values dump and load without shifting.
	m.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	return m.FormatDSN()
}

func OpenTarget(cfgJSON, password string) (*sql.DB, error) {
	var c TargetConfig
	if err := json.Unmarshal([]byte(cfgJSON), &c); err != nil {
		return nil, err
	}
	if c.User == "" || (c.Host == "" && c.Socket == "") {
		return nil, errors.New("database target needs a user and a host or socket")
	}
	db, err := sql.Open("mysql", c.dsn(password))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

// Spec says what one backup contains.
type Spec struct {
	Databases  []string          `json:"databases,omitempty"` // empty = every non-system database
	Tables     []string          `json:"tables,omitempty"`    // "db.table", * allowed; empty = every table
	Exclude    []string          `json:"exclude,omitempty"`   // "db.table", * allowed
	Content    string            `json:"content"`             // all | schema | data
	NoViews    bool              `json:"noViews,omitempty"`
	NoRoutines bool              `json:"noRoutines,omitempty"`
	NoTriggers bool              `json:"noTriggers,omitempty"`
	Where      map[string]string `json:"where,omitempty"` // "db.table" -> SQL condition for the rows
	Level      int               `json:"level,omitempty"` // zstd level 1..4, 0 = default
	NoEncrypt  bool              `json:"noEncrypt,omitempty"`
	RateMB     int               `json:"rateMB,omitempty"` // stored bytes per second limit, 0 = none
	// Method: logical (default) | physical (hot copy of the data files, whole server) | binlog (archive binary logs)
	Method string `json:"method,omitempty"`
	// IncrementalsPerFull: after a full physical backup, take this many incrementals before the next full.
	IncrementalsPerFull int `json:"incrementalsPerFull,omitempty"`
	Parallel            int `json:"parallel,omitempty"` // copy threads for physical backups (1..8)
	// TestEvery: after every Nth good backup of the plan, restore it into a sandbox and check it. 0 = never.
	TestEvery int `json:"testEvery,omitempty"`
	// PreHook / PostHook are shell commands run on the machine doing the backup, before and after it.
	PreHook  string `json:"preHook,omitempty"`
	PostHook string `json:"postHook,omitempty"`
	Force    bool   `json:"force,omitempty"` // Redis: skip the memory guard
}

func (s *Spec) Normalize() error {
	switch s.Method {
	case "":
		s.Method = "logical"
	case "logical", "postgres":
	case "files", "redis":
		s.Content, s.Tables, s.Databases, s.Exclude = "all", nil, nil, nil
		return nil
	case "physical", "binlog":
		if len(s.Tables)+len(s.Databases)+len(s.Exclude) > 0 {
			return fmt.Errorf("a %s backup always covers the whole server; remove the database and table choices", s.Method)
		}
		if s.IncrementalsPerFull < 0 || s.IncrementalsPerFull > 200 {
			return errors.New("incrementals per full must be between 0 and 200")
		}
		s.Content = "all"
		return nil
	default:
		return fmt.Errorf("unknown backup method %q", s.Method)
	}
	switch s.Content {
	case "":
		s.Content = "all"
	case "all", "schema", "data":
	default:
		return fmt.Errorf("content must be all, schema or data (got %q)", s.Content)
	}
	for _, t := range append(append([]string{}, s.Tables...), s.Exclude...) {
		if strings.Count(t, ".") != 1 {
			return fmt.Errorf("table %q must be written as database.table", t)
		}
	}
	return nil
}

func (s *Spec) ScopeText() string {
	switch {
	case s.Method == "physical":
		return "whole server (data files)"
	case s.Method == "files":
		return "folders"
	case s.Method == "redis":
		return "Redis snapshot"
	case len(s.Tables) > 0:
		if len(s.Tables) <= 3 {
			return "tables: " + strings.Join(s.Tables, ", ")
		}
		return fmt.Sprintf("tables: %s +%d more", strings.Join(s.Tables[:3], ", "), len(s.Tables)-3)
	case len(s.Databases) > 0:
		return "databases: " + strings.Join(s.Databases, ", ")
	}
	return "whole server"
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func (s *Spec) wantsDB(db string) bool {
	if len(s.Databases) > 0 {
		return matchAny(s.Databases, db)
	}
	if len(s.Tables) > 0 {
		for _, t := range s.Tables {
			if ok, _ := path.Match(strings.SplitN(t, ".", 2)[0], db); ok {
				return true
			}
		}
		return false
	}
	return true
}

func (s *Spec) wantsTable(db, table string) bool {
	full := db + "." + table
	if matchAny(s.Exclude, full) {
		return false
	}
	if len(s.Tables) > 0 {
		return matchAny(s.Tables, full)
	}
	return s.wantsDB(db)
}

type TableInfo struct {
	DB     string `json:"db"`
	Name   string `json:"name"`
	Type   string `json:"type"` // BASE TABLE | VIEW
	Engine string `json:"engine"`
	Rows   int64  `json:"rows"`  // estimate from the server
	Bytes  int64  `json:"bytes"` // data + index
}

var systemDBs = "('information_schema','performance_schema','mysql','sys')"

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func ListTables(ctx context.Context, q querier) ([]TableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE,''), COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)
		FROM information_schema.TABLES WHERE TABLE_SCHEMA NOT IN `+systemDBs+` AND TABLE_TYPE IN ('BASE TABLE','VIEW') ORDER BY TABLE_SCHEMA, TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableInfo
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.DB, &t.Name, &t.Type, &t.Engine, &t.Rows, &t.Bytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Snapshot is one connection holding one consistent, lock-free view of the server.
type Snapshot struct {
	db       *sql.DB
	conn     *sql.Conn
	Version  string
	Position map[string]string
}

// OpenSnapshot starts a REPEATABLE READ transaction with a consistent snapshot. It takes no table
// or global lock, so writes continue. Metadata lock waits are short: the dump gives up instead of
// making the application queue behind it.
func OpenSnapshot(ctx context.Context, db *sql.DB) (*Snapshot, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{"SET SESSION lock_wait_timeout=10", "SET SESSION wait_timeout=86400", "SET SESSION net_write_timeout=3600"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	conn.ExecContext(ctx, "SET SESSION max_statement_time=0") // MariaDB only; ignored elsewhere
	for _, q := range []string{"SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ", "START TRANSACTION WITH CONSISTENT SNAPSHOT"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	s := &Snapshot{db: db, conn: conn, Position: map[string]string{}}
	conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&s.Version)
	// Where the snapshot sits in the binary log (for point-in-time restore later). Best effort.
	if rows, err := conn.QueryContext(ctx, "SHOW STATUS LIKE 'binlog_snapshot_%'"); err == nil {
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil && v != "" {
				s.Position[strings.ToLower(k)] = v
			}
		}
		rows.Close()
	}
	var gtid sql.NullString
	if conn.QueryRowContext(ctx, "SELECT @@gtid_binlog_pos").Scan(&gtid) == nil && gtid.String != "" {
		s.Position["gtid_binlog_pos"] = gtid.String
	}
	return s, nil
}

func (s *Snapshot) Close() {
	s.conn.ExecContext(context.Background(), "ROLLBACK")
	s.conn.Close()
}

func (s *Snapshot) Tables(ctx context.Context) ([]TableInfo, error) { return ListTables(ctx, s.conn) }

func ident(n string) string { return "`" + strings.ReplaceAll(n, "`", "``") + "`" }

// showCreate runs a SHOW CREATE … statement and returns the column at idx.
func (s *Snapshot) showCreate(ctx context.Context, stmt string, idx int) (string, error) {
	rows, err := s.conn.QueryContext(ctx, stmt)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s returned nothing", stmt)
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	if idx >= len(vals) {
		return "", fmt.Errorf("%s: unexpected result shape", stmt)
	}
	return string(vals[idx]), nil
}

// Pin reads nothing but makes the snapshot hold a shared metadata lock on the table.
func (s *Snapshot) Pin(ctx context.Context, db, table string) error {
	_, err := s.conn.ExecContext(ctx, "SELECT 1 FROM "+ident(db)+"."+ident(table)+" LIMIT 0")
	return err
}

// TableDDL returns the CREATE TABLE text.
func (s *Snapshot) TableDDL(ctx context.Context, db, table string) (string, error) {
	return s.showCreate(ctx, "SHOW CREATE TABLE "+ident(db)+"."+ident(table), 1)
}

func WriteTableSchema(w io.Writer, table, ddl string) error {
	_, err := fmt.Fprintf(w, "DROP TABLE IF EXISTS %s;\n%s;\n", ident(table), ddl)
	return err
}

// use makes db the default database. SHOW CREATE VIEW / TRIGGER / PROCEDURE then print names without
// the database prefix, so the output loads into a database with any name.
func (s *Snapshot) use(ctx context.Context, db string) error {
	_, err := s.conn.ExecContext(ctx, "USE "+ident(db))
	return err
}

// DumpViews writes every wanted view of one database. Returns how many.
func (s *Snapshot) DumpViews(ctx context.Context, w io.Writer, db string, views []string) (int, error) {
	if err := s.use(ctx, db); err != nil {
		return 0, err
	}
	for _, v := range views {
		ddl, err := s.showCreate(ctx, "SHOW CREATE VIEW "+ident(db)+"."+ident(v), 1)
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(w, "DROP VIEW IF EXISTS %s;\n%s;\n", ident(v), ddl); err != nil {
			return 0, err
		}
	}
	return len(views), nil
}

func (s *Snapshot) names(ctx context.Context, q string, args ...any) ([][2]string, error) {
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// DumpRoutines writes the procedures and functions of one database.
func (s *Snapshot) DumpRoutines(ctx context.Context, w io.Writer, db string) (int, error) {
	list, err := s.names(ctx, `SELECT ROUTINE_TYPE, ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=? ORDER BY ROUTINE_TYPE, ROUTINE_NAME`, db)
	if err != nil || len(list) == 0 {
		return 0, err
	}
	if err := s.use(ctx, db); err != nil {
		return 0, err
	}
	fmt.Fprint(w, "DELIMITER ;;\n")
	for _, r := range list {
		if r[0] != "PROCEDURE" && r[0] != "FUNCTION" {
			continue
		}
		ddl, err := s.showCreate(ctx, "SHOW CREATE "+r[0]+" "+ident(db)+"."+ident(r[1]), 2)
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(w, "DROP %s IF EXISTS %s;;\n%s;;\n", r[0], ident(r[1]), ddl); err != nil {
			return 0, err
		}
	}
	_, err = fmt.Fprint(w, "DELIMITER ;\n")
	return len(list), err
}

// DumpTriggers writes the triggers that sit on the wanted tables of one database.
func (s *Snapshot) DumpTriggers(ctx context.Context, w io.Writer, db string, wanted func(table string) bool) (int, error) {
	list, err := s.names(ctx, `SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=? ORDER BY TRIGGER_NAME`, db)
	if err != nil {
		return 0, err
	}
	if err := s.use(ctx, db); err != nil {
		return 0, err
	}
	n := 0
	for _, t := range list {
		if !wanted(t[1]) {
			continue
		}
		ddl, err := s.showCreate(ctx, "SHOW CREATE TRIGGER "+ident(db)+"."+ident(t[0]), 2)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			fmt.Fprint(w, "DELIMITER ;;\n")
		}
		if _, err := fmt.Fprintf(w, "DROP TRIGGER IF EXISTS %s;;\n%s;;\n", ident(t[0]), ddl); err != nil {
			return 0, err
		}
		n++
	}
	if n > 0 {
		_, err = fmt.Fprint(w, "DELIMITER ;\n")
	}
	return n, err
}

// insertColumns lists the columns to dump and the expression that reads each one. Generated columns
// are left out (the server computes them). FLOAT is read as DOUBLE: as text the server prints only
// 6 digits of a FLOAT, which would silently change values.
func (s *Snapshot) insertColumns(ctx context.Context, db, table string) (cols, exprs []string, err error) {
	list, err := s.names(ctx, `SELECT COLUMN_NAME, CONCAT(DATA_TYPE, '|', EXTRA) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, db, table)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range list {
		typ, extra, _ := strings.Cut(strings.ToUpper(c[1]), "|")
		if strings.Contains(extra, "VIRTUAL") || strings.Contains(extra, "STORED GENERATED") || strings.Contains(extra, "PERSISTENT") {
			continue
		}
		cols = append(cols, ident(c[0]))
		if typ == "FLOAT" {
			exprs = append(exprs, "CAST("+ident(c[0])+" AS DOUBLE)")
		} else {
			exprs = append(exprs, ident(c[0]))
		}
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("table %s.%s has no columns to dump", db, table)
	}
	return cols, exprs, nil
}

const (
	kindNumber = iota
	kindBinary
	kindText
)

func columnKind(dbType string) int {
	t := strings.ToUpper(dbType)
	switch {
	case strings.Contains(t, "INT"), strings.Contains(t, "DECIMAL"), t == "FLOAT", t == "DOUBLE", t == "YEAR",
		strings.HasPrefix(t, "UNSIGNED "):
		return kindNumber
	case strings.Contains(t, "BLOB"), strings.Contains(t, "BINARY"), t == "BIT", t == "GEOMETRY":
		return kindBinary
	}
	return kindText
}

var sqlEscapes = [256]string{0: `\0`, '\n': `\n`, '\r': `\r`, 0x1a: `\Z`, '\'': `\'`, '\\': `\\`}

func writeQuoted(w *bufio.Writer, b []byte) {
	w.WriteByte('\'')
	start := 0
	for i, c := range b {
		if e := sqlEscapes[c]; e != "" {
			w.Write(b[start:i])
			w.WriteString(e)
			start = i + 1
		}
	}
	w.Write(b[start:])
	w.WriteByte('\'')
}

func writeValue(w *bufio.Writer, kind int, v sql.RawBytes) {
	switch {
	case v == nil:
		w.WriteString("NULL")
	case kind == kindNumber:
		w.Write(v)
	case kind == kindBinary:
		if len(v) == 0 {
			w.WriteString("''")
			return
		}
		w.WriteString("0x")
		hex.NewEncoder(w).Write(v)
	default:
		writeQuoted(w, v)
	}
}

// DumpRows streams the rows of one table as multi-row INSERT statements. It never loads the table
// into memory. onRows is called now and then with the running row count.
func (s *Snapshot) DumpRows(ctx context.Context, w io.Writer, db, table, where string, onRows func(int64)) (int64, error) {
	quoted, exprs, err := s.insertColumns(ctx, db, table)
	if err != nil {
		return 0, err
	}
	cols := quoted
	q := "SELECT " + strings.Join(exprs, ",") + " FROM " + ident(db) + "." + ident(table)
	if strings.TrimSpace(where) != "" {
		q += " WHERE " + where
	}
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return 0, err
	}
	kinds := make([]int, len(types))
	for i, t := range types {
		kinds[i] = columnKind(t.DatabaseTypeName())
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	bw := bufio.NewWriterSize(w, 256<<10)
	head := "INSERT INTO " + ident(table) + " (" + strings.Join(quoted, ",") + ") VALUES\n"
	const maxStatement = 1 << 20
	var n int64
	size := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		if size == 0 {
			bw.WriteString(head)
		} else {
			bw.WriteString(",\n")
		}
		bw.WriteByte('(')
		for i, v := range vals {
			if i > 0 {
				bw.WriteByte(',')
			}
			writeValue(bw, kinds[i], v)
			size += len(v) + 3
		}
		if _, err := bw.WriteString(")"); err != nil {
			return n, err
		}
		n++
		if size >= maxStatement {
			bw.WriteString(";\n")
			size = 0
		}
		if n%5000 == 0 {
			if onRows != nil {
				onRows(n)
			}
			if err := ctx.Err(); err != nil {
				return n, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return n, explainDumpError(db, table, err)
	}
	if size > 0 {
		bw.WriteString(";\n")
	}
	if onRows != nil {
		onRows(n)
	}
	return n, bw.Flush()
}

func explainDumpError(db, table string, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1412 {
		return fmt.Errorf("table %s.%s was altered while the backup was running; the backup will be retried: %w", db, table, err)
	}
	return fmt.Errorf("%s.%s: %w", db, table, err)
}

// SQLPreamble is put at the top of every downloaded .sql file so it loads cleanly anywhere.
const SQLPreamble = "/*!40101 SET NAMES utf8mb4 */;\n/*!40103 SET TIME_ZONE='+00:00' */;\n/*!40014 SET FOREIGN_KEY_CHECKS=0, UNIQUE_CHECKS=0 */;\n/*!40101 SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;\n"

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
