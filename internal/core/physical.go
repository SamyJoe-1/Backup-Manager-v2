package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Runner starts the native backup tools where the database files are: on this machine ("local")
// or inside a Docker container ("docker:<name>").
type Runner struct{ container string }

func NewRunner(mode string) (*Runner, error) {
	mode = strings.TrimSpace(mode)
	switch {
	case mode == "local":
		return &Runner{}, nil
	case strings.HasPrefix(mode, "docker:") && len(mode) > 7:
		name := mode[7:]
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`).MatchString(name) {
			return nil, fmt.Errorf("container name %q is not valid", name)
		}
		return &Runner{container: name}, nil
	case mode == "":
		return nil, errors.New("this database has no place set to run the backup tools (edit it and set \"local\" or \"docker:<container>\")")
	}
	return nil, fmt.Errorf("run mode %q must be \"local\" or \"docker:<container>\"", mode)
}

// Cmd builds a command. password is handed over as MYSQL_PWD through the environment, never on a
// command line where other users of the machine could read it.
func (r *Runner) Cmd(ctx context.Context, password string, stdin bool, args ...string) *exec.Cmd {
	env := map[string]string{}
	if password != "" {
		env["MYSQL_PWD"] = password
	}
	return r.CmdEnv(ctx, env, stdin, args...)
}

// CmdEnv is Cmd with any secret environment (values travel through the environment only).
func (r *Runner) CmdEnv(ctx context.Context, env map[string]string, stdin bool, args ...string) *exec.Cmd {
	var c *exec.Cmd
	if r.container == "" {
		c = exec.CommandContext(ctx, args[0], args[1:]...)
	} else {
		full := []string{"exec"}
		if stdin {
			full = append(full, "-i")
		}
		for _, k := range sortedKeys(env) {
			full = append(full, "-e", k)
		}
		c = exec.CommandContext(ctx, "docker", append(append(full, r.container), args...)...)
	}
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	return c
}

func (r *Runner) OutputEnv(ctx context.Context, env map[string]string, args ...string) (string, error) {
	c := r.CmdEnv(ctx, env, false, args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", args[0], err, lastLines(errb.String(), 6))
	}
	return out.String(), nil
}

func (r *Runner) Output(ctx context.Context, password string, args ...string) (string, error) {
	c := r.Cmd(ctx, password, false, args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", args[0], err, lastLines(errb.String(), 6))
	}
	return out.String(), nil
}

// KillMarked stops every process in the run place whose command line contains marker. Killing
// `docker exec` alone leaves the tool running inside the container, so cancel needs this.
func (r *Runner) KillMarked(marker string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	script := `for p in /proc/[0-9]*; do [ "${p#/proc/}" = "$$" ] && continue; if tr '\0' ' ' < "$p/cmdline" 2>/dev/null | grep -q -- "$1"; then kill "${p#/proc/}" 2>/dev/null; fi; done; true`
	r.Cmd(ctx, "", false, "sh", "-c", script, "sh", "--extra-lsndir=/tmp/"+marker).Run()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// tailBuffer keeps the end of a tool's log output.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16<<10 {
		t.buf = t.buf[len(t.buf)-8<<10:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.buf) }

// connArgs are the connection flags for a tool running in the run place. Inside a container the
// server is reached through its own default socket, so only the user is passed.
func connArgs(r *Runner, c TargetConfig) []string {
	a := []string{"--user=" + c.User}
	if r.container != "" {
		return a
	}
	if c.Socket != "" {
		return append(a, "--socket="+c.Socket)
	}
	port := c.Port
	if port == 0 {
		port = 3306
	}
	return append(a, "--host="+c.Host, fmt.Sprintf("--port=%d", port))
}

// ReplicaState describes a replica's health, or Replica=false for a primary.
type ReplicaState struct {
	Replica bool
	Running bool
	Lag     int64 // seconds behind, -1 when unknown
}

func replicaState(ctx context.Context, db *sql.DB) (ReplicaState, error) {
	rows, err := db.QueryContext(ctx, "SHOW SLAVE STATUS")
	if err != nil {
		// no privilege to look: treat as unknown primary rather than block the backup
		return ReplicaState{}, nil
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return ReplicaState{}, rows.Err()
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return ReplicaState{}, err
	}
	m := map[string]string{}
	for i, c := range cols {
		m[c] = string(vals[i])
	}
	st := ReplicaState{Replica: true, Running: m["Slave_IO_Running"] == "Yes" && m["Slave_SQL_Running"] == "Yes", Lag: -1}
	if s := m["Seconds_Behind_Master"]; s != "" {
		fmt.Sscan(s, &st.Lag)
	}
	return st, nil
}

// checkReplica refuses to take a backup from a replica that is broken or too far behind: such a
// backup would be an old or stale copy that looks fine.
func checkReplica(ctx context.Context, db *sql.DB, maxLag int) error {
	st, err := replicaState(ctx, db)
	if err != nil || !st.Replica {
		return err
	}
	if !st.Running {
		return errors.New("this server is a replica and its replication is stopped; a backup from it would be stale")
	}
	if maxLag <= 0 {
		maxLag = 300
	}
	if st.Lag < 0 || st.Lag > int64(maxLag) {
		return fmt.Errorf("this replica is %d seconds behind its primary (limit %d); waiting for it to catch up", st.Lag, maxLag)
	}
	return nil
}

var (
	reToLSN   = regexp.MustCompile(`(?m)^to_lsn\s*=\s*(\d+)`)
	reFromLSN = regexp.MustCompile(`(?m)^from_lsn\s*=\s*(\d+)`)
	reBinlog  = regexp.MustCompile(`binlog_pos = filename '([^']+)', position '(\d+)'(?:, GTID of the last change '([^']*)')?`)
)

// chainTip returns the newest usable physical backup of a plan and how many incrementals its chain has.
func (a *App) chainTip(planID, destID int64) (tip *Backup, incrementals int) {
	row := a.Store.DB.QueryRow(`SELECT `+backupCols+` FROM backups WHERE plan_id=? AND destination_id=? AND status='done' AND deleted_at=0
		AND method IN ('physical','incremental') ORDER BY created_at DESC, id DESC LIMIT 1`, planID, destID)
	b, err := scanBackup(row)
	if err != nil || b.VerifyError != "" || b.LSNTo == "" {
		return nil, 0
	}
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM backups WHERE chain_id=? AND method='incremental' AND status='done' AND deleted_at=0`, b.ChainID).Scan(&incrementals)
	return b, incrementals
}

