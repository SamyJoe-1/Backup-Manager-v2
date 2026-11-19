# Getting data back

Restore into something **new** first (a new database, a new folder, a second server), look at it, then switch over.
Backup Manager never overwrites a running database by itself.

- [SQL backup (MySQL / MariaDB)](#sql-backup-mysql--mariadb)
- [One table into a live database](#one-table-into-a-live-database)
- [A few rows](#a-few-rows)
- [Physical backup (MariaDB)](#physical-backup-mariadb)
- [To an exact second (point in time)](#to-an-exact-second-point-in-time)
- [PostgreSQL](#postgresql)
- [Redis](#redis)
- [Folders](#folders)
- [Several servers to one moment](#several-servers-to-one-moment)
- [An application with a database and Redis](#an-application-with-a-database-and-redis)
- [When the Backup Manager server is lost](#when-the-backup-manager-server-is-lost)

---

## SQL backup (MySQL / MariaDB)

Backup → **Download all (.sql)** gives one file that loads anywhere. It creates each database (`CREATE DATABASE IF NOT
EXISTS` + `USE`), its tables, rows, views, routines and triggers.

```bash
mysql -u root -p < 20261115T030000Z-a1b2c3.sql
```

Into a database with **another name** (to compare before switching), download with `nodb=1` (one database at a time):

```
https://your-panel/api/backups/<id>/download?db=shop&nodb=1
```

```bash
mysql -u root -p -e 'CREATE DATABASE shop_restored CHARACTER SET utf8mb4'
mysql -u root -p shop_restored < shop.sql
```

Big files: `pv shop.sql | mysql …` shows progress. A download that breaks is reported as failed, never saved as a
file that looks complete.

---

## One table into a live database

Backup → table → **Restore…**: copies that table into a database you choose **under a new name**
(`orders__restored_20261115_0930` by default). It refuses a name that exists, never drops or replaces anything, and
removes its own new table again if the row count does not match the backup. Give a database user that may create
tables (the backup user is read-only); the password is used once and not stored.

Then compare and swap yourself:

```sql
RENAME TABLE orders TO orders_broken, orders__restored_20261115_0930 TO orders;
```

Foreign keys of the copy get new names (suffix `_r…`) because names are unique per database.

---

## A few rows

Backup → table → **Browse rows** → filter to the rows you need → **Export CSV**, or copy the values. For a physical
backup: **Browse rows…** on the backup.

---

## Physical backup (MariaDB)

1. Backup → **Restore…** → a **new, empty folder** (e.g. `/var/lib/mysql-restore`), on the machine where the backup
   tools run (inside the container for a database in Docker). It needs room for the whole database.
2. The job extracts the full backup, applies every incremental up to the one you chose, and prepares the files.
   The folder is then a complete data folder, with `BM-RESTORE.txt` explaining it.
3. **Check it on a second server first**:

```bash
chown -R mysql:mysql /var/lib/mysql-restore
mariadbd --user=mysql --datadir=/var/lib/mysql-restore --port=3307 --socket=/tmp/restore.sock --skip-log-bin &
mariadb --socket=/tmp/restore.sock -e 'SHOW DATABASES'
```

   (Docker: `docker run -d -v /path/mysql-restore:/var/lib/mysql mariadb:<same version>`.)
4. **Switch over** when it is right:

```bash
systemctl stop mariadb
mv /var/lib/mysql /var/lib/mysql.before-restore
mv /var/lib/mysql-restore /var/lib/mysql
systemctl start mariadb
```

Use the same MariaDB major version as the backup (the backup page shows it). After restoring a primary, its replicas
no longer match it: rebuild each replica from the restored primary.

---

## To an exact second (point in time)

Needs a physical backup and a **Binary logs** plan for the same server (its primary) that ran after the moment you
want.

1. Run the binary-log plan once more (**Plans → Run now**) so the archive reaches the moment.
2. Restore the newest physical backup taken **before** the moment, and in the Restore dialog fill **Roll forward to a
   moment** (your local time).
3. The folder then also holds `bm-binlogs/` and `BM-RESTORE.txt` has the exact command. Start a server on the folder
   (step 3 above), then run the command, for example:

```bash
cd /var/lib/mysql-restore/bm-binlogs && \
TZ=UTC sh -c "mariadb-binlog --start-position=4711 --stop-datetime='2026-11-15 12:59:59' binlog.000041 binlog.000042 | mariadb --socket=/tmp/restore.sock"
```

The classic use: someone ran `DELETE` without `WHERE` at 13:00 → roll forward to 12:59:59.

A backup taken **from a replica** records the replica's position, not the primary's; rolling it forward with the
primary's logs is not automatic yet. For point in time, take the physical backups from the primary (at a quiet hour,
with a speed limit) or from a replica with its own binary logs.

---

## PostgreSQL

Download the database's file (`<db>.sql`) and the roles (`_globals.sql`):

```bash
psql -U postgres -f _globals.sql                     # roles first (errors for roles that exist are fine)
createdb -U postgres shop_restored
psql -U postgres -d shop_restored -v ON_ERROR_STOP=1 -f shop.sql
```

Each database is consistent in itself. There is no point-in-time restore for PostgreSQL.

---

## Redis

Download `dump.rdb`. Redis prefers its append-only file (AOF) over `dump.rdb` when AOF is on, so:

```bash
systemctl stop redis-server
mv /var/lib/redis/appendonlydir /var/lib/redis/appendonlydir.before-restore   # Redis 7 (older: appendonly.aof)
cp dump.rdb /var/lib/redis/dump.rdb && chown redis:redis /var/lib/redis/dump.rdb
# in redis.conf: appendonly no  (for this one start)
systemctl start redis-server
redis-cli DBSIZE
redis-cli CONFIG SET appendonly yes      # writes a fresh AOF from memory
# in redis.conf: appendonly yes again
```

Read the next section before restoring Redis next to a database.

---

## Folders

Download the archive (`files.tar`). Paths are stored without the leading `/`, with owners, permissions and links:

```bash
mkdir /tmp/restored && tar -xpf files.tar -C /tmp/restored    # look first
tar -xpf files.tar -C / srv/app/uploads                        # then put back only what you need, as root
```

The backup's file list (search, pages) shows what is inside without downloading. A file that changed while it was
read is listed as a warning on the backup.

---

## Several servers to one moment

For an application spread over several database servers (shards), run their plans as a **set**. Each set run shows
the earliest moment from which all members can be brought to the same time. To restore the whole platform:

1. Pause what writes: application workers, cron jobs; `backup-manager hold`.
2. Choose one moment at or after that time. For **every** server, restore its physical backup from the set run and
   roll it forward with its own binary logs **to that same moment** (previous section).
3. Restore the server holding shared/global data first, then the others.
4. Rebuild replicas and any aggregating copies from the restored primaries.
5. Start the application, `backup-manager release`.

---

## An application with a database and Redis

When an application keeps live state in Redis (sessions, queues, counters not yet written to the database), the two
must agree after a restore. General rules:

- **Restoring only the database to its newest point** (physical + binary logs, Redis left alone) is the normal case.
  If the application writes Redis events to the database with a stored "last processed id", it continues after that
  id and nothing is lost or doubled.
- **Restoring the database to an older moment on purpose** loses whatever happened after it in the database; data
  already removed from Redis cannot come back. Do it knowingly.
- **Never put an old Redis snapshot over a healthy Redis** to "match" a database restore: it replaces live sessions
  with old ones.
- If Redis is lost too: start it empty or from the newest snapshot; live sessions are rebuilt by the application.
- Order: stop the writers to the database first (the process that moves Redis data into it last to start again),
  restore, drop rebuildable cache keys, start the application, start that writer last.

---

## When the Backup Manager server is lost

What you need: **the master key** (you copied it off the server, [INSTALL.md section 10](INSTALL.md#10-after-installing))
and the **destination** (S3 bucket or another server's folder).

**Prepare once** (before anything happens): add a *Folders* target with
`/var/lib/backup-manager/catalog.backup.db` — a consistent copy of the catalog that the service refreshes every 6
hours (or make one any time: `backup-manager catalog-backup /path/copy.db`) — and a daily plan sending it to your
S3 destination.

**On a new server:**

1. Install Backup Manager ([INSTALL.md](INSTALL.md)). Do not create an account.
2. Put the old key in place and stop the service:

```bash
systemctl stop backup-manager
cp old-master.key /etc/backup-manager/master.key && chown backupmgr: /etc/backup-manager/master.key && chmod 600 /etc/backup-manager/master.key
```

3. Download the newest catalog backup from the destination (the folder `<backup id>/` holding `files.tar.zst.enc`
   and `meta.json`), then open it with only the key:

```bash
env $(grep -v '^#' /etc/backup-manager/env | xargs) backup-manager decrypt --in <backup id>/files.tar.zst.enc
tar -xf files.tar
cp var/lib/backup-manager/catalog.backup.db /var/lib/backup-manager/catalog.db
chown backupmgr: /var/lib/backup-manager/catalog.db
systemctl start backup-manager
```

4. Sign in with your old account. Every backup, plan and setting is back, and every backup on the destination can be
   downloaded and restored again. Check **Destinations** (a folder destination of the old server is gone) and the
   databases' addresses.

**Without any catalog copy**, any single file can still be opened with only the key. Each backup on the destination is
a folder `<backup id>/` with `meta.json` and the files:

```bash
backup-manager decrypt --in <backup id>/shop/orders.data.sql.zst.enc      # finds meta.json by itself
backup-manager decrypt --in <backup id>/shop/orders.schema.sql.zst.enc
cat orders.schema.sql orders.data.sql | mysql -u root -p shop_restored
```

Physical backups (`physical.mbstream.zst.enc`) open the same way; then `mbstream -x -C folder < physical.mbstream`
and `mariabackup --prepare --target-dir=folder`. Unencrypted backups (`.zst` only) open with `zstd -d`.

Without the master key, encrypted backups cannot be opened by anyone. That is the point of encryption — keep the key.
