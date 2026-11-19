package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"backupmanager/internal/api"
)

// The client commands talk to a running manager over its API with a token:
//   BM_URL (default http://127.0.0.1:8750) and BM_TOKEN.

func call(method, path string, body []byte) ([]byte, error) {
	base := strings.TrimRight(env("BM_URL", "http://"+env("BM_LISTEN", "127.0.0.1:8750")), "/")
	tok := os.Getenv("BM_TOKEN")
	if tok == "" {
		return nil, errors.New("set BM_TOKEN (create one with: backup-manager token create --name cli --role operator)")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasPrefix(path, "/api/") && path != "/metrics" {
		path = "/api" + path
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the manager at %s: %w", base, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("the manager answered %s", resp.Status)
	}
	return out, nil
}

func get(path string, v any) error {
	out, err := call("GET", path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

func apiCmd(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: backup-manager api METHOD PATH [JSON]   e.g. api GET /backups?status=failed")
	}
	var body []byte
	if len(args) > 2 {
		body = []byte(args[2])
	}
	out, err := call(strings.ToUpper(args[0]), args[1], body)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	fmt.Println(strings.TrimRight(string(out), "\n"))
	return nil
}

func when(t int64) string {
	if t == 0 {
		return "-"
	}
	return time.Unix(t, 0).Format("2006-01-02 15:04")
}

func size(n int64) string {
	u := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(u)-1 {
		f, i = f/1024, i+1
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + u[i]
}

type planRow struct {
	ID        int64
	Name      string
	Schedule  string
	Enabled   bool
	NextRunAt int64
	Spec      struct{ Method string }
}

func plansCmd() error {
	var plans []planRow
	if err := get("/plans", &plans); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPLAN\tMETHOD\tSCHEDULE\tNEXT RUN")
	for _, p := range plans {
		sch := p.Schedule
		if sch == "" {
			sch = "manual"
		}
		if !p.Enabled {
			sch += " (off)"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Spec.Method, sch, when(p.NextRunAt))
	}
	return w.Flush()
}

type backupRow struct {
	ID, PlanName, TargetName, Status, Method, Error string
	CreatedAt, StoredBytes, StartedAt, FinishedAt   int64
}

// runCmd starts a plan (by id or exact name) and, with --wait, follows it to the end. The exit
// code tells a script whether the backup succeeded.
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	wait := fs.Bool("wait", false, "wait until the backup finishes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: backup-manager run [--wait] <plan id or name>")
	}
	var plans []planRow
	if err := get("/plans", &plans); err != nil {
		return err
	}
	var id int64
	for _, p := range plans {
		if strconv.FormatInt(p.ID, 10) == fs.Arg(0) || p.Name == fs.Arg(0) {
			id = p.ID
		}
	}
	if id == 0 {
		return fmt.Errorf("no plan with id or name %q", fs.Arg(0))
	}
	out, err := call("POST", fmt.Sprintf("/plans/%d/run", id), []byte("{}"))
	if err != nil {
		return err
	}
	var b backupRow
	json.Unmarshal(out, &b)
	if b.ID == "" {
		fmt.Println("queued")
		return nil
	}
	fmt.Println("backup", b.ID, "queued")
	if !*wait {
		return nil
	}
	for {
		time.Sleep(2 * time.Second)
		var d struct{ Backup backupRow }
		if err := get("/backups/"+b.ID, &d); err != nil {
			return err
		}
		switch d.Backup.Status {
		case "done":
			fmt.Printf("done: %s stored in %ds\n", size(d.Backup.StoredBytes), d.Backup.FinishedAt-d.Backup.StartedAt)
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("backup %s: %s", d.Backup.Status, d.Backup.Error)
		}
	}
}

func backupsCmd() error {
	var d struct {
		Items []backupRow
		Total int
	}
	if err := get("/backups?pageSize=20", &d); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "BACKUP\tPLAN\tTARGET\tMETHOD\tSTATUS\tSIZE\tTAKEN")
	for _, b := range d.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.ID, b.PlanName, b.TargetName, b.Method, b.Status, size(b.StoredBytes), when(b.CreatedAt))
	}
	w.Flush()
	fmt.Printf("%d of %d\n", len(d.Items), d.Total)
	return nil
}

func jobsCmd() error {
	var d struct {
		Items []struct {
			ID                  int64
			Title, State, Error string
			CreatedAt           int64
		}
	}
	if err := get("/jobs?pageSize=20", &d); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tSTATE\tWHAT\tSTARTED\tERROR")
	for _, j := range d.Items {
		e := j.Error
		if len(e) > 70 {
			e = e[:70] + "…"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", j.ID, j.State, j.Title, when(j.CreatedAt), e)
	}
	return w.Flush()
}

func tokenCmd(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: backup-manager token create --name NAME --role owner|operator|viewer")
	}
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	name := fs.String("name", "", "what the token is for")
	role := fs.String("role", "operator", "owner, operator or viewer")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	app, err := open(loadPaths())
	if err != nil {
		return err
	}
	tok, err := api.NewToken(app.Store, *name, *role)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	fmt.Fprintln(os.Stderr, "Shown once. Use it as BM_TOKEN, or in the header  Authorization: Bearer <token>")
	return nil
}

// upgradeCmd replaces this program with the newest release after checking its checksum. The old
// file is kept as <name>.old so a bad release can be rolled back by hand.
func upgradeCmd(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "only tell whether a newer version exists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	base := os.Getenv("BM_RELEASE_BASE")
	if base == "" {
		repo := env("BM_REPO", "SamyJoe-1/Backup-Manager-v2")
		base = "https://github.com/" + repo + "/releases/latest/download"
	}
	fetch := func(name string) ([]byte, error) {
		resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(strings.TrimRight(base, "/") + "/" + name)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: %s", name, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	file := "backup-manager-linux-" + runtime.GOARCH
	sums, err := fetch("SHA256SUMS")
	if err != nil {
		return fmt.Errorf("%w — if the repository is private, upgrade by running the installer again with GITHUB_TOKEN set (docs/INSTALL.md, 'Upgrade')", err)
	}
	want, latest := "", ""
	for _, l := range strings.Split(string(sums), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			want = f[0]
		}
	}
	if v, err := fetch("VERSION"); err == nil {
		latest = strings.TrimSpace(string(v))
	}
	if want == "" {
		return fmt.Errorf("the release has no checksum for %s", file)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	have := sha256.Sum256(cur)
	if hex.EncodeToString(have[:]) == want {
		fmt.Println("already up to date:", version)
		return nil
	}
	if *check {
		fmt.Printf("a different release is available (%s); this is %s\n", latest, version)
		return nil
	}
	bin, err := fetch(file)
	if err != nil {
		return err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return errors.New("checksum mismatch: the download is damaged or was tampered with; nothing was changed")
	}
	if err := os.WriteFile(exe+".new", bin, 0o755); err != nil {
		return err
	}
	os.Remove(exe + ".old")
	if err := os.Link(exe, exe+".old"); err != nil {
		os.Remove(exe + ".new")
		return fmt.Errorf("could not keep the old version: %w", err)
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		return err
	}
	fmt.Printf("upgraded %s -> %s. The old program is %s.old. Restart the service: systemctl restart backup-manager\n", version, latest, exe)
	return nil
}