// doPhysical takes a hot physical backup with mariabackup and streams it straight to storage.
// With a parent it copies only the pages changed since that backup.
func (a *App) doPhysical(ctx context.Context, j *Job, b *Backup, p *Plan, report func(any)) (*Manifest, []byte, error) {
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
	db, err := OpenTarget(string(t.Cfg), pw)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if err := checkReplica(ctx, db, cfg.MaxLag); err != nil {
		return nil, nil, err
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return nil, nil, fmt.Errorf("cannot reach %q: %w", t.Name, err)
	}
	if !strings.Contains(strings.ToLower(version), "mariadb") {
		return nil, nil, fmt.Errorf("physical backups support MariaDB only for now (server reports %s): %w", version, ErrNoRetry)
	}
	var est int64
	if tables, err := ListTables(ctx, db); err == nil {
		for _, ti := range tables {
			est += ti.Bytes
		}
	}

	var parent *Backup
	if spec.IncrementalsPerFull > 0 {
		if a.Remote != nil {
			parent = a.Remote.Parent // the panel knows the chain; an agent has no catalog of its own
		} else if tip, n := a.chainTip(p.ID, b.DestinationID); tip != nil && n < spec.IncrementalsPerFull {
			parent = tip
		}
	}
	if free := st.Free(); free >= 0 && parent == nil {
		if need := est/2 + 256<<20; free < need {
			return nil, nil, fmt.Errorf("destination has %s free but this backup may need about %s", humanBytes(free), humanBytes(need))
		}
	}

	dataKey, wrapped, err := a.newDataKey(spec)
	if err != nil {
		return nil, nil, err
	}
	m := &Manifest{Format: 1, ID: b.ID, Tool: "backup-manager " + Version, Plan: p.Name, Target: t.Name, ServerVersion: version,
		StartedAt: now(), Position: map[string]string{}, Spec: spec, Compression: "zstd", Encrypted: dataKey != nil, Method: "physical", ChainID: b.ID}
	marker := "bm-" + b.ID
	lsnDir := "/tmp/" + marker
	parallel := spec.Parallel
	if parallel < 1 || parallel > 8 {
		parallel = 1
	}
	args := []string{"nice", "-n", "19", "mariabackup", "--backup", "--stream=mbstream", "--extra-lsndir=" + lsnDir, fmt.Sprintf("--parallel=%d", parallel)}
	args = append(args, connArgs(run, cfg)...)
	if parent != nil {
		m.Method, m.ParentID, m.ChainID, m.LSNFrom = "incremental", parent.ID, parent.ChainID, parent.LSNTo
		args = append(args, "--incremental-lsn="+parent.LSNTo)
		est = 0
	}
	if rs, _ := replicaState(ctx, db); rs.Replica {
		args = append(args, "--slave-info")
	}

	prog := Progress{Phase: "copying data files", TablesTotal: 1, EstBytes: est}
	if parent != nil {
		prog.Phase = "copying changed pages"
	}
	started := time.Now()
	limit := &rateLimiter{perSec: int64(spec.RateMB) << 20}
	key := b.ID + "/physical.mbstream" + objectSuffix(dataKey != nil)
	w, err := newObjectWriter(ctx, st, key, dataKey, spec.Level, limit, func(n int) {
		prog.RawBytes += int64(n)
		if d := time.Since(started).Seconds(); d > 0 {
			prog.Speed = float64(prog.RawBytes) / d
		}
		report(prog)
	}, func(n int) { prog.StoredBytes += int64(n) })
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		run.Output(c, "", "rm", "-rf", lsnDir)
		cancel()
	}
	defer cleanup()

	cmd := run.Cmd(ctx, pw, false, args...)
	logTail := &tailBuffer{}
	cmd.Stdout, cmd.Stderr = w, logTail
	runErr := cmd.Run()
	if ctx.Err() != nil {
		run.KillMarked(marker)
		w.finish(ctx.Err())
		return nil, nil, ctx.Err()
	}
	if runErr != nil || !strings.Contains(logTail.String(), "completed OK!") {
		ferr := fmt.Errorf("mariabackup failed: %v: %s", runErr, lastLines(logTail.String(), 5))
		w.finish(ferr)
		return nil, nil, ferr
	}
	raw, stored, sum, err := w.finish(nil)
	if err != nil {
		return nil, nil, err
	}
	checkpoints, err := run.Output(ctx, "", "cat", lsnDir+"/xtrabackup_checkpoints")
	if err != nil {
		return nil, nil, fmt.Errorf("the backup finished but its checkpoint file could not be read: %w", err)
	}
	info, _ := run.Output(ctx, "", "cat", lsnDir+"/xtrabackup_info")
	if mm := reToLSN.FindStringSubmatch(checkpoints); mm != nil {
		m.LSNTo = mm[1]
	} else {
		return nil, nil, errors.New("the backup finished but reported no end position (to_lsn)")
	}
	if mm := reFromLSN.FindStringSubmatch(checkpoints); mm != nil && parent != nil && mm[1] != parent.LSNTo {
		return nil, nil, fmt.Errorf("incremental starts at %s but its parent ended at %s", mm[1], parent.LSNTo)
	}
	if mm := reBinlog.FindStringSubmatch(info); mm != nil {
		m.Position["binlog_file"], m.Position["binlog_position"] = mm[1], mm[2]
		if mm[3] != "" {
			m.Position["gtid"] = mm[3]
		}
	}
	m.Position["lsn"] = m.LSNTo
	m.Objects = []Object{{Kind: "physical", Key: key, RawBytes: raw, StoredBytes: stored, SHA256: sum}}
	m.FinishedAt = now()

	suffix := objectSuffix(dataKey != nil)
	mj, _ := json.MarshalIndent(m, "", " ")
	mkey := b.ID + "/manifest.json" + suffix
	mw, err := newObjectWriter(ctx, st, mkey, dataKey, spec.Level, nil, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	_, werr := mw.Write(mj)
	if _, _, _, err := mw.finish(werr); err != nil {
		return nil, nil, err
	}
	meta, _ := json.Marshal(Meta{ID: b.ID, Encrypted: dataKey != nil, WrappedKey: hex.EncodeToString(wrapped), Manifest: mkey})
	if _, err := st.Put(ctx, b.ID+"/meta.json", bytes.NewReader(meta)); err != nil {
		return nil, nil, err
	}
	return m, wrapped, nil
}

