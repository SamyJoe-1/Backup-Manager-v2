package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// App ties the catalog, the keys and the queue together.
type App struct {
	Store *Store
	Keys  *Keyring
	Queue *Queue
	Hub   *Hub
	// Remote is set while an agent runs one backup on behalf of a panel.
	Remote *RemoteContext
	// CatalogCopy is where the scheduler keeps a consistent copy of the catalog ("" = none).
	CatalogCopy string
}

// RemoteContext carries what only the panel has: the backup's data key and its place in a chain.
type RemoteContext struct {
	DataKey, Wrapped []byte
	Parent           *Backup
}

// Object is one stored file of a backup.
type Object struct {
	DB          string `json:"db"`
	Table       string `json:"table,omitempty"`
	Kind        string `json:"kind"` // schema | data | views | routines | triggers
	Key         string `json:"key"`
	Rows        int64  `json:"rows,omitempty"`
	RawBytes    int64  `json:"rawBytes"`
	StoredBytes int64  `json:"storedBytes"`
	SHA256      string `json:"sha256"` // of the stored bytes
	DDL         string `json:"ddl,omitempty"`
	Engine      string `json:"engine,omitempty"`
}

// Manifest describes a backup completely. A copy is stored next to the data, so the catalog can
// be rebuilt from the destination alone.
type Manifest struct {
	Format        int               `json:"format"`
	ID            string            `json:"id"`
	Tool          string            `json:"tool"`
	Plan          string            `json:"plan"`
	Target        string            `json:"target"`
	ServerVersion string            `json:"serverVersion"`
	StartedAt     int64             `json:"startedAt"`
	FinishedAt    int64             `json:"finishedAt"`
	Position      map[string]string `json:"position"`
	Spec          Spec              `json:"spec"`
	Compression   string            `json:"compression"`
	Encrypted     bool              `json:"encrypted"`
	Method        string            `json:"method,omitempty"`   // "" = logical | physical | incremental
	ParentID      string            `json:"parentId,omitempty"` // incremental: the backup it builds on
	ChainID       string            `json:"chainId,omitempty"`  // the full backup the chain starts with
	LSNFrom       string            `json:"lsnFrom,omitempty"`
	LSNTo         string            `json:"lsnTo,omitempty"`
	Objects       []Object          `json:"objects"`
	Warnings      []string          `json:"warnings,omitempty"`
}

// Meta is the small plain file that lets a fresh install read the manifest.
type Meta struct {
	ID         string `json:"id"`
	Encrypted  bool   `json:"encrypted"`
	WrappedKey string `json:"wrappedKey,omitempty"` // hex, wrapped with the installation master key
	Manifest   string `json:"manifest"`
}

var Version = "dev"

func NewBackupID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

func (a *App) targetDB(id int64) (*Endpoint, string, error) {
	t, err := a.Store.Endpoint("targets", id)
	if err != nil {
		return nil, "", fmt.Errorf("database target %d not found", id)
	}
	pw, err := a.Keys.Open(t.secret)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read the stored password of %q (master key changed?)", t.Name)
	}
	return t, string(pw), nil
}

func (a *App) storage(id int64) (Storage, *Endpoint, error) {
	d, err := a.Store.Endpoint("destinations", id)
	if err != nil {
		return nil, nil, fmt.Errorf("destination %d not found", id)
	}
	sec, err := a.Keys.Open(d.secret)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the stored secret of %q (master key changed?)", d.Name)
	}
	st, err := OpenStorage(d.Kind, string(d.Cfg), string(sec))
	return st, d, err
}

// StartBackup records a backup and queues its job.
func (a *App) StartBackup(p *Plan, trigger string) (*Backup, error) {
	return a.StartBackupIn(p, trigger, "")
}

