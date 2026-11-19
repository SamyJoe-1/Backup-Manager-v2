package core

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Other things that can be backed up besides MySQL/MariaDB: folders, Redis, PostgreSQL.

type FilesConfig struct {
	Paths   []string `json:"paths"`
	Exclude []string `json:"exclude,omitempty"` // names or globs, e.g. node_modules, *.log, /srv/app/cache
}

type RedisConfig struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
}

// MethodForKind is the backup method a target kind uses ("" = chosen in the plan, MySQL/MariaDB).
func MethodForKind(kind string) string {
	switch kind {
	case "files", "redis", "postgres":
		return kind
	}
	return ""
}

func (a *App) newDataKey(spec Spec) (key, wrapped []byte, err error) {
	if spec.NoEncrypt {
		return nil, nil, nil
	}
	if a.Remote != nil { // an agent uses the key the panel made for this backup
		return a.Remote.DataKey, a.Remote.Wrapped, nil
	}
	return a.Keys.NewDataKey()
}

// writeManifest stores the manifest and the small meta file: the last step of every backup.
func writeManifest(ctx context.Context, st Storage, m *Manifest, dataKey, wrapped []byte, level int) error {
	m.FinishedAt = now()
	suffix := objectSuffix(dataKey != nil)
	mj, _ := json.MarshalIndent(m, "", " ")
	mkey := m.ID + "/manifest.json" + suffix
	w, err := newObjectWriter(ctx, st, mkey, dataKey, level, nil, nil, nil)
	if err != nil {
		return err
	}
	_, werr := w.Write(mj)
	if _, _, _, err := w.finish(werr); err != nil {
		return err
	}
	meta, _ := json.Marshal(Meta{ID: m.ID, Encrypted: dataKey != nil, WrappedKey: hex.EncodeToString(wrapped), Manifest: mkey})
	_, err = st.Put(ctx, m.ID+"/meta.json", bytes.NewReader(meta))
	return err
}

// ---- folders ----