// RestoreRequest asks for a ready-to-start data folder built from a physical backup.
type RestoreRequest struct {
	BackupID string `json:"backupId"`
	Dir      string `json:"dir"`    // absolute path in the run place; must not exist or be empty
	Exec     string `json:"exec"`   // "" = the database's own run place
	StopAt   int64  `json:"stopAt"` // unix seconds; 0 = restore to the backup's own moment
}

func (a *App) chain(b *Backup) ([]*Backup, error) {
	if b.Method != "physical" && b.Method != "incremental" {
		return nil, fmt.Errorf("backup %s is not a physical backup", b.ID)
	}
	rows, err := a.Store.DB.Query(`SELECT `+backupCols+` FROM backups WHERE chain_id=? AND status='done' AND deleted_at=0 AND created_at<=? ORDER BY created_at, id`, b.ChainID, b.CreatedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []*Backup
	for rows.Next() {
		x, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, x)
	}
	// follow parent links back from the wanted backup, so a broken chain is caught, not guessed
	byID := map[string]*Backup{}
	for _, x := range all {
		byID[x.ID] = x
	}
	var out []*Backup
	for cur := byID[b.ID]; cur != nil; cur = byID[cur.ParentID] {
		out = append([]*Backup{cur}, out...)
		if cur.Method == "physical" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("the chain of backup %s is broken: an earlier backup it depends on is missing", b.ID)
}

// RunRestore is the queue handler for kind "restore": it extracts the full backup, applies every
// incremental up to the chosen one, and (for a point in time) lays the binary logs next to it.
func (a *App) RunRestore(ctx context.Context, j *Job, report func(any)) error {
	var req RestoreRequest
	if err := json.Unmarshal(j.Payload, &req); err != nil {
		return fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	fail := func(err error) error { return fmt.Errorf("%w: %w", err, ErrNoRetry) } // a restore is never retried blindly
	b, err := a.Store.Backup(req.BackupID)
	if err != nil {
		return fail(errors.New("backup not found"))
	}
	links, err := a.chain(b)
	if err != nil {
		return fail(err)
	}
	t, _, err := a.targetDB(b.TargetID)
	if err != nil {
		return fail(err)
	}
	var cfg TargetConfig
	json.Unmarshal(t.Cfg, &cfg)
	mode := req.Exec
	if mode == "" {
		mode = cfg.Exec
	}
	run, err := NewRunner(mode)
	if err != nil {
		return fail(err)
	}
	dir := path.Clean(req.Dir)
	if !path.IsAbs(dir) || dir == "/" || strings.Count(dir, "/") < 2 {
		return fail(fmt.Errorf("restore folder %q must be an absolute path at least two levels deep", req.Dir))
	}
	// Never write into a folder that has something in it (for example a live data folder).
	if out, err := run.Output(ctx, "", "sh", "-c", `if [ -e "$1" ] && [ -n "$(ls -A "$1" 2>/dev/null)" ]; then echo busy; fi`, "sh", dir); err != nil {
		return fail(err)
	} else if strings.Contains(out, "busy") {
		return fail(fmt.Errorf("the folder %s is not empty; choose a new folder", dir))
	}
	total := len(links)
	if err := a.restoreChain(ctx, run, b, links, dir, report); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(err)
	}
	prog := Progress{TablesTotal: total}
	note := fmt.Sprintf("Restored backup %s (%d step(s)) into %s.\nGive the folder to the mysql user (chown -R) and start a MariaDB server of the same version on it.\n", b.ID, total, dir)
	if req.StopAt > 0 {
		n, cmdline, err := a.layBinlogs(ctx, run, b, dir, req.StopAt)
		if err != nil {
			return fail(err)
		}
		note += fmt.Sprintf("%d binary log file(s) are in %s/bm-binlogs. After the server is up, replay them with:\n%s\n", n, dir, cmdline)
	}
	c := run.Cmd(ctx, "", true, "sh", "-c", `cat > "$1"`, "sh", dir+"/BM-RESTORE.txt")
	c.Stdin = strings.NewReader(note)
	if err := c.Run(); err != nil {
		return fail(err)
	}
	prog.Phase, prog.TablesDone = "ready", total
	report(prog)
	a.Store.Audit("system", "restore.ready", b.ID+" -> "+dir, "")
	return nil
}

// restoreChain extracts a full backup into dir (in the run place) and applies its incrementals in order.
func (a *App) restoreChain(ctx context.Context, run *Runner, b *Backup, links []*Backup, dir string, report func(any)) error {
	st, _, err := a.storage(b.DestinationID)
	if err != nil {
		return err
	}
	total := len(links)
	prog := Progress{Phase: "extracting", TablesTotal: total}
	extract := func(x *Backup, into string) error {
		m, key, err := a.loadManifest(x)
		if err != nil {
			return err
		}
		if len(m.Objects) != 1 {
			return fmt.Errorf("backup %s has an unexpected layout", x.ID)
		}
		if _, err := run.Output(ctx, "", "mkdir", "-p", into); err != nil {
			return err
		}
		cmd := run.Cmd(ctx, "", true, "mbstream", "-x", "-C", into)
		pr, pw := io.Pipe()
		logTail := &tailBuffer{}
		cmd.Stdin, cmd.Stderr = pr, logTail
		if err := cmd.Start(); err != nil {
			return err
		}
		cw := &countWriter{w: pw, f: func(n int) { prog.RawBytes += int64(n); report(prog) }}
		cerr := copyObject(ctx, st, m.Objects[0], key, cw)
		pw.CloseWithError(cerr)
		werr := cmd.Wait()
		pr.Close()
		if cerr != nil {
			return cerr
		}
		if werr != nil {
			return fmt.Errorf("unpacking %s: %v: %s", x.ID, werr, lastLines(logTail.String(), 4))
		}
		return nil
	}
	prepare := func(args ...string) error {
		out := run.Cmd(ctx, "", false, append([]string{"mariabackup", "--prepare", "--target-dir=" + dir}, args...)...)
		logTail := &tailBuffer{}
		out.Stdout, out.Stderr = io.Discard, logTail
		if err := out.Run(); err != nil || !strings.Contains(logTail.String(), "completed OK!") {
			return fmt.Errorf("prepare failed: %v: %s", err, lastLines(logTail.String(), 5))
		}
		return nil
	}
	for i, x := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		prog.Table, prog.TablesDone = x.ID, i
		report(prog)
		if i == 0 {
			if err := extract(x, dir); err != nil {
				return err
			}
			prog.Phase = "preparing"
			report(prog)
			if err := prepare(); err != nil {
				return err
			}
			continue
		}
		inc := dir + ".inc" + fmt.Sprint(i)
		prog.Phase = "applying incremental"
		err := extract(x, inc)
		if err == nil {
			err = prepare("--incremental-dir=" + inc)
		}
		run.Output(context.Background(), "", "rm", "-rf", inc)
		if err != nil {
			return err
		}
	}
	return nil
}
