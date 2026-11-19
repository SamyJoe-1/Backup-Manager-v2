// backup-manager: one binary holding the API, the web panel, the queue worker and the scheduler.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"backupmanager/internal/api"
	"backupmanager/internal/core"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type paths struct{ data, key, listen string }

func loadPaths() paths {
	return paths{
		data:   env("BM_DATA_DIR", "/var/lib/backup-manager"),
		key:    env("BM_MASTER_KEY_FILE", "/etc/backup-manager/master.key"),
		listen: env("BM_LISTEN", "127.0.0.1:8750"),
	}
}

func open(p paths) (*core.App, error) {
	if err := os.MkdirAll(p.data, 0o700); err != nil {
		return nil, err
	}
	keys, created, err := core.LoadOrCreateMaster(p.key)
	if err != nil {
		return nil, err
	}
	if created {
		log.Printf("created a new master key at %s. Keep a copy somewhere safe: without it encrypted backups cannot be read.", p.key)
	}
	st, err := core.OpenStore(filepath.Join(p.data, "catalog.db"))
	if err != nil {
		return nil, err
	}
	hub := core.NewHub()
	app := &core.App{Store: st, Keys: keys, Hub: hub}
	app.Queue = core.NewQueue(st, hub, 2)
	app.Queue.Handle("backup", app.RunBackup)
	app.Queue.Handle("verify", app.RunVerify)
	app.Queue.Handle("binlog", app.RunBinlog)
	app.Queue.Handle("restore", app.RunRestore)
	app.Queue.Handle("explore", app.RunExplore)
	app.Queue.Handle("restore-test", app.RunRestoreTest)
	app.Queue.Handle("table-restore", app.RunTableRestore)
	return app, nil
}

func main() {
	core.Version = version
	syscall.Umask(0o077) // the catalog, the key and local backups are for the service user only
	log.SetFlags(log.LstdFlags)
	cmd := "help"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "setup-link":
		err = setupLink()
	case "admin":
		err = admin(os.Args[2:])
	case "api":
		err = apiCmd(os.Args[2:])
	case "plans":
		err = plansCmd()
	case "run":
		err = runCmd(os.Args[2:])
	case "backups":
		err = backupsCmd()
	case "jobs":
		err = jobsCmd()
	case "token":
		err = tokenCmd(os.Args[2:])
	case "upgrade":
		err = upgradeCmd(os.Args[2:])
	case "agent":
		err = agentCmd(os.Args[2:])
	case "healthcheck": // used by the container image; needs no shell or curl inside it
		resp, herr := http.Get("http://" + strings.Replace(loadPaths().listen, "0.0.0.0", "127.0.0.1", 1) + "/healthz")
		if herr != nil {
			err = herr
		} else if resp.StatusCode != 200 {
			err = fmt.Errorf("unhealthy: %s", resp.Status)
		}
	case "hold", "release":
		err = hold(cmd == "hold")
	case "catalog-backup":
		err = catalogBackup(os.Args[2:])
	case "decrypt":
		err = decrypt(os.Args[2:])
	case "doctor":
		err = doctor()
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		fmt.Print(`backup-manager — backups for databases and folders

  backup-manager serve                 run the panel, the queue and the scheduler
  backup-manager setup-link            print a one-time link to create the first account
  backup-manager admin create|reset --email E   create an account or reset its password
  backup-manager hold | release        pause / resume starting new jobs (use around deploys)
  backup-manager token create --name N --role owner|operator|viewer   make an API token (shown once)
  backup-manager upgrade [--check]     replace this program with the newest release (checksum verified)
  backup-manager agent --server URL --token T   run as an agent of a panel on another server
  backup-manager catalog-backup FILE   consistent copy of the catalog (also made every 6 h as catalog.backup.db)
  backup-manager decrypt --in FILE     open a stored backup file with only the master key (server lost)
  backup-manager doctor               check this machine: tools, Docker access, disk, ports, settings
  backup-manager version

 With BM_TOKEN set (and BM_URL when the panel is not on this machine):
  backup-manager plans | backups | jobs          list
  backup-manager run [--wait] <plan>             start a plan; --wait exits non-zero when it fails
  backup-manager api METHOD PATH [JSON]          anything the panel can do, e.g. api GET /backups?status=failed

Environment: BM_DATA_DIR (/var/lib/backup-manager), BM_MASTER_KEY_FILE (/etc/backup-manager/master.key),
             BM_LISTEN (127.0.0.1:8750), BM_PUBLIC_URL, BM_TLS_DOMAIN + BM_TLS_EMAIL (built-in HTTPS),
             BM_TLS_CERT + BM_TLS_KEY (own certificate), BM_HTTPS_LISTEN (:443), BM_HTTP_LISTEN (:80)
`)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve() error {
	p := loadPaths()
	app, err := open(p)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app.CatalogCopy = filepath.Join(p.data, "catalog.backup.db")
	app.Queue.Recover()
	go app.Queue.Run(ctx)
	go app.RunScheduler(ctx)

	handler := (&api.Server{App: app}).Handler()
	srv := &http.Server{Addr: p.listen, Handler: handler, ReadHeaderTimeout: 15 * time.Second}
	tlsSrv, err := setupTLS(p.data, handler)
	if err != nil {
		return err
	}
	errs := make(chan error, 3)
	if tlsSrv != nil {
		tlsSrv.run(ctx, errs)
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	go func() {
		log.Printf("backup-manager %s listening on http://%s (data: %s)", version, p.listen, p.data)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	select {
	case err := <-errs:
		stop()
		return err
	case <-ctx.Done():
		time.Sleep(300 * time.Millisecond)
		return nil
	}
}

func publicURL(p paths) string {
	if u := os.Getenv("BM_PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://" + p.listen
}

func setupLink() error {
	p := loadPaths()
	app, err := open(p)
	if err != nil {
		return err
	}
	var n int
	app.Store.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return errors.New("an account already exists. Use `backup-manager admin reset --email …` to reset a password")
	}
	t, err := api.NewSetupToken(app.Store)
	if err != nil {
		return err
	}
	fmt.Printf("%s/#setup=%s\n", publicURL(p), t)
	return nil
}

func admin(args []string) error {
	if len(args) == 0 || (args[0] != "create" && args[0] != "reset") {
		return errors.New("usage: backup-manager admin create|reset --email you@example.com")
	}
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	email := fs.String("email", "", "account e-mail")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	app, err := open(loadPaths())
	if err != nil {
		return err
	}
	pw := os.Getenv("BM_PASSWORD")
	if pw == "" {
		fmt.Fprint(os.Stderr, "New password (at least 10 characters): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	if args[0] == "reset" {
		res, _ := app.Store.DB.Exec(`DELETE FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(*email)))
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no account with e-mail %s", *email)
		}
		app.Store.DB.Exec(`DELETE FROM sessions`)
	}
	if _, err := api.CreateUser(app.Store, *email, pw); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// hold flips the flag in the catalog; a running server picks it up within a few seconds.
func hold(on bool) error {
	app, err := open(loadPaths())
	if err != nil {
		return err
	}
	v := ""
	if on {
		v = "1"
	}
	if err := app.Store.SetSetting("hold", v); err != nil {
		return err
	}
	fmt.Println(map[bool]string{true: "holding: no new jobs will start", false: "released"}[on])
	return nil
}
