# Using Backup Manager

From the first sign-in to a protected server. Every screen named here is in the left menu of the panel.

- [First steps (10 minutes)](#first-steps-10-minutes)
- [Databases & folders](#databases--folders)
  - [Database users and rights](#database-users-and-rights)
  - [Databases in Docker](#databases-in-docker)
- [Destinations](#destinations)
- [Plans](#plans)
  - [Choosing a method](#choosing-a-method)
  - [Schedules](#schedules)
  - [Keeping and deleting backups](#keeping-and-deleting-backups)
  - [Hooks](#hooks)
- [A good setup for a busy production database](#a-good-setup-for-a-busy-production-database)
- [Backups and jobs](#backups-and-jobs)
- [Looking inside a backup](#looking-inside-a-backup)
- [Proving backups: verify and restore tests](#proving-backups-verify-and-restore-tests)
- [Sets: several servers at one moment](#sets-several-servers-at-one-moment)
- [Deploys and migrations](#deploys-and-migrations)
- [Alerts](#alerts)
- [Users, roles, two-factor, tokens](#users-roles-two-factor-tokens)
- [Language](#language)

Getting data back is in [RESTORE.md](RESTORE.md).

---

## First steps (10 minutes)

1. **Create your account** with the link the installer printed. The first account is the *owner*.
2. **Databases & folders → Add.** Choose the kind, fill in the address and the user ([which user?](#database-users-and-rights)).
   Click **Test**: it shows the server version and the number of tables, or the exact error.
3. **Destinations → Add.** For a first try, *Folder on this server* with the suggested path. For real use add an
   [S3 destination](#destinations) too: a backup on the same server is gone when the server is.
4. **Plans → New plan.** Name it, choose the database and the destination, keep *Logical (SQL)* and *Whole server*,
   schedule *Every 24 hours*, keep 14 days and the newest 3. Save.
5. **Run now.** The **Backups** page shows it live: table by table, rows, speed.
6. Click the finished backup: tables, rows and sizes. **Test restore** loads it into a throwaway server and checks
   every table — the only real proof a backup works.
7. **Settings**: add a Telegram chat or a webhook, so a failure at night reaches you.
8. **Copy `/etc/backup-manager/master.key` off the server** if you have not yet.

---

## Databases & folders

The things to back up. Kinds:

| Kind | Fields |
|---|---|
| **MySQL / MariaDB** | Host + Port, or a Unix socket; User; Password; TLS. **Run the backup tools in**: empty for SQL backups only; `local` when the database is installed on this server; `docker:<container>` when it runs in a container. **Replica may be behind by at most (seconds)**: physical backups from a replica wait while it lags more (default 300). |
| **PostgreSQL** | Host + Port, User, Password, and **Run the backup tools in** (required): `local` (needs `pg_dump`/`psql` here) or `docker:<container>`. |
| **Redis** | Host, Port, optional User (ACL), Password. |
| **Folders** | Paths, one per line (absolute). **Leave out**: names (`node_modules`), patterns (`*.log`) or full paths (`/srv/app/cache`). |

With agents, **Runs on** chooses the server the backup runs on ([INSTALL.md, agents](INSTALL.md#8-agents-backing-up-other-servers)).

Passwords are stored encrypted with the master key and never shown again. Leave the password empty when editing to
keep the stored one.

**Point at a replica when you have one.** Backups read a lot; on a replica the primary feels nothing. Never use a
reporting / aggregation copy whose tables were changed: restore what you backed up from the real primary or its
replica.

### Database users and rights

The installer can create the MariaDB/MySQL user for you (`--create-db-user`). By hand, create **one user with only
these rights**:

**MariaDB / MySQL** — SQL backups only (read-only):

```sql
CREATE USER 'bm_backup'@'%' IDENTIFIED BY 'a-long-random-password';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON *.* TO 'bm_backup'@'%';
```

Add for **physical backups** (MariaDB) and **binary-log archiving**:

```sql
GRANT PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'bm_backup'@'%';
GRANT SLAVE MONITOR ON *.* TO 'bm_backup'@'%';      -- MariaDB 10.5.9+, when backing up a replica
```

Use `'bm_backup'@'localhost'` (and `@'127.0.0.1'`) instead of `'%'` when the manager runs on the database server.

**Binary logs must be on** for binary-log archiving and point-in-time restore: in the server's configuration
`log_bin = binlog`, `binlog_format = ROW`, a unique `server_id`, and `expire_logs_days` long enough to cover the time
between two archive runs (1–3 days is plenty). Replicas often run without binary logs: archive them from the primary.

**PostgreSQL** — simplest: the `postgres` user. A restricted alternative (PostgreSQL 14+):

```sql
CREATE ROLE bm_backup LOGIN PASSWORD 'a-long-random-password';
GRANT pg_read_all_data TO bm_backup;
```

With the restricted role the roles file (`_globals.sql`) cannot read passwords; the backup is still made and notes it.

**Redis** — the password from `requirepass`, or a restricted ACL user (Redis 6+):

```
ACL SETUSER bm_backup on >a-long-random-password +info +sync +ping
```

### Databases in Docker

- **Host**: the address the manager reaches the database on: `127.0.0.1` + the published port, or the container's own
  address (`docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' <container>`).
- **Run the backup tools in**: `docker:<container name>`. Physical backups run `mariabackup` inside that container
  (the official `mariadb` image has it), so the versions always match. PostgreSQL runs its own `pg_dump`.
- The service needs Docker access ([INSTALL.md section 7](INSTALL.md#7-access-to-databases-on-this-server)).
- The database user must be allowed from the manager's address (`'%'` or the Docker network) and, for the tools that
  run inside the container, from `localhost`.

---

## Destinations

Where backup files go. Every file is compressed (zstd) and, unless a plan says otherwise, encrypted (AES-256-GCM)
before it leaves the server; the destination never sees your data.

**Folder on this server** — an absolute path. Fast, but lost with the server: keep a second copy elsewhere.

**S3-compatible storage**:

| Provider | Endpoint | Region | Use HTTPS |
|---|---|---|---|
| AWS S3 | `s3.eu-central-1.amazonaws.com` | `eu-central-1` | on |
| Cloudflare R2 | `<account id>.r2.cloudflarestorage.com` | `auto` | on |
| Backblaze B2 | `s3.eu-central-003.backblazeb2.com` | `eu-central-003` | on |
| Wasabi | `s3.eu-central-1.wasabisys.com` | `eu-central-1` | on |
| DigitalOcean Spaces | `fra1.digitaloceanspaces.com` | `fra1` | on |
| MinIO (your own) | `minio.example.com:9000` | empty | as configured |

Create the bucket first. *Folder inside the bucket* keeps several servers apart in one bucket. The key needs: list,
read, write and delete objects in that bucket. **Test** writes and removes a small file.

Recommended: turn on the provider's **object lock / versioning** with a few days' retention. Then even someone with
the manager's passwords cannot destroy the backups.

---

## Plans

A plan says: what, how, how often, where to, and how long to keep.

### Choosing a method

For MySQL / MariaDB the plan form offers three methods. (PostgreSQL, Redis and folders have one method each.)

| Method | What it makes | Use it for |
|---|---|---|
| **Logical (SQL)** | one SQL file per table, from one consistent moment, no locks | small and medium databases; chosen databases or tables; schema only; rows only; anything you want to read or load elsewhere |
| **Physical (hot copy)** | a copy of the data files of the whole server while it runs (MariaDB), plus **incrementals** that copy only changed pages | big databases (tens of GB): much faster to take and to restore |
| **Binary logs** | archives the server's binary logs on every run | together with physical backups: restore to any second |

Logical options: **What to back up** (whole server / chosen databases / chosen tables — the picker lists them with
sizes); **Contents** (schema + rows / rows only / schema only). Through the [API](API.md) also: `exclude` patterns and
a row filter per table (`where`).

Physical option: **Incrementals between two full backups**. With 6 and a schedule of every 4 hours, a full backup is
taken once a day and 6 incrementals in between. Restoring an incremental applies its full backup and every
incremental before it, automatically.

Common options: **Speed limit (MB per second)** caps how fast the backup reads and sends; **Encrypt** (on by
default); **Test by restoring, every N good backups**; **Schedule is on**.

### Schedules

- **Every X hours**: `@every 6h`.
- **Cron line**: `0 3 * * *` (03:00 daily), `30 */4 * * *` (every 4 hours at :30), `0 2 * * 0` (Sundays 02:00).
  Server time zone, or set one: `CRON_TZ=Europe/Berlin 0 3 * * *`.
- **Manual only**.

Runs more often than every 5 minutes are refused. A run that is still going when the next is due is skipped, not
stacked. Missed runs (server was off) start once when it is back.

### Keeping and deleting backups

- **Delete backups older than (days)** and **Always keep the newest**. With both set, a backup goes only when it is
  older than the age *and* beyond the count. 0 = rule off.
- The newest good backup of a plan is **never** deleted by the rules. **Pinned** backups are never deleted
  automatically and cannot be deleted by hand until unpinned.
- A full physical backup and its incrementals are kept or deleted **together** (an incremental is useless alone);
  a full that newer incrementals need cannot be deleted by hand.
- Failed and cancelled attempts and deleted backups leave the list after 90 days. Every deletion and its reason are in
  **Activity**.

### Hooks

**Run before the backup** and **Run after the backup** are shell commands run on the machine doing the backup, as the
service user, with these variables:

| Variable | Value |
|---|---|
| `BM_BACKUP_ID`, `BM_PLAN`, `BM_TARGET`, `BM_METHOD` | what is being backed up |
| `BM_STATUS` | `starting` (before), `done` or `failed` (after) |
| `BM_ERROR` | the error, after a failed backup |

A failing *before* hook stops the backup (its message is shown). The *after* hook always runs, and its failure is
only noted on the backup. Each hook may run 15 minutes.

---

## A good setup for a busy production database

For a MariaDB of tens of GB with constant traffic, cron jobs and a Redis next to it:

| Plan | Target | Method | Schedule | Keep |
|---|---|---|---|---|
| Data | the **replica** (`docker:<replica>` or `local`) | Physical, 27 incrementals per full | every 6 h | 21 days |
| Binary logs | the **primary** | Binary logs | every 10 min | 7 days |
| Money / critical tables | the replica | Logical, chosen tables | hourly | 7 days |
| Uploads + config | folders: uploads, `.env` files, web server config | Folders | daily | 30 days |
| Redis | Redis | snapshot | every 30 min | 2 days |
| Backup Manager's own catalog | folder `/var/lib/backup-manager/catalog.backup.db` | Folders | daily | 30 days |

Plus: S3 destination with object lock; a restore test every 7 backups on the data plan; Telegram alerts.

Why this does not slow production: [HOW-IT-WORKS.md](HOW-IT-WORKS.md#taking-a-backup-at-peak-time).

---

## Backups and jobs

**Backups**: every backup, newest first, 15 per page. Search by id, plan, database or note; filter by status, plan,
database and dates; sort by date, size, rows or duration. Badges: *verified*, *restore tested*, 🔒 encrypted, 📌 pinned.

**Jobs**: the queue — backups, verifications, restores, restore tests, sandbox loads. Jobs keep running when you close
the browser and continue after the service restarts. **Cancel** stops the job and the tool it started (also inside
a container or on an agent) and removes partial files. A server has one backup at a time; others wait their turn.
A failed attempt is retried with growing pauses; a setting error is not retried and says what to fix.

**Dashboard**: good backups, last good backup, next run, failures in the last 7 days. **Pause new jobs** holds the
queue (running jobs finish).

---

## Looking inside a backup

Click a backup:

- **SQL backups**: every table with rows and sizes. **Structure** shows its `CREATE TABLE`. **Download** all, one
  database (through the API), one table, its structure or its rows. **Browse rows**: search the table's rows.
  **Restore…**: copy one table into a database under a new name ([RESTORE.md](RESTORE.md#one-table-into-a-live-database)).
- **Physical backups**: **Browse rows…** rebuilds the whole backup in the sandbox (needs disk for the whole database
  and as long as a restore) and lets you search any table. **Restore…** rebuilds a data folder.
- **Folder backups**: the file list (search, pages) and the archive to download.
- **PostgreSQL / Redis**: the dump files to download.

**Browse rows** loads the table into a **sandbox**: a throwaway MariaDB container on this server, reachable from this
machine only, removed after 30 idle minutes or with **Close sandbox**. Search every column, add filters per column
(=, ≠, <, >, contains, starts with, empty), sort, page, and **Export CSV** of exactly what is shown. Searches run
read-only, with a 30-second limit. Needs Docker access; memory limit `BM_SANDBOX_MEMORY` (default 512m).

---

## Proving backups: verify and restore tests

- **Verify now** reads every stored file back and checks its checksum, decryption and decompression.
- **Test restore** loads the backup into a fresh server in a throwaway container: SQL backups are loaded and every
  table's row count is compared; physical backups are rebuilt, the server is started and every table is checked
  (`CHECK TABLE`). The result, the time it took and any failure are shown on the backup; a failure sends an alert.
- In a plan, **Test by restoring, every N good backups** does it automatically.

Restore tests need Docker access and MariaDB backups.

---

## Sets: several servers at one moment

For an application whose data is spread over several database servers (shards): **Sets → New set**, choose the
plans, a schedule. The set starts all of them together; each run shows its members and, when all finished, the
**earliest moment from which all can be brought to the same time** with their binary logs. Turn off the members' own
schedules so they do not also run alone. Restoring a set: [RESTORE.md](RESTORE.md#several-servers-to-one-moment).

---

## Deploys and migrations

A logical backup in progress holds a light lock on the tables it has already saved; an `ALTER TABLE` on one of them
waits until the backup ends. Around a deploy that changes tables:

```bash
backup-manager hold        # no new backups start; running ones finish
# … deploy, run migrations …
backup-manager release
```

Or in a deploy script through the API with a token: `POST /api/hold {"on": true}`. Physical backups are not affected.

---

## Alerts

**Settings**: a Telegram bot token and chat id, and/or a webhook address that receives
`{"event": "failure"|"success", "text": "…"}`. Failures are always sent: a failed backup, a failed binary-log
archive, a backup that failed verification or its restore test, a set that could not start. **Also tell me when a
backup succeeds** sends successes too. **Save and send a test** checks the setup.

For monitoring tools: Prometheus metrics at `/metrics` ([API.md](API.md#metrics)), health at `/healthz`. Alert on
`time() - backup_manager_plan_last_success_timestamp_seconds > 2 * schedule` — that also catches a manager that stopped.

---

## Users, roles, two-factor, tokens

**Users & access**:

| Role | Can |
|---|---|
| **Viewer** | see backups, plans, jobs |
| **Operator** | + run plans and sets, verify, test, download, browse rows, cancel jobs, pin, pause the queue |
| **Owner** | + change everything: databases, destinations, plans, restores, deletions, settings, accounts, tokens, agents |

The last owner cannot be demoted or deleted. Deleting an account signs it out everywhere at once.

**Two-factor sign-in**: *My account → Turn on two-factor sign-in*, add the key to any authenticator app, type the
code, **save the 8 recovery codes** (each works once). Changing the password signs out your other devices.

**API tokens**: for scripts and the command line, each with a role. Shown once; revoke any time. See [API.md](API.md).

**Activity**: who signed in, downloaded, restored, deleted or changed what, and from which address.

Locked out: on the server, `backup-manager admin reset --email you@example.com` (run as the service user, see
[INSTALL.md troubleshooting](INSTALL.md#troubleshooting)).

---

## Language

The link at the bottom of the menu switches between English and Arabic (right to left). The choice is remembered in
the browser. Messages that come from the server stay in English.
