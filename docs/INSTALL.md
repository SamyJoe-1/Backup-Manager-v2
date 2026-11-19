# Installing Backup Manager

Everything needed to go from a fresh server to a running panel. If something goes wrong, see
[Troubleshooting](#troubleshooting) at the end.

- [1. Before you start](#1-before-you-start)
- [2. Install](#2-install)
- [3. The questions it asks](#3-the-questions-it-asks)
- [4. All options](#4-all-options)
- [5. What it creates](#5-what-it-creates)
- [6. Opening the panel (HTTPS)](#6-opening-the-panel-https)
- [7. Access to databases on this server](#7-access-to-databases-on-this-server)
- [8. Agents: backing up other servers](#8-agents-backing-up-other-servers)
- [9. Docker / container install](#9-docker--container-install)
- [10. After installing](#10-after-installing)
- [11. Upgrade](#11-upgrade)
- [12. Uninstall](#12-uninstall)
- [13. Troubleshooting](#troubleshooting)

---

## 1. Before you start

| Need | Detail |
|---|---|
| Linux with systemd | Debian 11+, Ubuntu 20.04+, RHEL / Rocky / AlmaLinux 8+ and close relatives. Tested: Debian 12, Ubuntu 22.04, Ubuntu 24.04, Rocky 9. Without systemd (Alpine, containers) use the [container](#9-docker--container-install). |
| CPU | x86-64 (amd64) or ARM64 |
| root | the installer creates a user, a service and files under `/etc` and `/var/lib` |
| Memory | 256 MB for the program. Building from source needs about 1 GB (the installer adds temporary swap on small servers). |
| Disk | room for local backups if you keep them on this server; S3 storage needs none |
| Network | outgoing HTTPS to github.com (download), and to your S3 storage if you use one |

Where to install it:

- **On the database server itself** — simplest; needed for physical backups of a MariaDB installed there.
- **On a separate server** — SQL, PostgreSQL and Redis backups work over the network. For physical backups of another
  server install an [agent](#8-agents-backing-up-other-servers) there.

---

## 2. Install

Pick the way that matches how you reach this repository.

### Public repository

```bash
curl -fsSL https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo bash
```

### Private repository

The installer and the program files are behind your GitHub login, so give it a token:

1. GitHub → *Settings → Developer settings → Personal access tokens → Fine-grained tokens → Generate new token*.
   Repository access: **only this repository**. Permissions: **Contents: Read-only** (that includes releases).
2. On the server:

```bash
export GITHUB_TOKEN=github_pat_xxxxxxxx
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo -E bash
```

`sudo -E` passes the token to the installer. The token is used only during the install and is **not stored**.

### From a copy of the repository

```bash
git clone https://github.com/SamyJoe-1/Backup-Manager-v2.git      # asks for your login if private
cd Backup-Manager-v2
sudo ./install.sh
```

From a copy, the installer first tries the release download; when that is not possible (private repository without
`GITHUB_TOKEN`, no internet to GitHub, no release yet) it **builds the program from the copy** instead: it fetches
the Go compiler from go.dev (checksum verified) and compiles. That takes 2–6 minutes on a small server.

### Without questions (scripts, cloud-init)

Add `--yes` and the options you want:

```bash
curl -fsSL https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh \
  | sudo bash -s -- --yes --domain backups.example.com --email you@example.com
```

When there is no terminal to ask on and `--yes` is missing, the installer stops without changing anything.

### Safe to run twice

Running the installer again on a server that already has it **upgrades** the program and keeps every setting,
account and backup. A download that is cut off half way runs nothing (the script is one function called on its last
line). `--dry-run` shows every step without doing it.

---

## 3. The questions it asks

It first shows what it found on the server (databases installed directly, database containers, what uses ports 80 and
443, free disk, memory), then asks only what it cannot decide:

1. **How will you open the panel?**
   1. *With a domain name and HTTPS* — recommended. The domain must point to this server (an `A` record). See
      [section 6](#6-opening-the-panel-https) for what happens next.
   2. *Only from this machine, through an SSH tunnel* — the default; nothing is opened to the network.
   3. *http://server-ip:port without HTTPS* — works, but passwords travel unprotected. Avoid on the internet.
2. **Port of the panel on this machine** — default 8750. It refuses a port that is already used.
3. **Folder for the catalog and local backups** — default `/var/lib/backup-manager`. Put it on the disk with room.
4. **Run the service as root?** — default *no*: it runs as the locked-down user `backupmgr`. Say *yes* when you need:
   - physical backups of a MariaDB **installed directly on this server** (its data folder is readable only by root);
   - folder backups of files only root can read (`/root`, `/etc/shadow`, application `.env` files owned by root…).
5. **Let the service use Docker?** (only asked when Docker runs here) — needed for databases running in containers
   (physical backups, binary logs, PostgreSQL), for searching rows inside a backup and for restore tests. Access to
   Docker is as powerful as root; say no if you need none of these.
6. **Install mariadb-backup?** (only when a MariaDB runs directly here and the tool is missing) — needed for its
   physical backups. The package comes from your distribution, so it matches your server.
7. **Create a database user `bm_backup`?** (only when a local MariaDB/MySQL accepts root without a password, the
   default on Debian/Ubuntu) — creates the user with exactly the rights backups need, a random password, prints it,
   and saves it in `/etc/backup-manager/db-user.txt` (readable by root only).

Then it prints a summary of everything it will do and asks to continue.

---

## 4. All options

Give them after `bash -s --` when piping, or directly to `./install.sh`.

| Option | Meaning |
|---|---|
| `--yes` | no questions; defaults plus the options given |
| `--domain NAME` | serve the panel on `https://NAME`, certificate made automatically |
| `--email ADDRESS` | e-mail for certificate expiry notices (optional) |
| `--public` | open `http://<server IP>:PORT` without HTTPS |
| `--port N` | port of the panel on this machine (default 8750) |
| `--data-dir DIR` | catalog and local backups (default `/var/lib/backup-manager`) |
| `--run-as backupmgr\|root` | the account the service runs as (default `backupmgr`) |
| `--docker-access` / `--no-docker-access` | let the service use Docker, or never ask |
| `--install-tools` | install `mariadb-backup` when a MariaDB runs directly here |
| `--create-db-user` | create the `bm_backup` database user on the local MariaDB/MySQL |
| `--version vX.Y.Z` | install that release instead of the newest |
| `--from-source` | build here from source instead of downloading |
| `--binary FILE` | install a program file you already have |
| `--agent-server URL` `--agent-token T` | install as an agent of another panel ([section 8](#8-agents-backing-up-other-servers)) |
| `--agent-allow-http` | let an agent use a plain `http://` panel address (private networks only) |
| `--dry-run` | show what would happen, change nothing |
| `--uninstall` | remove it ([section 12](#12-uninstall)) |

Environment: `GITHUB_TOKEN` (private repository), `BM_REPO=owner/name` (install from a fork), `BM_BRANCH`,
`BM_BUILD_SWAP=no` (never add temporary swap when building).

Examples:

```bash
# panel on https with a MariaDB on this server, everything automatic
sudo ./install.sh --yes --domain backups.example.com --run-as root --install-tools --create-db-user

# only SQL backups of remote databases, panel reached through an SSH tunnel
sudo ./install.sh --yes --no-docker-access

# a server with databases in Docker
sudo ./install.sh --yes --domain backups.example.com --docker-access
```

---

## 5. What it creates

| Path | What |
|---|---|
| `/usr/local/bin/backup-manager` | the program (one file) |
| `/etc/backup-manager/env` | settings (owner-only). Change, then `systemctl restart backup-manager`. |
| `/etc/backup-manager/master.key` | **the encryption key. Copy it somewhere safe.** |
| `/etc/backup-manager/db-user.txt` | the database user's password, when it created one |
| `/var/lib/backup-manager/catalog.db` | the catalog: accounts, settings, plans, the list of backups |
| `/var/lib/backup-manager/catalog.backup.db` | a consistent copy of the catalog, refreshed every 6 hours |
| `/var/lib/backup-manager/backups/` | the default "this server" destination folder |
| `/etc/systemd/system/backup-manager.service` | the service: starts on boot, restarts on failure, runs at low CPU and disk priority |
| user `backupmgr` | system account without login (not created with `--run-as root`) |

Settings in `/etc/backup-manager/env`:

| Setting | Meaning |
|---|---|
| `BM_DATA_DIR` | catalog and local backups |
| `BM_MASTER_KEY_FILE` | the key |
| `BM_LISTEN` | where the panel listens, e.g. `127.0.0.1:8750` (this machine only) or `0.0.0.0:8750` |
| `BM_PUBLIC_URL` | the address people use; used in printed links |
| `BM_TLS_DOMAIN`, `BM_TLS_EMAIL` | built-in HTTPS with a Let's Encrypt certificate |
| `BM_TLS_CERT`, `BM_TLS_KEY` | built-in HTTPS with your own certificate files |
| `BM_HTTPS_LISTEN`, `BM_HTTP_LISTEN` | ports of the built-in HTTPS (default `:443`) and its redirect / certificate check (`:80`, `off` to disable with own certificates) |
| `BM_SANDBOX_IMAGE`, `BM_SANDBOX_MEMORY` | image and memory limit of the row-search sandbox (default: the MariaDB version of the backup, `512m`) |
| `BM_REPO` | where `backup-manager upgrade` looks for releases |
| `BM_AGENT_SERVER`, `BM_AGENT_TOKEN` | agent mode |

---

## 6. Opening the panel (HTTPS)

### Domain name, nothing else on ports 80/443 (built-in HTTPS)

With `--domain` (or answer 1), when ports 80 and 443 are free, the program serves HTTPS itself and gets a free
certificate from Let's Encrypt on the first visit, then renews it by itself. Requirements:

- the domain's `A` record points to this server;
- ports 80 and 443 are open in any cloud firewall (the installer opens them in `ufw` / `firewalld` if those are on).

Port 80 answers the certificate check and sends everyone else to `https://`. The first page can take a few seconds
while the certificate is fetched.

### A server that already runs nginx

When nginx already uses port 80 or 443, the installer adds a site for your domain that forwards to the panel
(`/etc/nginx/sites-available/backup-manager.conf` or `/etc/nginx/conf.d/backup-manager.conf`), checks it with
`nginx -t`, and runs `certbot --nginx` to get the certificate (offering to install certbot). If the domain does not
point here yet, it says so; run later:

```bash
certbot --nginx -d backups.example.com
```

### Another web server (Apache, Caddy, Traefik…)

The installer says so and leaves the panel on `127.0.0.1:8750`. Point your web server at it. The panel needs the
`Host` and `X-Forwarded-Proto` headers and long-lived connections (live progress, large downloads):

**Caddy**

```
backups.example.com {
    reverse_proxy 127.0.0.1:8750 {
        flush_interval -1
    }
}
```

**Apache** (`a2enmod proxy proxy_http headers ssl`)

```apache
<VirtualHost *:443>
    ServerName backups.example.com
    # SSLEngine on + your certificate lines here
    ProxyPreserveHost On
    RequestHeader set X-Forwarded-Proto "https"
    ProxyPass        / http://127.0.0.1:8750/ flushpackets=on timeout=3600
    ProxyPassReverse / http://127.0.0.1:8750/
</VirtualHost>
```

**nginx by hand**: copy the block the installer writes (shown in `install.sh`, function `setup_nginx`).

### Your own certificate files

```bash
cat >>/etc/backup-manager/env <<'EOF'
BM_TLS_CERT=/etc/backup-manager/fullchain.pem
BM_TLS_KEY=/etc/backup-manager/privkey.pem
EOF
chown backupmgr /etc/backup-manager/*.pem      # skip when running as root
systemctl restart backup-manager
```

The service user needs permission to bind ports below 1024: the installer adds it when it set up built-in HTTPS; for
your own certificate on 443 add `AmbientCapabilities=CAP_NET_BIND_SERVICE` to the `[Service]` part of the unit (or
use `BM_HTTPS_LISTEN=:8443`).

### No domain: SSH tunnel

The default. Nothing is opened. From your own computer:

```bash
ssh -L 8750:127.0.0.1:8750 root@your-server
```

Keep that window open and browse to `http://127.0.0.1:8750`.

---

## 7. Access to databases on this server

How each kind of target is reached, and what the service needs:

| Target | Runs where | Needs |
|---|---|---|
| MySQL/MariaDB, SQL backups | over the network (host/port or socket) | a database user ([rights](USER-GUIDE.md#database-users-and-rights)) |
| MariaDB physical / binary logs, installed directly | on this server ("Run the backup tools in": `local`) | `--run-as root`, `mariadb-backup` installed |
| MariaDB physical / binary logs, in Docker | inside its container (`docker:<container>`) | Docker access |
| PostgreSQL | where `pg_dump` is (`local` or `docker:<container>`) | `pg_dump`/`psql` there; Docker access for a container |
| Redis | over the network | its password, if set |
| Folders | on this server | read permission (root for root-only files) |
| Row search, restore tests | a throwaway container | Docker access |

Changing your mind later is fine: run the installer again with `--run-as root` or `--docker-access`, or by hand:

```bash
usermod -aG docker backupmgr && systemctl restart backup-manager      # Docker access
```

`backup-manager doctor` shows what the service can use right now (the installer runs it at the end):

```bash
sudo -u backupmgr env $(cat /etc/backup-manager/env | grep -v '^#' | xargs) backup-manager doctor
```

---

## 8. Agents: backing up other servers

One panel, many servers. An agent is the same program in agent mode on another server: it connects **out** to the
panel (no port opened on that server), takes the backups there, sends them straight to the destination, and reports
to the panel.

1. In the panel: **Users & access → Agents → New agent**. It shows a token once.
2. On the other server:

```bash
curl -fsSL https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh \
  | sudo bash -s -- --agent-server https://backups.example.com --agent-token bma_xxxxxxxx
```

   (private repository: add the `GITHUB_TOKEN` header and `sudo -E` as in [section 2](#private-repository)). The
   same questions about root and Docker access are asked, for the same reasons.
3. In the panel the agent turns **online**. Add that server's databases under **Databases & folders** and choose the
   agent in **Runs on**.

Use an **S3 destination** for agent backups: a "folder on this server" destination would be a folder on the agent,
which the panel cannot read, verify or delete. The panel must be on HTTPS, because database passwords travel to the
agent (`--agent-allow-http` exists for private networks only). What runs on the agent: SQL, physical, PostgreSQL,
Redis and folder backups. What does not: binary-log archiving, restores, the sandbox, connection tests.

---

## 9. Docker / container install

For servers without systemd, or if you prefer containers. Build the image from this repository:

```bash
git clone https://github.com/SamyJoe-1/Backup-Manager-v2.git && cd Backup-Manager-v2
mkdir -p bin && curl -fsSL -o bin/backup-manager \
  https://github.com/SamyJoe-1/Backup-Manager-v2/releases/latest/download/backup-manager-linux-amd64 \
  && chmod +x bin/backup-manager                   # or: scripts/build.sh (needs Go)
docker compose up -d
docker compose exec backup-manager backup-manager setup-link
```

The panel is on `127.0.0.1:8750` of the host. Data, key and catalog are in the `bm-data` volume — back up that volume
too. For physical backups of other containers, the sandbox and restore tests, uncomment the Docker socket line in
`docker-compose.yml` (it gives the container control of Docker on the host).

---

## 10. After installing

1. Open the link the installer printed and create your account (the first one is the owner). Lost the link?
   `sudo -u backupmgr env $(grep -v '^#' /etc/backup-manager/env | xargs) backup-manager setup-link`
   (as root: `env $(grep -v '^#' /etc/backup-manager/env | xargs) backup-manager setup-link`).
2. **Copy `/etc/backup-manager/master.key` off the server** (password manager, a second server). It is a 64-character
   line.
3. Turn on two-factor sign-in: **Users & access → My account**.
4. Follow [USER-GUIDE.md](USER-GUIDE.md): add a database, a destination, a plan; run it; turn on a restore test.
5. Protect the manager itself: [RESTORE.md — when the backup server is lost](RESTORE.md#when-the-backup-manager-server-is-lost).

---

## 11. Upgrade

Run the installer again, the same way you installed. It keeps all settings, accounts and backups, and restarts the
service. Or, for a public repository:

```bash
sudo backup-manager upgrade --check      # is there a newer release?
sudo backup-manager upgrade              # verified download; the old program is kept as backup-manager.old
sudo systemctl restart backup-manager
```

Running jobs are interrupted by the restart and start again by themselves. To avoid that, wait until **Jobs** is
empty, or `backup-manager hold` first and `backup-manager release` after.

Going back: `mv /usr/local/bin/backup-manager.old /usr/local/bin/backup-manager && systemctl restart backup-manager`.

---

## 12. Uninstall

```bash
sudo ./install.sh --uninstall       # or: curl …/install.sh | sudo bash -s -- --uninstall
```

It stops and removes the service, the program and the nginx site it added. It **keeps** `/etc/backup-manager` (the
key) and the data folder, and asks (twice) before deleting them. Backups stored on S3 are never touched. Installing
again later on the kept folders brings everything back. The `bm_backup` database user, if one was created, stays in
your database (`DROP USER 'bm_backup'@'localhost', 'bm_backup'@'127.0.0.1';`).

---

## Troubleshooting

Start with:

```bash
systemctl status backup-manager
journalctl -u backup-manager -n 100 --no-pager
backup-manager doctor             # run it as the service user, see section 7
```

| Symptom | Cause / fix |
|---|---|
| `systemd is not running on this machine` | container or no systemd: use the [container install](#9-docker--container-install) |
| `run it as root` | add `sudo` before `bash` (and `sudo -E` for `GITHUB_TOKEN`) |
| `no terminal to ask on` | piped without a terminal (cloud-init, CI): add `--yes` and options |
| `download failed` | private repository: set `GITHUB_TOKEN` and use `sudo -E`; no release yet: it offers to build from source; firewall blocks github.com |
| `checksum mismatch` | the download was damaged or changed: nothing was installed; run again |
| build runs out of memory | small server with `BM_BUILD_SWAP=no`: allow swap, or build on another machine and use `--binary` |
| `port … is already used` | choose another with `--port` |
| the service does not start | the installer prints the last log lines; often a port taken by something else (`ss -ltnp`) |
| HTTPS certificate not issued | the domain does not point here yet, or ports 80/443 are closed in the cloud firewall; it retries on the next visit |
| `ports 80/443 are used by …` | another web server: use it in front ([section 6](#another-web-server-apache-caddy-traefik)) |
| setup link says the code is wrong | a link works once; make a new one (section 10) |
| forgot the password | `backup-manager admin reset --email you@example.com` as the service user (signs everyone out) |
| `permission denied` in a folder backup | the service user cannot read it: `--run-as root`, or give read access |
| `docker … permission denied` | no Docker access: section 7 |
| `mariabackup not found` | install `mariadb-backup`, or set "Run the backup tools in" to the database's container |
| `this replica is … seconds behind` | the replica is lagging; it waits and retries. Raise "Replica may be behind by at most" on the database |
| agent stays offline | `journalctl -u backup-manager` on the agent: wrong address, token, or the panel is not reachable from there |
| backups fail with `destination has … free` | the destination folder is short of space: free space or use S3 |