func excluded(patterns []string, full, base string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			if full == path.Clean(p) || strings.HasPrefix(full, path.Clean(p)+"/") {
				return true
			}
			if ok, _ := path.Match(p, full); ok {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

type fileEntry struct {
	Path  string `json:"p"`
	Size  int64  `json:"s"`
	Mode  string `json:"m"`
	MTime int64  `json:"t"`
}

// doFiles packs folders into one tar stream. Files are read while the system runs: a file that
// changes size while being read is stored at the size first seen and listed as a warning.
func (a *App) doFiles(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	spec := p.Spec
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	if err := st.DeletePrefix(ctx, b.ID); err != nil {
		return nil, nil, fmt.Errorf("destination: %w", err)
	}
	t, err := a.Store.Endpoint("targets", b.TargetID)
	if err != nil {
		return nil, nil, fmt.Errorf("folder target not found: %w", ErrNoRetry)
	}
	var cfg FilesConfig
	json.Unmarshal(t.Cfg, &cfg)
	if len(cfg.Paths) == 0 {
		return nil, nil, fmt.Errorf("this folder target lists no paths: %w", ErrNoRetry)
	}
	dataKey, wrapped, err := a.newDataKey(spec)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	m := &Manifest{Format: 1, ID: b.ID, Tool: "backup-manager " + Version, Plan: p.Name, Target: t.Name, ServerVersion: "files on " + host,
		StartedAt: now(), Position: map[string]string{}, Spec: spec, Compression: "zstd", Encrypted: dataKey != nil, Method: "files"}
	suffix := objectSuffix(dataKey != nil)
	prog := Progress{Phase: "packing files"}
	started := time.Now()
	limit := &rateLimiter{perSec: int64(spec.RateMB) << 20}
	tw0, err := newObjectWriter(ctx, st, b.ID+"/files.tar"+suffix, dataKey, spec.Level, limit, func(n int) {
		prog.RawBytes += int64(n)
		if d := time.Since(started).Seconds(); d > 0 {
			prog.Speed = float64(prog.RawBytes) / d
		}
		report(prog)
	}, func(n int) { prog.StoredBytes += int64(n) })
	if err != nil {
		return nil, nil, err
	}
	iw0, err := newObjectWriter(ctx, st, b.ID+"/files.index.jsonl"+suffix, dataKey, spec.Level, nil, nil, nil)
	if err != nil {
		tw0.finish(err)
		return nil, nil, err
	}
	tw := tar.NewWriter(tw0)
	iw := bufio.NewWriter(iw0)
	var files, total int64
	warn := func(msg string) {
		if len(m.Warnings) < 50 {
			m.Warnings = append(m.Warnings, msg)
		}
	}
	add := func(full string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			warn(full + ": " + err.Error())
			return nil
		}
		mode := info.Mode()
		link := ""
		switch {
		case mode&os.ModeSymlink != 0:
			if link, err = os.Readlink(full); err != nil {
				warn(full + ": " + err.Error())
				return nil
			}
		case mode.IsDir(), mode.IsRegular():
		default:
			return nil // sockets, devices, pipes are not data
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = strings.TrimPrefix(full, "/")
		if mode.IsDir() {
			h.Name += "/"
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			h.Uid, h.Gid = int(sys.Uid), int(sys.Gid)
		}
		h.Uname, h.Gname = "", ""
		var f *os.File
		if mode.IsRegular() {
			if f, err = os.Open(full); err != nil {
				warn(full + ": " + err.Error())
				return nil
			}
			defer f.Close()
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if f != nil {
			n, err := io.CopyN(tw, &ctxReader{ctx, f}, h.Size)
			if err == io.EOF { // the file shrank while we read it: keep the archive valid
				if _, perr := io.CopyN(tw, zeroReader{}, h.Size-n); perr != nil {
					return perr
				}
				warn(full + ": changed while it was read")
			} else if err != nil {
				return err
			} else if now, serr := f.Stat(); serr == nil && (now.Size() != h.Size || !now.ModTime().Equal(info.ModTime())) {
				warn(full + ": changed while it was read")
			}
			files++
			total += h.Size
		}
		e, _ := json.Marshal(fileEntry{Path: full, Size: h.Size, Mode: mode.String(), MTime: info.ModTime().Unix()})
		iw.Write(e)
		iw.WriteByte('\n')
		prog.Rows, prog.Table = files, full
		return nil
	}
	walkErr := func() error {
		for _, root := range cfg.Paths {
			root = filepath.Clean(root)
			if !filepath.IsAbs(root) {
				return fmt.Errorf("path %q must be absolute: %w", root, ErrNoRetry)
			}
			if _, err := os.Lstat(root); err != nil {
				return fmt.Errorf("%s: %w: %w", root, err, ErrNoRetry)
			}
			err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				if err != nil {
					warn(full + ": " + err.Error())
					return nil
				}
				if full != root && excluded(cfg.Exclude, full, d.Name()) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				return add(full, d)
			})
			if err != nil {
				return err
			}
		}
		return tw.Close()
	}()
	raw, stored, sum, err := tw0.finish(walkErr)
	ferr := iw.Flush()
	iraw, istored, isum, ierr := iw0.finish(errors.Join(err, ferr))
	if err != nil {
		return nil, nil, err
	}
	if ierr != nil {
		return nil, nil, ierr
	}
	m.Objects = []Object{
		{Kind: "files", Key: b.ID + "/files.tar" + suffix, Rows: files, RawBytes: raw, StoredBytes: stored, SHA256: sum},
		{Kind: "index", Key: b.ID + "/files.index.jsonl" + suffix, RawBytes: iraw, StoredBytes: istored, SHA256: isum},
	}
	m.Position["files"], m.Position["bytes"] = strconv.FormatInt(files, 10), strconv.FormatInt(total, 10)
	return m, wrapped, writeManifest(ctx, st, m, dataKey, wrapped, spec.Level)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// ListFiles pages through the file list of a folder backup.
func (a *App) ListFiles(ctx context.Context, b *Backup, search string, page, size int) ([]fileEntry, int, error) {
	m, key, err := a.loadManifest(b)
	if err != nil {
		return nil, 0, err
	}
	var idx *Object
	for i := range m.Objects {
		if m.Objects[i].Kind == "index" {
			idx = &m.Objects[i]
		}
	}
	if idx == nil {
		return nil, 0, errors.New("this backup has no file list")
	}
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return nil, 0, err
	}
	pr, pw := io.Pipe()
	go func() { o := *idx; o.SHA256 = ""; pw.CloseWithError(copyObject(ctx, st, o, key, pw)) }()
	defer pr.Close()
	if size <= 0 || size > 500 {
		size = 50
	}
	if page < 1 {
		page = 1
	}
	search = strings.ToLower(search)
	out := []fileEntry{}
	total := 0
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e fileEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || (search != "" && !strings.Contains(strings.ToLower(e.Path), search)) {
			continue
		}
		if total >= (page-1)*size && len(out) < size {
			out = append(out, e)
		}
		total++
	}
	return out, total, sc.Err()
}

// ---- Redis ----