// StartBackupIn is StartBackup for a run that belongs to a backup set.
func (a *App) StartBackupIn(p *Plan, trigger, setRun string) (*Backup, error) {
	t, err := a.Store.Endpoint("targets", p.TargetID)
	if err != nil {
		return nil, errors.New("the plan's database target no longer exists")
	}
	if k := MethodForKind(t.Kind); k != "" {
		p.Spec.Method = k
	} else if MethodForKind(p.Spec.Method) != "" {
		p.Spec.Method = "logical"
	}
	d, err := a.Store.Endpoint("destinations", p.DestinationID)
	if err != nil {
		return nil, errors.New("the plan's destination no longer exists")
	}
	if err := p.Spec.Normalize(); err != nil {
		return nil, err
	}
	prio := 0
	if trigger == "manual" {
		prio = 5
	}
	if NeedsRunner(p.Spec.Method) {
		var cfg TargetConfig
		json.Unmarshal(t.Cfg, &cfg)
		if _, err := NewRunner(cfg.Exec); err != nil {
			return nil, err
		}
	}
	if p.Spec.Method == "binlog" {
		// no backup record: archived logs are listed per database
		return nil, a.Queue.Enqueue(&Job{Kind: "binlog", PlanID: p.ID, Title: "Binary logs: " + p.Name, Priority: prio})
	}
	id := NewBackupID()
	_, err = a.Store.DB.Exec(`INSERT INTO backups(id,plan_id,target_id,destination_id,plan_name,target_name,destination_name,content,scope,status,trigger,created_at,encrypted,method,set_run)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.ID, t.ID, d.ID, p.Name, t.Name, d.Name, p.Spec.Content, p.Spec.ScopeText(), "queued", trigger, now(), !p.Spec.NoEncrypt, p.Spec.Method, setRun)
	if err != nil {
		return nil, err
	}
	if err := a.Queue.Enqueue(&Job{Kind: "backup", TargetID: t.ID, PlanID: p.ID, BackupID: id, Title: "Backup: " + p.Name, Priority: prio}); err != nil {
		return nil, err
	}
	a.Store.DB.Exec(`UPDATE plans SET last_run_at=? WHERE id=?`, now(), p.ID)
	return a.Store.Backup(id)
}

type Progress struct {
	Phase       string  `json:"phase"`
	Table       string  `json:"table"`
	TablesDone  int     `json:"tablesDone"`
	TablesTotal int     `json:"tablesTotal"`
	Rows        int64   `json:"rows"`
	RawBytes    int64   `json:"rawBytes"`
	StoredBytes int64   `json:"storedBytes"`
	EstBytes    int64   `json:"estBytes"`
	Speed       float64 `json:"speed"` // raw bytes per second
}

// objectWriter pushes one object through compress -> encrypt -> hash -> storage as a stream.
type objectWriter struct {
	raw, stored *countWriter
	closers     []io.Closer
	pw          *io.PipeWriter
	done        chan error
	sum         hash.Hash
	top         io.Writer
}

type countWriter struct {
	w io.Writer
	n int64
	f func(int)
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.f != nil {
		c.f(n)
	}
	return n, err
}

func newObjectWriter(ctx context.Context, st Storage, key string, dataKey []byte, level int, limit *rateLimiter, onRaw, onStored func(int)) (*objectWriter, error) {
	pr, pw := io.Pipe()
	o := &objectWriter{pw: pw, done: make(chan error, 1), sum: sha256.New()}
	go func() {
		_, err := st.Put(ctx, key, pr)
		pr.CloseWithError(err) // unblock the writer if storage failed
		o.done <- err
	}()
	o.stored = &countWriter{w: io.MultiWriter(pw, o.sum), f: func(n int) {
		limit.wait(ctx, n)
		if onStored != nil {
			onStored(n)
		}
	}}
	var w io.Writer = o.stored
	if dataKey != nil {
		enc, err := NewEncWriter(w, dataKey)
		if err != nil {
			pw.CloseWithError(err)
			return nil, err
		}
		o.closers = append(o.closers, enc)
		w = enc
	}
	lv := zstd.SpeedDefault
	if level >= 1 && level <= 4 {
		lv = zstd.EncoderLevel(level)
	}
	z, err := zstd.NewWriter(w, zstd.WithEncoderLevel(lv), zstd.WithEncoderConcurrency(1))
	if err != nil {
		pw.CloseWithError(err)
		return nil, err
	}
	o.closers = append(o.closers, z)
	o.raw = &countWriter{w: z, f: onRaw}
	o.top = o.raw
	return o, nil
}

func (o *objectWriter) Write(p []byte) (int, error) { return o.top.Write(p) }

// finish closes the chain (inner first) and waits for storage. failed aborts the upload.
func (o *objectWriter) finish(failed error) (raw, stored int64, sum string, err error) {
	if failed != nil {
		o.pw.CloseWithError(failed)
		<-o.done
		return 0, 0, "", failed
	}
	for i := len(o.closers) - 1; i >= 0; i-- {
		if cerr := o.closers[i].Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err != nil {
		o.pw.CloseWithError(err)
	} else {
		o.pw.Close()
	}
	if perr := <-o.done; perr != nil && err == nil {
		err = perr
	}
	return o.raw.n, o.stored.n, hex.EncodeToString(o.sum.Sum(nil)), err
}

type rateLimiter struct {
	perSec int64
	start  time.Time
	sent   int64
}

func (r *rateLimiter) wait(ctx context.Context, n int) {
	if r == nil || r.perSec <= 0 {
		return
	}
	if r.start.IsZero() {
		r.start = time.Now()
	}
	r.sent += int64(n)
	due := time.Duration(float64(r.sent) / float64(r.perSec) * float64(time.Second))
	if sleep := due - time.Since(r.start); sleep > 0 {
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
		}
	}
}

func objectSuffix(encrypted bool) string {
	if encrypted {
		return ".zst.enc"
	}
	return ".zst"
}

// RunBackup is the queue handler for kind "backup".
func (a *App) RunBackup(ctx context.Context, j *Job, report func(any)) error {
	b, err := a.Store.Backup(j.BackupID)
	if err != nil {
		return fmt.Errorf("backup record %s is missing: %w", j.BackupID, ErrNoRetry)
	}
	p, err := a.Store.Plan(b.PlanID)
	if err != nil {
		return a.failBackup(b, j, fmt.Errorf("the plan was deleted: %w", ErrNoRetry))
	}
	a.Store.DB.Exec(`UPDATE backups SET status='running', started_at=?, error='' WHERE id=?`, now(), b.ID)
	a.Hub.Publish("backup", b.ID)

	if b.Method != "" { // the method fixed when the backup was queued (it follows the target's kind)
		p.Spec.Method = b.Method
	}
	if err := p.Spec.Normalize(); err != nil {
		return a.failBackup(b, j, fmt.Errorf("%w: %w", err, ErrNoRetry))
	}
	m, wrapped, err := a.execute(ctx, j, b, p, report)
	if err != nil {
		if st, _, serr := a.storage(b.DestinationID); serr == nil { // leave no partial files behind
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			st.DeletePrefix(cctx, b.ID)
			cancel()
		}
		return a.failBackup(b, j, err)
	}
	var raw, stored, rows int64
	tables := 0
	for _, o := range m.Objects {
		raw, stored, rows = raw+o.RawBytes, stored+o.StoredBytes, rows+o.Rows
		if o.Kind == "schema" || (o.Kind == "data" && m.Spec.Content == "data") {
			tables++
		}
	}
	mj, _ := json.Marshal(m)
	method := m.Method
	if method == "" {
		method = "logical"
	}
	_, err = a.Store.DB.Exec(`UPDATE backups SET status='done', finished_at=?, raw_bytes=?, stored_bytes=?, tables=?, row_count=?, manifest=?, wrapped_key=?, error='',
		method=?, parent_id=?, chain_id=?, lsn_to=? WHERE id=?`,
		m.FinishedAt, raw, stored, tables, rows, string(mj), wrapped, method, m.ParentID, m.ChainID, m.LSNTo, b.ID)
	if err != nil {
		return err
	}
	a.Hub.Publish("backup", b.ID)
	a.Notify("success", fmt.Sprintf("Backup done: %s (%s, %d tables, %s stored)", p.Name, b.TargetName, tables, humanBytes(stored)))
	if p.Spec.TestEvery > 0 {
		var done int
		a.Store.DB.QueryRow(`SELECT COUNT(*) FROM backups WHERE plan_id=? AND status='done'`, p.ID).Scan(&done)
		if done%p.Spec.TestEvery == 0 {
			a.Queue.Enqueue(&Job{Kind: "restore-test", BackupID: b.ID, Title: "Restore test: " + b.ID, MaxAttempts: 1})
		}
	}
	if n, err := a.ApplyRetention(ctx, p); err != nil {
		log.Printf("retention for plan %q: %v", p.Name, err)
	} else if n > 0 {
		log.Printf("retention for plan %q: removed %d old backups", p.Name, n)
	}
	return nil
}

// execute runs one backup where the data is: here, or on the agent the target belongs to.
func (a *App) execute(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	if a.Remote == nil {
		if t, err := a.Store.Endpoint("targets", b.TargetID); err == nil {
			if agent := endpointAgent(t); agent > 0 {
				return a.runOnAgent(ctx, agent, j, b, p, t, report)
			}
		}
	}
	return a.ExecuteLocal(ctx, j, b, p, report)
}

// ExecuteLocal runs the hooks and the backup itself on this machine.
func (a *App) ExecuteLocal(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	do := a.doBackup
	switch p.Spec.Method {
	case "physical":
		do = a.doPhysical
	case "files":
		do = a.doFiles
	case "redis":
		do = a.doRedis
	case "postgres":
		do = a.doPostgres
	}
	env := map[string]string{"BM_BACKUP_ID": b.ID, "BM_PLAN": p.Name, "BM_TARGET": b.TargetName, "BM_METHOD": p.Spec.Method, "BM_STATUS": "starting"}
	if err := runHook(ctx, "before", p.Spec.PreHook, env); err != nil {
		return nil, nil, err
	}
	m, wrapped, err := do(ctx, j, b, p, report)
	env["BM_STATUS"] = "done"
	if err != nil {
		env["BM_STATUS"], env["BM_ERROR"] = "failed", err.Error()
	}
	// the "after" hook always runs (it usually undoes what "before" did); it cannot turn a good backup bad
	if herr := runHook(context.WithoutCancel(ctx), "after", p.Spec.PostHook, env); herr != nil {
		log.Printf("backup %s: %v", b.ID, herr)
		if m != nil {
			m.Warnings = append(m.Warnings, herr.Error())
		}
	}
	return m, wrapped, err
}

// NeedsRunner tells whether a method starts native tools next to the data.
func NeedsRunner(method string) bool {
	return method == "physical" || method == "binlog" || method == "postgres"
}

func (a *App) failBackup(b *Backup, j *Job, err error) error {
	final := j.Attempt >= j.MaxAttempts || errors.Is(err, ErrNoRetry)
	var cancelled int
	a.Store.DB.QueryRow(`SELECT cancel FROM jobs WHERE id=?`, j.ID).Scan(&cancelled)
	switch {
	case cancelled == 1:
		a.Store.DB.Exec(`UPDATE backups SET status='cancelled', finished_at=?, error='cancelled' WHERE id=?`, now(), b.ID)
	case final:
		a.Store.DB.Exec(`UPDATE backups SET status='failed', finished_at=?, error=? WHERE id=?`, now(), err.Error(), b.ID)
		a.Notify("failure", fmt.Sprintf("Backup FAILED: %s (%s): %v", b.PlanName, b.TargetName, err))
	default:
		a.Store.DB.Exec(`UPDATE backups SET status='queued', error=? WHERE id=?`, "will retry: "+err.Error(), b.ID)
	}
	a.Hub.Publish("backup", b.ID)
	return err
}

func (a *App) doBackup(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
	spec := p.Spec
	if err := spec.Normalize(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	// A retry starts clean.
	if err := st.DeletePrefix(ctx, b.ID); err != nil {
		return nil, nil, fmt.Errorf("destination: %w", err)
	}
	t, pw, err := a.targetDB(b.TargetID)
	if err != nil {
		return nil, nil, err
	}
	db, err := OpenTarget(string(t.Cfg), pw)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	snap, err := OpenSnapshot(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot start a consistent snapshot on %q: %w", t.Name, err)
	}
	defer snap.Close()

	all, err := snap.Tables(ctx)
	if err != nil {
		return nil, nil, err
	}
	var tables []TableInfo
	views := map[string][]string{}
	dbs := map[string]bool{}
	var est int64
	for _, ti := range all {
		if !spec.wantsTable(ti.DB, ti.Name) {
			continue
		}
		dbs[ti.DB] = true
		if ti.Type == "VIEW" {
			views[ti.DB] = append(views[ti.DB], ti.Name)
			continue
		}
		tables = append(tables, ti)
		est += ti.Bytes
	}
	if len(tables) == 0 && len(views) == 0 {
		return nil, nil, fmt.Errorf("nothing matches this plan on %q (check the database and table names): %w", t.Name, ErrNoRetry)
	}
	// Disk guard: a full disk stops every database on the machine, so refuse before writing.
	if free := st.Free(); free >= 0 && spec.Content != "schema" {
		need := est/2 + 256<<20
		if free < need {
			return nil, nil, fmt.Errorf("destination has %s free but this backup may need about %s", humanBytes(free), humanBytes(need))
		}
	}

	dataKey, wrapped, err := a.newDataKey(spec)
	if err != nil {
		return nil, nil, err
	}
	m := &Manifest{Format: 1, ID: b.ID, Tool: "backup-manager " + Version, Plan: p.Name, Target: t.Name, ServerVersion: snap.Version,
		StartedAt: now(), Position: snap.Position, Spec: spec, Compression: "zstd", Encrypted: dataKey != nil}

	limit := &rateLimiter{perSec: int64(spec.RateMB) << 20}
	prog := Progress{Phase: "dumping", TablesTotal: len(tables), EstBytes: est}
	started := time.Now()
	tick := func() {
		if d := time.Since(started).Seconds(); d > 0 {
			prog.Speed = float64(prog.RawBytes) / d
		}
		report(prog)
	}
	suffix := objectSuffix(dataKey != nil)

	// put writes one object and appends it to the manifest.
	put := func(o Object, fill func(w io.Writer) (int64, error)) error {
		o.Key = b.ID + "/" + o.Key + suffix
		w, err := newObjectWriter(ctx, st, o.Key, dataKey, spec.Level, limit,
			func(n int) { prog.RawBytes += int64(n) }, func(n int) { prog.StoredBytes += int64(n) })
		if err != nil {
			return err
		}
		rows, ferr := fill(w)
		raw, stored, sum, err := w.finish(ferr)
		if err != nil {
			return err
		}
		o.Rows, o.RawBytes, o.StoredBytes, o.SHA256 = rows, raw, stored, sum
		m.Objects = append(m.Objects, o)
		return nil
	}

	baseRows := int64(0)
	for i, ti := range tables {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		prog.Table, prog.TablesDone = ti.DB+"."+ti.Name, i
		tick()
		name := objName(ti.DB) + "/" + objName(ti.Name)
		if spec.Content != "schema" {
			// Touch the table first. From here to the end of the snapshot its definition cannot
			// change, so the structure and the rows saved below always match each other. A table
			// rebuilt since the snapshot started fails here, early, and the backup is retried.
			if err := snap.Pin(ctx, ti.DB, ti.Name); err != nil {
				return nil, nil, explainDumpError(ti.DB, ti.Name, err)
			}
		}
		if spec.Content != "data" {
			ddl, err := snap.TableDDL(ctx, ti.DB, ti.Name)
			if err != nil {
				return nil, nil, explainDumpError(ti.DB, ti.Name, err)
			}
			err = put(Object{DB: ti.DB, Table: ti.Name, Kind: "schema", Key: name + ".schema.sql", DDL: ddl, Engine: ti.Engine},
				func(w io.Writer) (int64, error) { return 0, WriteTableSchema(w, ti.Name, ddl) })
			if err != nil {
				return nil, nil, err
			}
		}
		if spec.Content != "schema" {
			err := put(Object{DB: ti.DB, Table: ti.Name, Kind: "data", Key: name + ".data.sql", Engine: ti.Engine}, func(w io.Writer) (int64, error) {
				return snap.DumpRows(ctx, w, ti.DB, ti.Name, spec.Where[ti.DB+"."+ti.Name], func(n int64) { prog.Rows = baseRows + n; tick() })
			})
			if err != nil {
				return nil, nil, err
			}
			baseRows = prog.Rows
		}
	}
	prog.TablesDone, prog.Phase, prog.Table = len(tables), "finishing", ""
	tick()

	if spec.Content != "data" {
		for _, dbn := range sortedKeys(dbs) {
			if vs := views[dbn]; len(vs) > 0 && !spec.NoViews {
				var buf bytes.Buffer
				if _, err := snap.DumpViews(ctx, &buf, dbn, vs); err != nil {
					return nil, nil, err
				}
				if err := put(Object{DB: dbn, Kind: "views", Key: objName(dbn) + "/_views.sql"}, bufFill(&buf, int64(len(vs)))); err != nil {
					return nil, nil, err
				}
			}
			// Routines belong to a database, so they are saved only with a whole-database scope.
			if !spec.NoRoutines && len(spec.Tables) == 0 {
				var buf bytes.Buffer
				n, err := snap.DumpRoutines(ctx, &buf, dbn)
				if err != nil {
					return nil, nil, err
				}
				if n > 0 {
					if err := put(Object{DB: dbn, Kind: "routines", Key: objName(dbn) + "/_routines.sql"}, bufFill(&buf, int64(n))); err != nil {
						return nil, nil, err
					}
				}
			}
			if !spec.NoTriggers {
				var buf bytes.Buffer
				n, err := snap.DumpTriggers(ctx, &buf, dbn, func(tb string) bool { return spec.wantsTable(dbn, tb) })
				if err != nil {
					return nil, nil, err
				}
				if n > 0 {
					if err := put(Object{DB: dbn, Kind: "triggers", Key: objName(dbn) + "/_triggers.sql"}, bufFill(&buf, int64(n))); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}

	// The manifest and the meta file go last: a backup without them is incomplete by definition.
	m.FinishedAt = now()
	mj, _ := json.MarshalIndent(m, "", " ")
	mkey := b.ID + "/manifest.json" + suffix
	w, err := newObjectWriter(ctx, st, mkey, dataKey, spec.Level, nil, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	_, werr := w.Write(mj)
	if _, _, _, err := w.finish(werr); err != nil {
		return nil, nil, err
	}
	meta, _ := json.Marshal(Meta{ID: b.ID, Encrypted: dataKey != nil, WrappedKey: hex.EncodeToString(wrapped), Manifest: mkey})
	if _, err := st.Put(ctx, b.ID+"/meta.json", bytes.NewReader(meta)); err != nil {
		return nil, nil, err
	}
	return m, wrapped, nil
}

func bufFill(buf *bytes.Buffer, count int64) func(io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		_, err := w.Write(buf.Bytes())
		return count, err
	}
}

// objName makes a database or table name safe as a file name.
func objName(n string) string {
	var sb strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '$':
			sb.WriteRune(r)
		default:
			fmt.Fprintf(&sb, "@%04x", r)
		}
	}
	return sb.String()
}

// ---- reading backups back ----

func (a *App) loadManifest(b *Backup) (*Manifest, []byte, error) {
	mj, err := a.Store.BackupManifest(b.ID)
	if err != nil || mj == "" {
		return nil, nil, errors.New("this backup has no manifest (it did not finish)")
	}
	var m Manifest
	if err := json.Unmarshal([]byte(mj), &m); err != nil {
		return nil, nil, err
	}
	var key []byte
	if m.Encrypted {
		if key, err = a.Keys.Open(b.wrappedKey); err != nil {
			return nil, nil, errors.New("cannot unwrap this backup's key (master key changed?)")
		}
	}
	return &m, key, nil
}

// copyObject writes one object, decrypted and decompressed, to w. It also checks the stored checksum.
func copyObject(ctx context.Context, st Storage, o Object, key []byte, w io.Writer) error {
	rc, _, err := st.Open(ctx, o.Key)
	if err != nil {
		return fmt.Errorf("%s: %w", o.Key, err)
	}
	defer rc.Close()
	sum := sha256.New()
	var r io.Reader = io.TeeReader(&ctxReader{ctx, rc}, sum)
	if key != nil {
		if r, err = NewDecReader(r, key); err != nil {
			return fmt.Errorf("%s: %w", o.Key, err)
		}
	}
	z, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer z.Close()
	if _, err := io.Copy(w, z); err != nil {
		return fmt.Errorf("%s: %w", o.Key, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); o.SHA256 != "" && got != o.SHA256 {
		return fmt.Errorf("%s: checksum mismatch, the stored file changed after the backup", o.Key)
	}
	return nil
}

type ExportOptions struct {
	DB, Table string
	Part      string // "" (both) | schema | data
	NoDB      bool   // leave out CREATE DATABASE / USE, to load into any database
}

var kindOrder = map[string]int{"schema": 0, "data": 1, "views": 2, "routines": 3, "triggers": 4}

// ExportSQL streams a backup (or one database / one table of it) as one loadable .sql file.
func (a *App) ExportSQL(ctx context.Context, b *Backup, opt ExportOptions, w io.Writer) error {
	m, key, err := a.loadManifest(b)
	if err != nil {
		return err
	}
	if m.Method == "physical" || m.Method == "incremental" {
		return errors.New("a physical backup is a copy of the data files, not SQL: use Restore to rebuild a data folder from it, or download the raw file")
	}
	if m.Method != "" {
		return fmt.Errorf("a %s backup has no per-table SQL; download its files instead", m.Method)
	}
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return err
	}
	var pick []Object
	for _, o := range m.Objects {
		if opt.DB != "" && o.DB != opt.DB {
			continue
		}
		if opt.Table != "" && (o.Table != opt.Table || (o.Kind != "schema" && o.Kind != "data")) {
			continue
		}
		if opt.Part != "" && o.Kind != opt.Part {
			continue
		}
		pick = append(pick, o)
	}
	if len(pick) == 0 {
		return errors.New("nothing in this backup matches the request")
	}
	// Per database: tables, rows, views, routines, triggers (triggers after the rows on purpose).
	byDB := map[string][]Object{}
	for _, o := range pick {
		byDB[o.DB] = append(byDB[o.DB], o)
	}
	fmt.Fprintf(w, "-- backup-manager export\n-- backup %s, target %s, taken %s UTC\n", m.ID, m.Target, time.Unix(m.StartedAt, 0).UTC().Format("2006-01-02 15:04:05"))
	for _, k := range sortedKeys(m.Position) {
		fmt.Fprintf(w, "-- %s: %s\n", k, m.Position[k])
	}
	io.WriteString(w, SQLPreamble)
	for _, dbn := range sortedKeys(byDB) {
		if !opt.NoDB {
			fmt.Fprintf(w, "CREATE DATABASE IF NOT EXISTS %s;\nUSE %s;\n", ident(dbn), ident(dbn))
		}
		for step := 0; step <= 4; step++ {
			for _, o := range byDB[dbn] {
				if kindOrder[o.Kind] != step {
					continue
				}
				if err := copyObject(ctx, st, o, key, w); err != nil {
					return err
				}
			}
		}
	}
	_, err = io.WriteString(w, "/*!40014 SET FOREIGN_KEY_CHECKS=1, UNIQUE_CHECKS=1 */;\n-- end of export\n")
	return err
}

// VerifyBackup re-reads every object and checks checksums, decryption and decompression.
func (a *App) VerifyBackup(ctx context.Context, b *Backup) error {
	m, key, err := a.loadManifest(b)
	if err != nil {
		return err
	}
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return err
	}
	for _, o := range m.Objects {
		cw := &countWriter{w: io.Discard}
		if err := copyObject(ctx, st, o, key, cw); err != nil {
			return err
		}
		if cw.n != o.RawBytes {
			return fmt.Errorf("%s: expected %d bytes, read %d", o.Key, o.RawBytes, cw.n)
		}
	}
	return nil
}

// DeleteBackup removes the stored files and marks the record (the record stays for the history).
func (a *App) DeleteBackup(ctx context.Context, b *Backup, reason string) error {
	if b.Status == "running" || b.Status == "queued" {
		return errors.New("this backup is still in progress; cancel its job first")
	}
	if b.ChainID != "" && !strings.HasPrefix(reason, "retention") {
		var newer int
		a.Store.DB.QueryRow(`SELECT COUNT(*) FROM backups WHERE chain_id=? AND deleted_at=0 AND status='done' AND id<>? AND created_at>=?`, b.ChainID, b.ID, b.CreatedAt).Scan(&newer)
		if newer > 0 {
			return fmt.Errorf("%d newer incremental backup(s) need this one; delete them first", newer)
		}
	}
	st, _, err := a.storage(b.DestinationID)
	if err == nil {
		err = st.DeletePrefix(ctx, b.ID)
	}
	if err != nil {
		return fmt.Errorf("could not remove the files of %s: %w", b.ID, err)
	}
	a.RemoveSandbox(b.ID, "explore")
	_, err = a.Store.DB.Exec(`UPDATE backups SET deleted_at=?, delete_reason=?, manifest='' WHERE id=?`, now(), reason, b.ID)
	a.Hub.Publish("backup", b.ID)
	return err
}

// RetentionVictims picks what the plan's rules allow to delete. Pure function, easy to test.
// A full physical backup and its incrementals form one group that is kept or removed as a whole
// (an incremental is useless without what came before it); every other backup is its own group.
// Rules: only finished groups with no pinned member; a group's age is the age of its newest member;
// older than keepDays (when set); beyond the newest keepLast groups (when set); when both are set a
// group must break both to go; the newest group always stays.
func RetentionVictims(list []*Backup, keepDays, keepLast int, nowUnix int64) []*Backup {
	if keepDays <= 0 && keepLast <= 0 {
		return nil
	}
	type group struct {
		members []*Backup
		newest  int64
		pinned  bool
	}
	byKey := map[string]*group{}
	var groups []*group
	for _, b := range list {
		if b.Status != "done" || b.DeletedAt != 0 {
			continue
		}
		key := b.ChainID
		if key == "" {
			key = b.ID
		}
		g := byKey[key]
		if g == nil {
			g = &group{}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, b)
		g.pinned = g.pinned || b.Pinned
		if b.CreatedAt > g.newest {
			g.newest = b.CreatedAt
		}
	}
	sort.SliceStable(groups, func(i, k int) bool { return groups[i].newest > groups[k].newest })
	var out []*Backup
	for i, g := range groups {
		if i == 0 || g.pinned {
			continue
		}
		tooOld := keepDays > 0 && g.newest < nowUnix-int64(keepDays)*86400
		beyond := keepLast > 0 && i >= keepLast
		if (keepDays <= 0 || tooOld) && (keepLast <= 0 || beyond) {
			m := append([]*Backup{}, g.members...) // newest member first: a chain is taken apart from its end
			sort.SliceStable(m, func(x, y int) bool { return m[x].CreatedAt > m[y].CreatedAt })
			out = append(out, m...)
		}
	}
	return out
}

func (a *App) ApplyRetention(ctx context.Context, p *Plan) (int, error) {
	list, err := a.Store.PlanDoneBackups(p.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range RetentionVictims(list, p.KeepDays, p.KeepLast, now()) {
		reason := fmt.Sprintf("retention of plan %q (keep %d days, keep last %d)", p.Name, p.KeepDays, p.KeepLast)
		if err := a.DeleteBackup(ctx, b, reason); err != nil {
			return n, err
		}
		a.Store.Audit("system", "backup.delete", b.ID+": "+reason, "")
		n++
	}
	return n, nil
}

func humanBytes(n int64) string {
	const u = "KMGTP"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n)/1024, 0
	for f >= 1024 && i < len(u)-1 {
		f, i = f/1024, i+1
	}
	return fmt.Sprintf("%.1f %ciB", f, u[i])
}

// RunVerify is the queue handler for kind "verify": it proves a stored backup can be read back.
func (a *App) RunVerify(ctx context.Context, j *Job, report func(any)) error {
	b, err := a.Store.Backup(j.BackupID)
	if err != nil {
		return fmt.Errorf("backup %s not found: %w", j.BackupID, ErrNoRetry)
	}
	report(Progress{Phase: "verifying"})
	verr := a.VerifyBackup(ctx, b)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := ""
	if verr != nil {
		msg = verr.Error()
		a.Notify("failure", fmt.Sprintf("Backup %s FAILED verification: %v", b.ID, verr))
	}
	a.Store.DB.Exec(`UPDATE backups SET verified_at=?, verify_error=? WHERE id=?`, now(), msg, b.ID)
	a.Hub.Publish("backup", b.ID)
	if verr != nil {
		return fmt.Errorf("%w: %w", verr, ErrNoRetry)
	}
	return nil
}
