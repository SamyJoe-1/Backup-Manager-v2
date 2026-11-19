# End-to-end tests

They run against throwaway containers, never against a real database. Run them from this folder.

```bash
mkdir -p .run/restore
docker run -d --name bm-test-db --memory=380m --cpus=0.8 -p 127.0.0.1:33999:3306 -v "$PWD/.run/restore:/restore" \
  -e MARIADB_ROOT_PASSWORD=bmtest-root-pw mariadb:10.11 \
  --innodb-buffer-pool-size=96M --log-bin=binlog --server-id=99 --performance-schema=OFF --skip-name-resolve
docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw < seed.sql
docker run -d --name bm-test-minio --memory=300m -p 127.0.0.1:39000:9000 -e MINIO_ROOT_USER=bmtestaccess \
  -e MINIO_ROOT_PASSWORD=bmtest-secret-key-123 --entrypoint sh pgsty/minio -c 'mkdir -p /data/bm-bucket && exec minio server /data'
docker run -d --name bm-test-redis --memory=150m -p 127.0.0.1:36379:6379 redis:7-alpine redis-server --requirepass bmtest-redis-pw
docker run -d --name bm-test-pg --memory=250m -e POSTGRES_PASSWORD=bmtest-pg-pw postgres:16-alpine
```

| Script | Covers | Needs |
|---|---|---|
| `01-full-under-load.sh` | logical backup during writes, restore, compare; access rules | db |
| `02-queue-and-safety.sh` | partial backups, kill mid-backup, cancel, altered tables, tamper, retention, hold, schedule (run after 01) | db |
| `03-s3.sh` | S3 storage, storage going down (run after 01) | db, minio |
| `04-physical.sh` | physical, incremental, binary logs, point-in-time | db |
| `05-sandbox.sh` | row search, table restore, restore tests | db, Docker |
| `06-kinds.sh` | folders, Redis, PostgreSQL, hooks, sets | db, redis, pg |
| `07-access.sh` | roles, tokens, two-factor, command line, metrics, self-update | db |
| `08-agents-docker.sh` | agents, container image | db, minio, Docker |
| `09-recovery.sh` | the manager's own server is lost: catalog copy, decrypt with only the key | db |

`browser/*.mjs` drive the panel in headless Chromium over the DevTools protocol.

## Installer on fresh machines

`installer/` installs the program on throwaway containers that run **real systemd**, in every supported way, and
checks the result: refusals and dry run, plain install / reboot / upgrade / uninstall (Debian 12), a server with
MariaDB installed directly with root mode, tools, database user and real physical / SQL / folder backups (Debian 12),
build from source with built-in HTTPS (Ubuntu 24.04), a server already running nginx (Ubuntu 22.04), the panel on the
server IP (Rocky 9), an agent on a second server, and a machine without systemd.

```bash
scripts/release.sh                 # dist/ with the program files
test/installer/images.sh           # the fresh-machine images
test/installer/run.sh              # all scenarios, or name some: run.sh plain mariadb
```

The containers run privileged (systemd needs it) with a memory limit, and are removed after each scenario.

Clean up with `-v`, the database volume holds a lot of binary log:

```bash
docker rm -fv bm-test-db bm-test-restore bm-test-minio bm-test-redis bm-test-pg
docker rmi bm-vps:debian12 bm-vps:ubuntu24 bm-vps:ubuntu22 bm-vps:rocky9      # installer images
```