func respCommand(args ...string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.Bytes()
}

type redisConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialRedis(ctx context.Context, cfg RedisConfig, password string) (*redisConn, error) {
	port := cfg.Port
	if port == 0 {
		port = 6379
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	rc := &redisConn{c: c, r: bufio.NewReaderSize(c, 256<<10)}
	if password != "" {
		args := []string{"AUTH", password}
		if cfg.User != "" {
			args = []string{"AUTH", cfg.User, password}
		}
		if _, err := rc.do(args...); err != nil {
			c.Close()
			return nil, fmt.Errorf("Redis sign-in failed: %w", err)
		}
	}
	return rc, nil
}

// do sends a command and reads a simple or bulk reply.
func (rc *redisConn) do(args ...string) (string, error) {
	rc.c.SetDeadline(time.Now().Add(30 * time.Second))
	defer rc.c.SetDeadline(time.Time{})
	if _, err := rc.c.Write(respCommand(args...)); err != nil {
		return "", err
	}
	line, err := rc.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "-"):
		return "", errors.New(line[1:])
	case strings.HasPrefix(line, "$"):
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return "", nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(rc.r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	return strings.TrimLeft(line, "+:"), nil
}

func redisInfo(rc *redisConn, section string) (map[string]string, error) {
	text, err := rc.do("INFO", section)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ":"); ok {
			out[k] = v
		}
	}
	return out, nil
}

// readRDB reads the snapshot the server sends after SYNC, in either transfer format.
func readRDB(r *bufio.Reader, w io.Writer) (int64, error) {
	var line string
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("waiting for the Redis snapshot: %w", err)
		}
		if l = strings.TrimRight(l, "\r\n"); l != "" { // empty lines are keep-alives while Redis prepares
			line = l
			break
		}
	}
	if strings.HasPrefix(line, "-") {
		return 0, errors.New("Redis refused the snapshot: " + line[1:])
	}
	if !strings.HasPrefix(line, "$") {
		return 0, fmt.Errorf("unexpected Redis reply %q", line)
	}
	if rest, ok := strings.CutPrefix(line, "$EOF:"); ok { // stream ended by a 40-byte marker
		mark := []byte(rest)
		if len(mark) != 40 {
			return 0, errors.New("unexpected Redis stream marker")
		}
		var n int64
		hold := make([]byte, 0, 1<<20)
		buf := make([]byte, 256<<10)
		for {
			k, err := r.Read(buf)
			hold = append(hold, buf[:k]...)
			if bytes.HasSuffix(hold, mark) {
				body := hold[:len(hold)-40]
				if _, werr := w.Write(body); werr != nil {
					return n, werr
				}
				return n + int64(len(body)), nil
			}
			if len(hold) > 40 {
				cut := len(hold) - 40
				if _, werr := w.Write(hold[:cut]); werr != nil {
					return n, werr
				}
				n += int64(cut)
				hold = append(hold[:0], hold[cut:]...)
			}
			if err != nil {
				return n, fmt.Errorf("the Redis snapshot stream was cut: %w", err)
			}
		}
	}
	size, err := strconv.ParseInt(line[1:], 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("unexpected Redis snapshot size %q", line)
	}
	n, err := io.CopyN(w, r, size)
	if err != nil {
		return n, fmt.Errorf("the Redis snapshot stream was cut: %w", err)
	}
	return n, nil
}

