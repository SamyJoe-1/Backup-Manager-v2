package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

func (a *App) OpenDestination(id int64) (Storage, error) {
	st, _, err := a.storage(id)
	return st, err
}

// TestTarget connects and reports what the backup user can see.
func (a *App) TestTarget(ctx context.Context, id int64) (map[string]any, error) {
	t, pw, err := a.targetDB(id)
	if err != nil {
		return nil, err
	}
	if agent := endpointAgent(t); agent > 0 && a.Remote == nil {
		list, _ := a.Agents()
		for _, x := range list {
			if x.ID == agent {
				if !x.Online {
					return nil, fmt.Errorf("agent %q is offline (last seen %s)", x.Name, agoText(x.LastSeen))
				}
				return map[string]any{"ok": true, "version": "agent " + x.Name + " is online; the connection itself is checked when a backup runs"}, nil
			}
		}
		return nil, errors.New("the agent of this target was deleted")
	}
	switch t.Kind {
	case "files":
		var cfg FilesConfig
		json.Unmarshal(t.Cfg, &cfg)
		if len(cfg.Paths) == 0 {
			return nil, errors.New("no paths listed")
		}
		for _, p := range cfg.Paths {
			if _, err := os.Stat(p); err != nil {
				return nil, err
			}
			if _, err := os.ReadDir(p); err != nil {
				if f, ferr := os.Open(p); ferr != nil {
					return nil, ferr
				} else {
					f.Close()
				}
			}
		}
		return map[string]any{"ok": true, "version": fmt.Sprintf("%d path(s) readable", len(cfg.Paths))}, nil
	case "redis":
		var cfg RedisConfig
		json.Unmarshal(t.Cfg, &cfg)
		rc, err := dialRedis(ctx, cfg, pw)
		if err != nil {
			return nil, err
		}
		defer rc.c.Close()
		srv, err := redisInfo(rc, "server")
		if err != nil {
			return nil, err
		}
		mem, _ := redisInfo(rc, "memory")
		used, _ := strconv.ParseInt(mem["used_memory"], 10, 64)
		return map[string]any{"ok": true, "version": "Redis " + srv["redis_version"], "bytes": used}, nil
	case "postgres":
		var cfg TargetConfig
		json.Unmarshal(t.Cfg, &cfg)
		run, err := NewRunner(cfg.Exec)
		if err != nil {
			return nil, err
		}
		ver, err := pgQuery(ctx, run, cfg, pw, "postgres", "SHOW server_version")
		if err != nil || len(ver) == 0 {
			return nil, fmt.Errorf("cannot reach PostgreSQL: %v", err)
		}
		dbs, _ := pgDatabases(ctx, run, cfg, pw)
		return map[string]any{"ok": true, "version": "PostgreSQL " + ver[0], "tables": len(dbs)}, nil
	}
	db, err := OpenTarget(string(t.Cfg), pw)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "version": version}
	var readOnly, logBin any
	if db.QueryRowContext(ctx, "SELECT @@global.read_only, @@global.log_bin").Scan(&readOnly, &logBin) == nil {
		out["readOnly"], out["logBin"] = toText(readOnly) == "1", toText(logBin) == "1"
	}
	tables, err := ListTables(ctx, db)
	if err != nil {
		return nil, err
	}
	var bytes int64
	for _, ti := range tables {
		bytes += ti.Bytes
	}
	out["tables"], out["bytes"] = len(tables), bytes
	return out, nil
}

func toText(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case int64:
		if x != 0 {
			return "1"
		}
		return "0"
	case string:
		return x
	}
	return ""
}

func (a *App) TargetTables(ctx context.Context, id int64) ([]TableInfo, error) {
	t, pw, err := a.targetDB(id)
	if err != nil {
		return nil, err
	}
	if endpointAgent(t) > 0 && a.Remote == nil {
		return []TableInfo{}, nil // the table list lives on the agent's network; type names by hand
	}
	if t.Kind == "postgres" {
		var cfg TargetConfig
		json.Unmarshal(t.Cfg, &cfg)
		run, err := NewRunner(cfg.Exec)
		if err != nil {
			return nil, err
		}
		dbs, err := pgDatabases(ctx, run, cfg, pw)
		if err != nil {
			return nil, err
		}
		out := []TableInfo{}
		for _, d := range dbs {
			lines, err := pgQuery(ctx, run, cfg, pw, d, `SELECT table_name || '|' || pg_total_relation_size(quote_ident(table_schema)||'.'||quote_ident(table_name)) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') AND table_type='BASE TABLE' ORDER BY 1`)
			if err != nil {
				continue
			}
			for _, l := range lines {
				name, size, _ := strings.Cut(l, "|")
				n, _ := strconv.ParseInt(size, 10, 64)
				out = append(out, TableInfo{DB: d, Name: name, Type: "BASE TABLE", Bytes: n})
			}
		}
		return out, nil
	}
	if t.Kind != "mysql" {
		return []TableInfo{}, nil
	}
	db, err := OpenTarget(string(t.Cfg), pw)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return ListTables(ctx, db)
}

// OpenRawObject opens one stored file of a backup. The key must be listed in the manifest.
func (a *App) OpenRawObject(ctx context.Context, b *Backup, key string) (io.ReadSeekCloser, string, error) {
	m, _, err := a.loadManifest(b)
	if err != nil {
		return nil, "", err
	}
	for _, o := range m.Objects {
		if o.Key == key {
			st, _, err := a.storage(b.DestinationID)
			if err != nil {
				return nil, "", err
			}
			rs, _, err := st.Open(ctx, key)
			return rs, path.Base(key), err
		}
	}
	return nil, "", errors.New("this file is not part of the backup")
}
