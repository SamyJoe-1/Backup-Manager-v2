package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"backupmanager/internal/core"
)

// doctor checks what this machine allows the program to do, so a missing tool or permission is
// found before a backup fails at night. Run it as the service user: the installer does.
func doctor() error {
	p := loadPaths()
	ok, warn, bad := 0, 0, 0
	line := func(state, what, detail string) {
		mark := map[string]string{"ok": "  ok  ", "warn": " note ", "bad": " FAIL "}[state]
		switch state {
		case "ok":
			ok++
		case "warn":
			warn++
		default:
			bad++
		}
		if detail != "" {
			what += " — " + detail
		}
		fmt.Printf("[%s] %s\n", mark, what)
	}
	who, _ := user.Current()
	name := "?"
	if who != nil {
		name = who.Username
	}
	fmt.Printf("backup-manager %s, running as %s\n\n", version, name)

	// the program's own files
	if err := os.MkdirAll(p.data, 0o700); err != nil {
		line("bad", "data folder "+p.data, err.Error())
	} else if f, err := os.CreateTemp(p.data, ".doctor-*"); err != nil {
		line("bad", "data folder "+p.data, "not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
		var st syscall.Statfs_t
		if syscall.Statfs(p.data, &st) == nil {
			free := int64(st.Bavail) * int64(st.Bsize)
			state := "ok"
			if free < 5<<30 {
				state = "warn"
			}
			line(state, "data folder "+p.data, fmt.Sprintf("writable, %.1f GB free", float64(free)/(1<<30)))
		}
	}
	if _, created, err := core.LoadOrCreateMaster(p.key); err != nil {
		line("bad", "master key "+p.key, err.Error())
	} else if created {
		line("warn", "master key "+p.key, "was missing and has just been created; keep a copy of it")
	} else {
		line("ok", "master key "+p.key, "readable")
	}
	if srv := os.Getenv("BM_AGENT_SERVER"); srv != "" {
		line("ok", "agent of "+srv, "this machine runs backups for that panel")
	} else if st, err := core.OpenStore(filepath.Join(p.data, "catalog.db")); err != nil {
		line("bad", "catalog", err.Error())
	} else {
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
		if n == 0 {
			line("warn", "catalog", "no account yet: run  backup-manager setup-link")
		} else {
			line("ok", "catalog", fmt.Sprintf("%d account(s)", n))
		}
		st.DB.Close()
	}

	// tools started next to the data
	tool := func(name, use string) {
		if path, err := exec.LookPath(name); err == nil {
			line("ok", name, path)
		} else {
			line("warn", name+" not found", "needed for "+use+" of a database installed directly on this machine (a database in Docker uses the tools inside its container)")
		}
	}
	tool("mariabackup", "physical backups")
	tool("mbstream", "physical backups and restores")
	tool("mariadb-binlog", "binary-log archiving")
	tool("pg_dump", "PostgreSQL backups")
	if _, err := exec.LookPath("docker"); err != nil {
		line("warn", "docker not found", "needed for databases in containers, the row-search sandbox and restore tests")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").CombinedOutput()
		cancel()
		if err != nil {
			line("warn", "docker present but not usable by "+name, strings.TrimSpace(lastLine(string(out)))+" (the installer can grant access)")
		} else {
			n := len(strings.Fields(string(out)))
			line("ok", "docker", fmt.Sprintf("usable, %d container(s) running", n))
		}
	}

	// settings
	if os.Getenv("BM_AGENT_SERVER") != "" {
		// an agent opens no port
	} else if d := os.Getenv("BM_TLS_DOMAIN"); d != "" {
		line("ok", "HTTPS", "Let's Encrypt for "+d+" on "+env("BM_HTTPS_LISTEN", ":443"))
	} else if os.Getenv("BM_TLS_CERT") != "" {
		line("ok", "HTTPS", "own certificate "+os.Getenv("BM_TLS_CERT"))
	} else if strings.HasPrefix(p.listen, "127.0.0.1:") || strings.HasPrefix(p.listen, "localhost:") {
		line("ok", "panel", "http://"+p.listen+" (this machine only; reach it through an SSH tunnel or a web server in front)")
	} else {
		line("warn", "panel", "http://"+p.listen+" is reachable over the network without HTTPS")
	}
	fmt.Printf("\n%d ok, %d notes, %d failed\n", ok, warn, bad)
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}