// doRedis asks Redis for a snapshot the way a replica does. Redis makes it in a background child
// process: it never blocks commands and never touches a key. The only cost is memory for pages
// written during the snapshot, so the backup is refused when the machine has little memory left.
func (a *App) doRedis(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	spec := p.Spec
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	if err := st.DeletePrefix(ctx, b.ID); err != nil {
		return nil, nil, fmt.Errorf("destination: %w", err)
	}
	t, pw, err := a.targetDB(b.TargetID)
	if err != nil {
		return nil, nil, err
	}
	var cfg RedisConfig
	json.Unmarshal(t.Cfg, &cfg)
	rc, err := dialRedis(ctx, cfg, pw)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach Redis %q: %w", t.Name, err)
	}
	defer rc.c.Close()
	stop := context.AfterFunc(ctx, func() { rc.c.Close() })
	defer stop()
	srv, err := redisInfo(rc, "server")
	if err != nil {
		return nil, nil, err
	}
	mem, _ := redisInfo(rc, "memory")
	used, _ := strconv.ParseInt(mem["used_memory"], 10, 64)
	sys, _ := strconv.ParseInt(mem["total_system_memory"], 10, 64)
	if sys > 0 && used > sys*45/100 && !spec.Force {
		return nil, nil, fmt.Errorf("Redis uses %s of %s on its machine; a snapshot now could run it out of memory. Free memory, or set \"force\" on the plan: %w",
			humanBytes(used), humanBytes(sys), ErrNoRetry)
	}
	dataKey, wrapped, err := a.newDataKey(spec)
	if err != nil {
		return nil, nil, err
	}
	m := &Manifest{Format: 1, ID: b.ID, Tool: "backup-manager " + Version, Plan: p.Name, Target: t.Name, ServerVersion: "Redis " + srv["redis_version"],
		StartedAt: now(), Position: map[string]string{"used_memory": mem["used_memory"]}, Spec: spec, Compression: "zstd", Encrypted: dataKey != nil, Method: "redis"}
	if rep, err := redisInfo(rc, "replication"); err == nil {
		m.Position["master_repl_offset"], m.Position["role"] = rep["master_repl_offset"], rep["role"]
	}
	if ks, err := redisInfo(rc, "keyspace"); err == nil {
		for _, k := range sortedKeys(ks) {
			m.Position[k] = ks[k]
		}
	}
	prog := Progress{Phase: "waiting for the Redis snapshot", EstBytes: used}
	report(prog)
	key := b.ID + "/dump.rdb" + objectSuffix(dataKey != nil)
	w, err := newObjectWriter(ctx, st, key, dataKey, spec.Level, &rateLimiter{perSec: int64(spec.RateMB) << 20},
		func(n int) { prog.Phase = "receiving the snapshot"; prog.RawBytes += int64(n); report(prog) }, nil)
	if err != nil {
		return nil, nil, err
	}
	if _, err := rc.c.Write(respCommand("SYNC")); err != nil {
		w.finish(err)
		return nil, nil, err
	}
	_, rerr := readRDB(rc.r, w)
	if rerr != nil && ctx.Err() != nil {
		rerr = ctx.Err()
	}
	raw, stored, sum, err := w.finish(rerr)
	if err != nil {
		return nil, nil, err
	}
	m.Objects = []Object{{Kind: "rdb", Key: key, RawBytes: raw, StoredBytes: stored, SHA256: sum}}
	return m, wrapped, writeManifest(ctx, st, m, dataKey, wrapped, spec.Level)
}

// ---- PostgreSQL (pg_dump, one consistent dump per database) ----

func pgArgs(r *Runner, c TargetConfig) []string {
	a := []string{"-U", c.User}
	if r.container != "" {
		return a
	}
	port := c.Port
	if port == 0 {
		port = 5432
	}
	return append(a, "-h", c.Host, "-p", strconv.Itoa(port))
}

