# API, command line, metrics

Everything the panel does goes through this API, so anything in the panel can be scripted.

## Authentication

Create a token: **Users & access → API tokens → New token** (or on the server
`backup-manager token create --name deploy --role operator`). It is shown once. Send it as:

```
Authorization: Bearer bm_xxxxxxxxxxxxxxxx
```

The token's role (viewer / operator / owner) decides what it may do, exactly as for people
([USER-GUIDE.md](USER-GUIDE.md#users-roles-two-factor-tokens)). A refusal is `403` with the reason; a wrong token
`401`. Browser sessions use a cookie and must send the header `X-BM: 1` on changing requests; tokens do not need it.

Errors are `{"error": "explanation"}` with a 4xx/5xx status. Times are Unix seconds. Sizes are bytes.

## Command line

On any machine, with the panel's address and a token:

```bash
export BM_URL=https://backups.example.com BM_TOKEN=bm_xxxxxxxx
backup-manager plans                        # list plans
backup-manager run --wait "Nightly full"    # start a plan by name or id; exit code 1 when it fails
backup-manager backups                      # newest 20 backups
backup-manager jobs                         # the queue
backup-manager api GET '/backups?status=failed'
backup-manager api POST /hold '{"on":true}'
```

On the server itself:

| Command | Does |
|---|---|
| `backup-manager serve` | the service (systemd starts it) |
| `backup-manager setup-link` | one-time link to create the first account |
| `backup-manager admin create\|reset --email E` | create an account / reset its password (asks for it, or `BM_PASSWORD`) |
| `backup-manager token create --name N --role R` | make an API token |
| `backup-manager hold` / `release` | stop / allow new jobs (around deploys) |
| `backup-manager doctor` | check tools, Docker access, disk, settings |
| `backup-manager catalog-backup FILE` | consistent copy of the catalog |
| `backup-manager decrypt --in FILE [--meta meta.json] [--out FILE]` | open a stored file with only the master key |
| `backup-manager upgrade [--check]` | install the newest release (public repository) |
| `backup-manager agent --server URL --token T` | agent mode |
| `backup-manager version` | |

Commands on the server read the same settings as the service: run them as the service user with its environment,
e.g. `sudo -u backupmgr env $(grep -v '^#' /etc/backup-manager/env | xargs) backup-manager doctor`.

## Endpoints

Role needed: **V** viewer, **O** operator, **W** owner.

### Backups

| | Endpoint | Role |
|---|---|---|
| list | `GET /api/backups?page=&pageSize=&search=&status=&plan=&target=&from=&to=&sort=created\|size\|rows\|duration\|status&dir=asc\|desc&pinned=1&deleted=1` → `{items, total}` | V |
| one, with its manifest | `GET /api/backups/{id}` | V |
| download SQL | `GET /api/backups/{id}/download[?db=&table=&part=schema\|data&nodb=1]` | O |
| download one stored file, decrypted | `GET /api/backups/{id}/download?obj=<key from the manifest>` | O |
| download one stored file as stored (resumable) | `GET /api/backups/{id}/download?raw=<key>` | O |
| file list (folder backups) | `GET /api/backups/{id}/files?search=&page=&pageSize=` | O |
| verify / restore test | `POST /api/backups/{id}/verify`, `POST /api/backups/{id}/test` | O |
| pin, note | `POST /api/backups/{id}/pin {"pinned":true}`, `POST /api/backups/{id}/note {"note":"…"}` | O |
| delete | `DELETE /api/backups/{id}` | W |
| physical restore | `POST /api/backups/{id}/restore {"dir":"/var/lib/mysql-restore","stopAt":1763211599,"exec":""}` | W |
| one table under a new name | `POST /api/backups/{id}/restore-table {"db":"shop","table":"orders","targetId":1,"toDb":"shop","toTable":"orders_restored","user":"root","password":"…"}` | W |
| sandbox state / load / close | `GET`, `POST {"db":"shop","table":"orders"}`, `DELETE /api/backups/{id}/explore` | V / O / O |
| search rows | `GET /api/backups/{id}/rows?db=&table=&page=&pageSize=&sort=&dir=&search=&filters=[{"column":"id","op":"ge","value":"10"}]&format=csv` | O |

Row filter `op`: `eq ne lt gt le ge contains starts null notnull`.

### Plans, sets, queue

| | Endpoint | Role |
|---|---|---|
| plans | `GET /api/plans`, `POST /api/plans`, `PUT /api/plans/{id}`, `DELETE /api/plans/{id}` | V / W |
| run a plan | `POST /api/plans/{id}/run` → the queued backup | O |
| sets | `GET/POST /api/sets`, `PUT/DELETE /api/sets/{id}`, `POST /api/sets/{id}/run`, `GET /api/sets/{id}/runs` | V / W / O |
| jobs | `GET /api/jobs?page=&pageSize=&state=`, `POST /api/jobs/{id}/cancel` | V / O |
| hold the queue | `POST /api/hold {"on":true}` | O |
| dashboard numbers | `GET /api/dashboard` | V |
| live events (server-sent events) | `GET /api/events` | V |

A plan:

```json
{
  "name": "Shop nightly",
  "targetId": 1, "destinationId": 2,
  "schedule": "0 3 * * *",
  "keepDays": 14, "keepLast": 3, "enabled": true,
  "spec": {
    "method": "logical",
    "databases": ["shop"], "tables": [], "exclude": ["shop.logs_*"],
    "content": "all",
    "where": {"shop.events": "created_at > NOW() - INTERVAL 30 DAY"},
    "noViews": false, "noRoutines": false, "noTriggers": false,
    "level": 3, "noEncrypt": false, "rateMB": 50,
    "incrementalsPerFull": 0, "parallel": 1,
    "testEvery": 7,
    "preHook": "", "postHook": "",
    "force": false
  }
}
```

`method`: `logical`, `physical`, `binlog` for MySQL/MariaDB targets; PostgreSQL, Redis and folder targets set it
themselves. `content`: `all`, `schema`, `data`. `tables`/`exclude` are `database.table`, `*` allowed. `level` 1–4
(zstd speed ↔ size). `force` skips the Redis memory guard.

### Databases, destinations, settings

| | Endpoint | Role |
|---|---|---|
| databases & folders | `GET/POST /api/targets`, `PUT/DELETE /api/targets/{id}`, `POST /api/targets/{id}/test`, `GET /api/targets/{id}/tables`, `GET /api/targets/{id}/binlogs` | V / W |
| destinations | `GET/POST /api/destinations`, `PUT/DELETE /api/destinations/{id}`, `POST /api/destinations/{id}/test` | V / W |
| alerts | `GET/PUT /api/settings` | W |
| activity log | `GET /api/audit?page=` | W |
| accounts | `GET/POST /api/users`, `PUT/DELETE /api/users/{id}` | W |
| tokens | `GET/POST /api/tokens`, `DELETE /api/tokens/{id}` | W |
| agents | `GET/POST /api/agents`, `DELETE /api/agents/{id}` | W |
| my account | `GET /api/me`, `POST /api/me/password`, `POST /api/me/totp/start\|enable\|disable` | V |

Target bodies — `{"name": "…", "kind": "…", "cfg": {…}, "secret": "password"}`:

| kind | cfg |
|---|---|
| `mysql` | `host`, `port`, `socket`, `user`, `tls`, `exec` (`local` / `docker:<name>`), `maxLag`, `agent` |
| `postgres` | `host`, `port`, `user`, `exec` (required), `agent` |
| `redis` | `host`, `port`, `user`, `agent` |
| `files` | `paths` (list), `exclude` (list), `agent` |

Destination bodies: `{"kind":"local","cfg":{"path":"/backups"}}` or
`{"kind":"s3","cfg":{"endpoint":"…","region":"…","bucket":"…","prefix":"…","accessKey":"…","useSSL":true},"secret":"<secret key>"}`.

### Example: back up before a deploy

```bash
id=$(curl -fsS -X POST -H "Authorization: Bearer $BM_TOKEN" https://backups.example.com/api/plans/3/run | jq -r .id)
until s=$(curl -fsS -H "Authorization: Bearer $BM_TOKEN" https://backups.example.com/api/backups/$id | jq -r .backup.status); \
      [ "$s" = done ] || [ "$s" = failed ]; do sleep 10; done
[ "$s" = done ] || { echo "backup failed, not deploying"; exit 1; }
```

(or simply `backup-manager run --wait 3`).

## Metrics

`GET /metrics` (token or session, viewer) in Prometheus format:

| Metric | Meaning |
|---|---|
| `backup_manager_backups` | good backups stored |
| `backup_manager_stored_bytes` | bytes they use |
| `backup_manager_last_success_timestamp_seconds` | newest good backup |
| `backup_manager_plan_last_success_timestamp_seconds{plan="…"}` | newest good backup per plan |
| `backup_manager_failed_7d` | failed backups, last 7 days |
| `backup_manager_jobs_running`, `backup_manager_jobs_queued` | the queue |
| `backup_manager_restore_tests_failed` | stored backups whose restore test failed |
| `backup_manager_binlog_last_archive_timestamp_seconds` | last binary-log archive |

```yaml
scrape_configs:
  - job_name: backup-manager
    scheme: https
    authorization: { credentials: bm_xxxxxxxx }
    static_configs: [ { targets: ["backups.example.com"] } ]
```

`GET /healthz` (no sign-in) answers `ok` while the service and its catalog work.
