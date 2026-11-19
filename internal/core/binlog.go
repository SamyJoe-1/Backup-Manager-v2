package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

var reBinlogName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// BinlogFile is one archived binary log of a database server.
type BinlogFile struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Closed     bool   `json:"closed"` // false = it was still being written when archived; it is fetched again next run
	ArchivedAt int64  `json:"archivedAt"`
	key        string
	wrapped    []byte
	destID     int64
}

func (a *App) Binlogs(targetID int64) ([]BinlogFile, error) {
	rows, err := a.Store.DB.Query(`SELECT name,size,closed,archived_at,obj_key,wrapped_key,destination_id FROM binlogs WHERE target_id=? ORDER BY name`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BinlogFile{}
	for rows.Next() {
		var f BinlogFile
		if err := rows.Scan(&f.Name, &f.Size, &f.Closed, &f.ArchivedAt, &f.key, &f.wrapped, &f.destID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RunBinlog is the queue handler for kind "binlog": it copies every binary log the server has that
// is not archived yet. The log still being written is copied too and replaced on the next run, so
// the most that can be lost is the time between two runs.
func (a *App) RunBinlog(ctx context.Context, j *Job, report func(any)) error {
	p, err := a.Store.Plan(j.PlanID)
	if err != nil {
		return fmt.Errorf("the plan was deleted: %w", ErrNoRetry)
	}
	err = a.archiveBinlogs(ctx, j, p, report)
	if err != nil && (j.Attempt >= j.MaxAttempts || errors.Is(err, ErrNoRetry)) {
		a.Notify("failure", fmt.Sprintf("Binary log archive FAILED: %s: %v", p.Name, err))
	}
	return err
}

func (a *App) archiveBinlogs(ctx context.Context, j *Job, p *Plan, report func(any)) error {
	t, pw, err := a.targetDB(p.TargetID)
	if err != nil {
		return err
	}
	var cfg TargetConfig
	json.Unmarshal(t.Cfg, &cfg)
	run, err := NewRunner(cfg.Exec)
	if err != nil {
		return fmt.Errorf("%w: %w", err, ErrNoRetry)
	}
	st, _, err := a.storage(p.DestinationID)
	if err != nil {
		return err
	}
	db, err := OpenTarget(string(t.Cfg), pw)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return fmt.Errorf("cannot list binary logs on %q (is log_bin on, and does the user have BINLOG MONITOR?): %w: %w", t.Name, err, ErrNoRetry)
	}
	type lf struct {
		name string
		size int64
	}
	var logs []lf
	for rows.Next() {
		var l lf
		if err := rows.Scan(&l.name, &l.size); err != nil {
			rows.Close()
			return err
		}
		logs = append(logs, l)
	}
	rows.Close()
	if len(logs) == 0 {
		return fmt.Errorf("%q has no binary logs (log_bin is off): %w", t.Name, ErrNoRetry)
	}
	have := map[string]BinlogFile{}
	old, err := a.Binlogs(t.ID)
	if err != nil {
		return err
	}
	for _, f := range old {
		if f.destID == p.DestinationID {
			have[f.name()] = f
		}
	}
	tmp := "/tmp/bm-binlog-" + strconv.FormatInt(j.ID, 10)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		run.Output(c, "", "rm", "-rf", tmp)
		cancel()
	}()
	prog := Progress{Phase: "archiving binary logs", TablesTotal: len(logs)}
	for i, l := range logs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !reBinlogName.MatchString(l.name) {
			return fmt.Errorf("unexpected binary log name %q: %w", l.name, ErrNoRetry)
		}
		last := i == len(logs)-1
		prog.Table, prog.TablesDone = l.name, i
		report(prog)
		if f, ok := have[l.name]; ok && f.Closed {
			continue
		}
		if _, err := run.Output(ctx, "", "mkdir", "-p", tmp); err != nil {
			return err
		}
		args := append([]string{"mariadb-binlog", "--read-from-remote-server", "--raw", "--result-file=" + tmp + "/"}, connArgs(run, cfg)...)
		if _, err := run.Output(ctx, pw, append(args, l.name)...); err != nil {
			return fmt.Errorf("reading %s: %w", l.name, err)
		}
		dataKey, wrapped, err := []byte(nil), []byte(nil), error(nil)
		if !p.Spec.NoEncrypt {
			if dataKey, wrapped, err = a.Keys.NewDataKey(); err != nil {
				return err
			}
		}
		key := fmt.Sprintf("binlog/%d/%s%s", t.ID, l.name, objectSuffix(dataKey != nil))
		w, err := newObjectWriter(ctx, st, key, dataKey, p.Spec.Level, nil, func(n int) { prog.RawBytes += int64(n); report(prog) }, nil)
		if err != nil {
			return err
		}
		cat := run.Cmd(ctx, "", false, "cat", tmp+"/"+l.name)
		cat.Stdout = w
		raw, _, _, err := w.finish(cat.Run())
		run.Output(ctx, "", "rm", "-f", tmp+"/"+l.name)
		if err != nil {
			return fmt.Errorf("storing %s: %w", l.name, err)
		}
		_, err = a.Store.DB.Exec(`INSERT INTO binlogs(target_id,destination_id,name,size,closed,obj_key,wrapped_key,archived_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(target_id,destination_id,name) DO UPDATE SET size=excluded.size, closed=excluded.closed, obj_key=excluded.obj_key, wrapped_key=excluded.wrapped_key, archived_at=excluded.archived_at`,
			t.ID, p.DestinationID, l.name, raw, !last, key, wrapped, now())
		if err != nil {
			return err
		}
	}
	a.Store.DB.Exec(`UPDATE plans SET last_run_at=? WHERE id=?`, now(), p.ID)
	// retention: closed logs older than the plan's age limit
	if p.KeepDays > 0 {
		cut := now() - int64(p.KeepDays)*86400
		for _, f := range old {
			if f.destID == p.DestinationID && f.Closed && f.ArchivedAt < cut {
				if err := st.DeletePrefix(ctx, f.key); err == nil {
					a.Store.DB.Exec(`DELETE FROM binlogs WHERE target_id=? AND destination_id=? AND name=?`, t.ID, p.DestinationID, f.Name)
				}
			}
		}
	}
	return nil
}

func (f BinlogFile) name() string { return f.Name }

// layBinlogs writes the archived binary logs needed to roll a restored backup forward into
// <dir>/bm-binlogs and returns the replay command.
func (a *App) layBinlogs(ctx context.Context, run *Runner, b *Backup, dir string, stopAt int64) (int, string, error) {
	m, _, err := a.loadManifest(b)
	if err != nil {
		return 0, "", err
	}
	first, pos := m.Position["binlog_file"], m.Position["binlog_position"]
	if first == "" {
		return 0, "", errors.New("this backup has no binary log position (it was taken from a server without log_bin), so it cannot be rolled forward")
	}
	all, err := a.Binlogs(b.TargetID)
	if err != nil {
		return 0, "", err
	}
	var need []BinlogFile
	for _, f := range all {
		if f.Name >= first {
			need = append(need, f)
		}
	}
	if len(need) == 0 || need[0].Name != first {
		return 0, "", fmt.Errorf("binary log %s is not archived, so the time after the backup cannot be replayed (is a binary-log plan running for this database?)", first)
	}
	last := need[len(need)-1]
	if stopAt > last.ArchivedAt {
		return 0, "", fmt.Errorf("the archive ends at %s UTC, before the requested moment; run the binary-log plan first", time.Unix(last.ArchivedAt, 0).UTC().Format("2006-01-02 15:04:05"))
	}
	out := dir + "/bm-binlogs"
	if _, err := run.Output(ctx, "", "mkdir", "-p", out); err != nil {
		return 0, "", err
	}
	cmdline := fmt.Sprintf("cd %s && mariadb-binlog --start-position=%s --stop-datetime='%s'", out, pos, time.Unix(stopAt, 0).UTC().Format("2006-01-02 15:04:05"))
	for _, f := range need {
		st, _, err := a.storage(f.destID)
		if err != nil {
			return 0, "", err
		}
		var key []byte
		if len(f.wrapped) > 0 {
			if key, err = a.Keys.Open(f.wrapped); err != nil {
				return 0, "", errors.New("cannot unwrap a binary log key (master key changed?)")
			}
		}
		cmd := run.Cmd(ctx, "", true, "sh", "-c", `cat > "$1"`, "sh", out+"/"+f.Name)
		pr, pw := io.Pipe()
		cmd.Stdin = pr
		if err := cmd.Start(); err != nil {
			return 0, "", err
		}
		cerr := copyObject(ctx, st, Object{Key: f.key}, key, pw)
		pw.CloseWithError(cerr)
		werr := cmd.Wait()
		pr.Close()
		if cerr != nil {
			return 0, "", cerr
		}
		if werr != nil {
			return 0, "", werr
		}
		cmdline += " " + f.Name
	}
	// TZ=UTC makes --stop-datetime mean UTC whatever the machine's time zone is.
	return len(need), "TZ=UTC sh -c \"" + cmdline + " | mariadb\"", nil
}