func pgQuery(ctx context.Context, r *Runner, c TargetConfig, pw, db, q string) ([]string, error) {
	args := append([]string{"psql", "-X", "-At", "-v", "ON_ERROR_STOP=1"}, pgArgs(r, c)...)
	out, err := r.OutputEnv(ctx, map[string]string{"PGPASSWORD": pw}, append(args, "-d", db, "-c", q)...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func pgDatabases(ctx context.Context, r *Runner, c TargetConfig, pw string) ([]string, error) {
	return pgQuery(ctx, r, c, pw, "postgres", "SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 1")
}

func (a *App) doPostgres(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	spec := p.Spec
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	if err := st.DeletePrefix(ctx, b.ID); err != nil {
		return nil, nil, fmt.Errorf("destination: %w", err)
	}
	t, pw, err := a.targetDB(b.TargetID)
	if err != nil {
		return nil, nil, err
	}
	var cfg TargetConfig
	json.Unmarshal(t.Cfg, &cfg)
	run, err := NewRunner(cfg.Exec)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	env := map[string]string{"PGPASSWORD": pw}
	ver, err := pgQuery(ctx, run, cfg, pw, "postgres", "SHOW server_version")
	if err != nil || len(ver) == 0 {
		return nil, nil, fmt.Errorf("cannot reach PostgreSQL %q: %v", t.Name, err)
	}
	all, err := pgDatabases(ctx, run, cfg, pw)
	if err != nil {
		return nil, nil, err
	}
	var dbs []string
	for _, d := range all {
		if spec.wantsDB(d) {
			dbs = append(dbs, d)
		}
	}
	if len(dbs) == 0 {
		return nil, nil, fmt.Errorf("nothing matches this plan on %q: %w", t.Name, ErrNoRetry)
	}
	dataKey, wrapped, err := a.newDataKey(spec)
	if err != nil {
		return nil, nil, err
	}
	m := &Manifest{Format: 1, ID: b.ID, Tool: "backup-manager " + Version, Plan: p.Name, Target: t.Name, ServerVersion: "PostgreSQL " + ver[0],
		StartedAt: now(), Position: map[string]string{}, Spec: spec, Compression: "zstd", Encrypted: dataKey != nil, Method: "postgres"}
	if lsn, err := pgQuery(ctx, run, cfg, pw, "postgres", "SELECT pg_current_wal_lsn()"); err == nil && len(lsn) > 0 {
		m.Position["wal_lsn"] = lsn[0]
	}
	suffix := objectSuffix(dataKey != nil)
	prog := Progress{Phase: "dumping", TablesTotal: len(dbs)}
	started := time.Now()
	limit := &rateLimiter{perSec: int64(spec.RateMB) << 20}
	dump := func(o Object, args []string) error {
		w, err := newObjectWriter(ctx, st, o.Key, dataKey, spec.Level, limit, func(n int) {
			prog.RawBytes += int64(n)
			if d := time.Since(started).Seconds(); d > 0 {
				prog.Speed = float64(prog.RawBytes) / d
			}
			report(prog)
		}, func(n int) { prog.StoredBytes += int64(n) })
		if err != nil {
			return err
		}
		cmd := run.CmdEnv(ctx, env, false, args...)
		logTail := &tailBuffer{}
		cmd.Stdout, cmd.Stderr = w, logTail
		rerr := cmd.Run()
		if rerr != nil {
			rerr = fmt.Errorf("%s failed: %v: %s", args[0], rerr, lastLines(logTail.String(), 4))
		}
		raw, stored, sum, err := w.finish(rerr)
		if err != nil {
			return err
		}
		o.RawBytes, o.StoredBytes, o.SHA256 = raw, stored, sum
		m.Objects = append(m.Objects, o)
		return nil
	}
	for i, d := range dbs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		prog.Table, prog.TablesDone = d, i
		report(prog)
		args := append([]string{"pg_dump", "--no-password"}, pgArgs(run, cfg)...)
		switch spec.Content {
		case "schema":
			args = append(args, "--schema-only")
		case "data":
			args = append(args, "--data-only")
		}
		picked := 0
		for _, tb := range spec.Tables {
			if db, name, _ := strings.Cut(tb, "."); db == d || db == "*" {
				args = append(args, "-t", name)
				picked++
			}
		}
		if len(spec.Tables) > 0 && picked == 0 {
			continue
		}
		for _, tb := range spec.Exclude {
			if db, name, _ := strings.Cut(tb, "."); db == d || db == "*" {
				args = append(args, "-T", name)
			}
		}
		if err := dump(Object{DB: d, Kind: "pgdump", Key: b.ID + "/" + objName(d) + ".sql" + suffix}, append(args, d)); err != nil {
			return nil, nil, err
		}
	}
	if len(m.Objects) == 0 {
		return nil, nil, fmt.Errorf("nothing matches this plan on %q: %w", t.Name, ErrNoRetry)
	}
	if len(spec.Tables) == 0 && spec.Content != "data" { // roles and tablespaces live outside any database
		args := append([]string{"pg_dumpall", "--globals-only", "--no-password"}, pgArgs(run, cfg)...)
		if err := dump(Object{Kind: "pgglobals", Key: b.ID + "/_globals.sql" + suffix}, args); err != nil {
			m.Warnings = append(m.Warnings, "roles were not saved (the backup user may not read them): "+err.Error())
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
		}
	}
	return m, wrapped, writeManifest(ctx, st, m, dataKey, wrapped, spec.Level)
}

// ExportObject streams one stored file of a backup, decrypted and decompressed.
func (a *App) ExportObject(ctx context.Context, b *Backup, key string, w io.Writer) error {
	m, dk, err := a.loadManifest(b)
	if err != nil {
		return err
	}
	for _, o := range m.Objects {
		if o.Key == key {
			st, _, err := a.storage(b.DestinationID)
			if err != nil {
				return err
			}
			return copyObject(ctx, st, o, dk, w)
		}
	}
	return errors.New("this file is not part of the backup")
}

// ---- hooks ----

// runHook runs a plan's command on the machine doing the backup. Its output goes to the log, its
// failure text into the job.
func runHook(ctx context.Context, name, command string, env map[string]string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(c, "sh", "-c", command)
	cmd.Env = os.Environ()
	for _, k := range sortedKeys(env) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s hook failed: %v: %s", name, err, lastLines(string(out), 4))
	}
	return nil
}

var _ = sort.Strings
