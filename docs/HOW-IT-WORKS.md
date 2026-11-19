# How it works

## The pieces

One program (`/usr/local/bin/backup-manager`, written in Go, no other runtime) holds:

- the **web panel** and its **API**;
- a **job queue** kept in the catalog, so jobs survive restarts;
- a **scheduler** for plans and sets;
- the **backup engines**, which start the proven native tools where the data is (`mariabackup`, `mbstream`,
  `mariadb-binlog`, `pg_dump`) or speak the protocols themselves (MySQL snapshot reads, Redis replication, tar);
- an **agent mode** for other servers.

Its own data is one SQLite file (the **catalog**: accounts, settings, plans, the list of backups and their contents)
and one 32-byte **master key** in its own file. Backup data never goes into the catalog; it goes straight to the
destination.

## Taking a backup at peak time

A backup must never break or visibly slow the application. The rules it follows:

1. **Prefer a replica.** Point the database at a replica and the primary does no backup work at all. A physical backup
   refuses a replica whose replication is stopped or more than *N* seconds behind (default 300): its backup would be
   an old copy that looks fine.
2. **Logical (SQL) backups take no table locks and no global lock.** They read everything inside one
   `START TRANSACTION WITH CONSISTENT SNAPSHOT`: the backup is the database exactly as it was at its first second, even
   while thousands of writes per second continue. Lock waits are capped at 10 seconds: if something (a migration)
   holds a table, the backup gives up and retries later instead of making the application queue behind it.
3. **Physical backups copy the data files while the server runs** (`mariabackup`), recording every change made during
   the copy, so the result is consistent at its end. The short final lock uses MariaDB's backup locks.
4. **Changes of table structure during a backup** are handled: a table changed by `ALTER` after the snapshot started
   fails that attempt clearly ("the backup will be retried") and the job retries. The structure and rows saved for one
   table always match.
5. **Stream, never stage.** Data flows `database → zstd → AES-256-GCM → destination` in one pipe. A 50 GB backup does
   not need 50 GB of free local disk, and does not write it twice.
6. **Low priority.** The service runs with low CPU weight and idle-class disk priority, `mariabackup` with
   `nice 19`; plans can cap their speed (MB/s). Backups of one database server run one at a time.
7. **Guards before writing.** A local destination short of space is refused before anything is written (a full disk
   stops every database on it). Redis snapshots are refused when Redis already uses a large part of the machine's
   memory.
8. **A failed backup changes nothing.** It only reads. Its partial files are deleted, the error is recorded and
   alerted, and the previous good backup stays.

Measured on a small test database: about 400–600 money transfers per second continued during a full backup on a
0.8-CPU database container; every restored copy had exactly matching totals. Numbers on a real 50 GB server are not
measured yet.

## Point in time

A physical backup records its binary-log position. The *Binary logs* plan copies the server's binary logs on every
run (the log still being written is copied too and replaced on the next run). A restore to a moment = the newest
physical backup before it + its incrementals + the archived logs from its position up to the moment. The finest
point you can reach is the time of the last archive run.

## Storage format

Each backup is a folder named by its id (`20261115T030000Z-a1b2c3`) on the destination:

```
20261115T030000Z-a1b2c3/
  meta.json                       plain: id, encrypted?, the backup's key wrapped by the master key
  manifest.json.zst.enc           what is inside, positions, sizes, checksums
  shop/orders.schema.sql.zst.enc  logical: one structure file and one rows file per table
  shop/orders.data.sql.zst.enc
  shop/_views.sql.zst.enc, _routines…, _triggers…
  physical.mbstream.zst.enc       physical
  files.tar.zst.enc + files.index.jsonl.zst.enc     folders
  dump.rdb.zst.enc                Redis
  <db>.sql.zst.enc + _globals.sql.zst.enc            PostgreSQL
binlog/<database id>/binlog.000123.zst.enc           archived binary logs
```

A backup is complete only when its `manifest` and `meta.json` exist; they are written last.

## Encryption

- The **master key** (32 random bytes, `/etc/backup-manager/master.key`, owner-only) never leaves the server and is
  never in the catalog or the destination.
- Each backup gets its own random **data key**, stored next to it *wrapped* (encrypted) by the master key.
- Each file is encrypted with AES-256-GCM in 1 MB frames with a per-file key, numbered frames and an
  authenticated "last frame" flag: a damaged, cut, reordered or swapped file is detected, never silently accepted.
- Database and storage passwords in the catalog are encrypted with the master key too.
- Every stored file also has a SHA-256 checksum in the manifest, checked on every read.

Losing the master key means losing access to every encrypted backup. Keep a copy off the server.

## The queue

Jobs (backup, verify, restore, restore test, sandbox load, table restore, binary-log archive) are rows in the catalog.
A worker takes the next one by priority (manual before scheduled), never two on the same database server. Each job
sends a heartbeat; a job interrupted by a crash or a restart is queued again at the next start. Failures are retried
with growing pauses (3 attempts by default); errors that a retry cannot fix (wrong setting, nothing matches) fail at
once with an explanation. Cancel stops the tool the job started, also inside a container or on an agent.

## Sandbox

Row search and restore tests run in a throwaway MariaDB container of the backup's own version, created for the job,
listening on this machine only, with random passwords, a memory limit and no binary logs; searches use a read-only
account with bound parameters (typed text never becomes SQL) and a 30-second limit. It is removed after 30 idle
minutes.

## Agents

An agent asks the panel for work over HTTPS (long polling: it connects out, nothing connects in), receives one task at
a time with the passwords it needs and the backup's data key, runs it next to the data, uploads straight to the
destination and returns the manifest. The panel keeps the catalog, the schedule, the chains of incrementals and the
master key. An agent that stops answering mid-backup is noticed within two minutes and the job is retried.

## Security summary

- Panel on this machine only unless you choose otherwise; built-in HTTPS or behind your web server.
- Passwords hashed with bcrypt; sign-in throttled per address; two-factor (TOTP) with one-time recovery codes;
  sessions in `HttpOnly`, `SameSite=Strict`, `Secure` (on HTTPS) cookies; a request header blocks cross-site requests.
- Roles (viewer / operator / owner) checked on every route; API tokens stored as hashes; agent tokens separate.
- Every download, restore, deletion and change is in the activity log with the user and address.
- The service runs as a locked-down user by default, with systemd hardening (no new privileges, read-only system).

## Limits

- Measured only on small data (tens of MB); no figures yet for tens of GB.
- Physical backups: MariaDB only (no MySQL / XtraBackup). Point in time from a replica's backup is not automatic.
- Restores of physical backups stop at "data folder ready + exact commands"; starting a server on it and switching
  the application is manual, on purpose.
- PostgreSQL: one consistent dump per database, not across databases; no point in time (no WAL archive).
- Redis: snapshots only; putting one back is manual ([RESTORE.md](RESTORE.md#redis)).
- Folders: every run is a full copy (no deduplication); a file changing while read is saved as first seen and listed.
- Sandbox and restore tests: MariaDB backups only, need Docker; a physical backup needs disk for the whole database.
- Logical backups hold a light lock on tables already saved until they finish (`ALTER` on those waits); after a
  restart a running backup starts again from the beginning.
- Agents do not archive binary logs or run restores and the sandbox; a "folder" destination on an agent is not
  readable by the panel.
- Sets start their members together and show the common moment; bringing every member to that moment is done per
  member.
