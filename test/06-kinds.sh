#!/usr/bin/env bash
# Folders, Redis, PostgreSQL, hooks and backup sets. Standalone. Needs the containers named in README.md.
source "$(dirname "$0")/lib.sh"
mkdir -p "$RUN"; rm -rf "$RUN/data" "$RUN/keys" "$RUN/store" "$RUN/tree" "$RUN/out"
catalog() { python3 -c "import sqlite3,sys; c=sqlite3.connect('$RUN/data/catalog.db', timeout=20); r=c.execute(sys.argv[1]).fetchall(); c.commit(); print('\n'.join('|'.join(str(x) for x in row) for row in r))" "$1"; }
start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
D=$(call POST /destinations -d "{\"name\":\"local\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
plan() { call POST /plans -d "{\"name\":\"$1\",\"targetId\":$2,\"destinationId\":$D,\"spec\":$3,\"schedule\":\"\",\"enabled\":true}"; }
obj() { call GET "/backups/$1" | js "[o['key'] for o in d['manifest']['objects'] if o['kind']=='$2'][${3:-0}]"; }
dl() { curl -sS -b "$JAR" -G -o "$3" --data-urlencode "obj=$2" "$URL/api/backups/$1/download"; }

echo "== folders"
T="$RUN/tree"; mkdir -p "$T/app/src" "$T/app/node_modules/pkg" "$T/app/uploads/مجلد" "$T/empty-dir"
echo 'console.log(1)' > "$T/app/src/main.js"; echo 'SECRET_BOX_KEY=abc123' > "$T/app/.env"; chmod 600 "$T/app/.env"
head -c 5000000 /dev/urandom > "$T/app/uploads/photo.bin"; echo 'arabic name' > "$T/app/uploads/مجلد/ملف عربي.txt"; echo 'x y' > "$T/app/uploads/name with spaces.txt"
echo junk > "$T/app/node_modules/pkg/index.js"; echo log > "$T/app/debug.log"; ln -s src/main.js "$T/app/link.js"; mkfifo "$T/app/pipe"; : > "$T/app/zero-bytes"
TF=$(call POST /targets -d "{\"name\":\"app files\",\"kind\":\"files\",\"cfg\":{\"paths\":[\"$T/app\",\"$T/empty-dir\"],\"exclude\":[\"node_modules\",\"*.log\"]}}" | js 'd["id"]')
eq "folder target test" "$(call POST /targets/$TF/test -d '{}' | js 'd["ok"]')" True
PF=$(plan files $TF '{"content":"all"}' | js 'd["id"]')
BF=$(run_plan $PF); eq "folder backup finished" "$(wait_backup $BF 120)" done
eq "it is a files backup with 6 files" "$(call GET /backups/$BF | js '(d["backup"]["method"], [o["rows"] for o in d["manifest"]["objects"] if o["kind"]=="files"][0])')" "('files', 6)"
check "stored archive is encrypted" bash -c "! grep -r -a -q -e 'SECRET_BOX_KEY' -e 'console.log' '$RUN/store/$BF'"
mkdir -p "$RUN/out"; dl $BF "$(obj $BF files)" "$RUN/out/files.tar"; mkdir "$RUN/out/x"; tar -xf "$RUN/out/files.tar" -C "$RUN/out/x"
R="$RUN/out/x$T"
check "downloaded archive unpacks to the same files" bash -c "diff -r --no-dereference -x node_modules -x debug.log -x pipe '$T/app' '$R/app'"
check "excluded folders and patterns are not in it" bash -c "[ ! -e '$R/app/node_modules' ] && [ ! -e '$R/app/debug.log' ] && [ ! -e '$R/app/pipe' ]"
eq "permissions kept" "$(stat -c %a "$R/app/.env")" 600
eq "symlink kept as a link" "$(readlink "$R/app/link.js")" "src/main.js"
check "empty folder and empty file kept" bash -c "[ -d '$R/empty-dir' ] && [ -f '$R/app/zero-bytes' ] && [ ! -s '$R/app/zero-bytes' ]"
eq "file list can be searched" "$(curl -sS -b "$JAR" -G --data-urlencode 'search=عربي' "$URL/api/backups/$BF/files" | js 'd["total"]')" 1
eq "file list pages" "$(call GET "/backups/$BF/files?pageSize=3&page=2" | js '(len(d["items"]), d["total"]>6)')" "(3, True)"
call POST /backups/$BF/verify -d '{}' >/dev/null; sleep 3
eq "verify reads the archive back" "$(call GET /backups/$BF | js '(d["backup"]["verifiedAt"]>0, d["backup"]["verifyError"])')" "(True, '')"
eq "a missing path fails clearly" "$(TX=$(call POST /targets -d '{"name":"gone","kind":"files","cfg":{"paths":["/no/such/folder-xyz"]}}' | js 'd["id"]'); BX=$(run_plan $(plan gone $TX '{}' | js 'd["id"]')); wait_backup $BX 30; call GET /backups/$BX | js '"no such file" in d["backup"]["error"]')" "failed
True"

echo "== Redis"
rcli() { docker exec -i bm-test-redis redis-cli --no-auth-warning -a bmtest-redis-pw "$@"; }
seq 1 30000 | awk '{print "SET key:"$1" value-"$1"-"$1*7}' | rcli --pipe >/dev/null; rcli RPUSH jobs a b c >/dev/null; rcli HSET session:1 user 42 mac aa:bb >/dev/null; rcli XADD rad:ev '*' k v >/dev/null
TR=$(call POST /targets -d '{"name":"redis","kind":"redis","cfg":{"host":"127.0.0.1","port":36379},"secret":"bmtest-redis-pw"}' | js 'd["id"]')
eq "Redis target test" "$(call POST /targets/$TR/test -d '{}' | js 'd["version"][:7]')" "Redis 7"
PR=$(plan redis $TR '{}' | js 'd["id"]')
( for i in $(seq 1 40); do rcli SET "live:$i" "$i" >/dev/null; sleep 0.05; done ) & LP=$!
BR=$(run_plan $PR); eq "Redis backup finished" "$(wait_backup $BR 120)" done; wait $LP
eq "Redis kept answering and lost nothing" "$(rcli GET key:777)/$(rcli GET live:40)" "value-777-5439/40"
eq "key count recorded" "$(call GET /backups/$BR | js '"keys=" in d["manifest"]["position"].get("db0","")')" True
dl $BR "$(obj $BR rdb)" "$RUN/out/dump.rdb"
eq "downloaded file is a Redis snapshot" "$(head -c 5 "$RUN/out/dump.rdb")" REDIS
docker rm -f bm-test-redis2 >/dev/null 2>&1; mkdir -p "$RUN/out/rdata"; cp "$RUN/out/dump.rdb" "$RUN/out/rdata/dump.rdb"; chmod -R a+rwX "$RUN/out/rdata"
docker run -d --name bm-test-redis2 --memory=150m -v "$RUN/out/rdata:/data" redis:7-alpine >/dev/null; sleep 3
r2() { docker exec bm-test-redis2 redis-cli "$@"; }
eq "a new Redis started from the snapshot has the data" "$(r2 GET key:29999)/$(r2 LLEN jobs)/$(r2 HGET session:1 mac)/$(r2 XLEN rad:ev)" "value-29999-209993/3/aa:bb/1"
if [ "$(r2 DBSIZE)" -ge 30003 ]; then ok "all keys are there ($(r2 DBSIZE))"; else bad "only $(r2 DBSIZE) keys"; fi
docker rm -f bm-test-redis2 >/dev/null
TRX=$(call POST /targets -d '{"name":"redis-badpw","kind":"redis","cfg":{"host":"127.0.0.1","port":36379},"secret":"wrong"}' | js 'd["id"]')
eq "wrong Redis password is reported" "$(call POST /targets/$TRX/test -o /dev/null -w '%{http_code}' -d '{}')" 400

echo "== PostgreSQL"
pg() { docker exec -i bm-test-pg psql -X -q -At -U postgres "$@"; }
for i in $(seq 1 30); do pg -c 'select 1' >/dev/null 2>&1 && break; sleep 1; done
pg -c 'CREATE DATABASE shop' ; pg -d shop -c "CREATE TABLE items (id serial PRIMARY KEY, name text, price numeric(10,2), tags text[], meta jsonb, made timestamptz DEFAULT now()); INSERT INTO items (name, price, tags, meta) SELECT 'item ' || g || E' it''s \"q\" عربي', g * 1.5, ARRAY['a','b'], jsonb_build_object('n', g) FROM generate_series(1, 20000) g; CREATE TABLE other_t (k int); INSERT INTO other_t VALUES (1),(2); CREATE VIEW cheap AS SELECT * FROM items WHERE price < 10; CREATE ROLE app_user LOGIN;"
TP=$(call POST /targets -d '{"name":"pg","kind":"postgres","cfg":{"user":"postgres","exec":"docker:bm-test-pg"},"secret":"bmtest-pg-pw"}' | js 'd["id"]')
eq "PostgreSQL target test" "$(call POST /targets/$TP/test -d '{}' | js 'd["version"][:13]')" "PostgreSQL 16"
eq "PostgreSQL target needs a place to run pg_dump" "$(TPX=$(call POST /targets -d '{"name":"pg-noexec","kind":"postgres","cfg":{"user":"postgres"}}' | js 'd["id"]'); plan x $TPX '{}' | js '"run the backup tools" in d.get("error","")')" True
eq "table picker lists its tables" "$(call GET /targets/$TP/tables | js 'sorted(t["name"] for t in d if t["db"]=="shop")')" "['items', 'other_t']"
PP=$(plan pg-all $TP '{"content":"all","databases":["shop"]}' | js 'd["id"]')
BP=$(run_plan $PP); eq "PostgreSQL backup finished" "$(wait_backup $BP 120)" done
eq "one dump per database plus the roles" "$(call GET /backups/$BP | js 'sorted(o["kind"] for o in d["manifest"]["objects"])')" "['pgdump', 'pgglobals']"
dl $BP "$(obj $BP pgdump)" "$RUN/out/shop.sql"; dl $BP "$(obj $BP pgglobals)" "$RUN/out/globals.sql"
pg -c 'CREATE DATABASE shop_restored'
if pg -v ON_ERROR_STOP=1 -d shop_restored < "$RUN/out/shop.sql" >/dev/null 2>"$RUN/out/pg.err"; then ok "the dump loads into a new database"; else bad "pg restore failed: $(head -2 "$RUN/out/pg.err")"; fi
eq "restored rows are identical" "$(pg -d shop_restored -c "SELECT count(*), md5(string_agg(id || name || price || tags::text || meta::text, ',' ORDER BY id)) FROM items")" "$(pg -d shop -c "SELECT count(*), md5(string_agg(id || name || price || tags::text || meta::text, ',' ORDER BY id)) FROM items")"
eq "view and sequence restored" "$(pg -d shop_restored -c "SELECT (SELECT count(*) FROM cheap) || '/' || nextval('items_id_seq')")" "6/20001"
check "roles are in the globals file" grep -q 'app_user' "$RUN/out/globals.sql"
PP2=$(plan pg-one $TP '{"content":"schema","tables":["shop.other_t"]}' | js 'd["id"]')
BP2=$(run_plan $PP2); wait_backup $BP2 60 >/dev/null; dl $BP2 "$(obj $BP2 pgdump)" "$RUN/out/one.sql"
eq "chosen table, schema only" "$(grep -c 'CREATE TABLE' "$RUN/out/one.sql")/$(grep -c 'items' "$RUN/out/one.sql")/$(grep -c '^COPY' "$RUN/out/one.sql")" "1/0/0"

echo "== hooks"
TM=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup"},"secret":"bm-backup-pw"}' | js 'd["id"]')
PH=$(plan hooked $TM "{\"content\":\"all\",\"tables\":[\"shop.stats\"],\"preHook\":\"echo \$BM_PLAN/\$BM_STATUS > $RUN/out/pre.txt\",\"postHook\":\"echo \$BM_STATUS/\$BM_BACKUP_ID > $RUN/out/post.txt\"}" | js 'd["id"]')
BH=$(run_plan $PH); eq "backup with hooks finished" "$(wait_backup $BH 60)" done
eq "before-hook ran with the plan's details" "$(cat "$RUN/out/pre.txt")" "hooked/starting"
eq "after-hook ran with the result" "$(cat "$RUN/out/post.txt")" "done/$BH"
PH2=$(plan hook-fails $TM "{\"content\":\"all\",\"tables\":[\"shop.stats\"],\"preHook\":\"echo cannot pause the app >&2; exit 3\",\"postHook\":\"echo \$BM_STATUS > $RUN/out/post2.txt\"}" | js 'd["id"]')
BH2=$(run_plan $PH2)
eq "a failing before-hook stops the backup, with its message" "$(wait_backup $BH2 60)/$(call GET /backups/$BH2 | js '"cannot pause the app" in d["backup"]["error"]')" "failed/True"
PH3=$(plan after-fails $TM '{"content":"all","tables":["shop.stats"],"postHook":"exit 9"}' | js 'd["id"]')
BH3=$(run_plan $PH3)
eq "a failing after-hook does not spoil a good backup, but is noted" "$(wait_backup $BH3 60)/$(call GET /backups/$BH3 | js 'len(d["manifest"]["warnings"])')" "done/1"

echo "== backup sets"
PA=$(plan part-a $TM '{"content":"all","tables":["shop.accounts"]}' | js 'd["id"]'); PB=$(plan part-b $TM '{"content":"all","tables":["shop.ledger"]}' | js 'd["id"]')
eq "a set needs two plans" "$(call POST /sets -o /dev/null -w '%{http_code}' -d "{\"name\":\"x\",\"planIds\":[$PA]}")" 400
S=$(call POST /sets -d "{\"name\":\"platform\",\"planIds\":[$PA,$PB,$PR],\"schedule\":\"@every 12h\"}" | js 'd["id"]')
RUNID=$(call POST /sets/$S/run -d '{}' | js 'd["run"]')
for i in $(seq 1 80); do [ "$(catalog "SELECT COUNT(*) FROM backups WHERE set_run='$RUNID' AND status='done'")" = 3 ] && break; sleep 0.5; done
eq "all three members ran under one run id" "$(catalog "SELECT COUNT(*) FROM backups WHERE set_run='$RUNID' AND status='done'")" 3
eq "the run is complete and names the common moment" "$(call GET /sets/$S/runs | js '(d[0]["complete"], d[0]["commonFrom"]==max(b["finishedAt"] for b in d[0]["backups"]), len(d[0]["backups"]))')" "(True, True, 3)"
eq "a set with a schedule gets a next run" "$(call GET /sets | js 'd[0]["nextRunAt"]>0')" True
catalog "UPDATE plan_sets SET next_run_at=1" >/dev/null
for i in $(seq 1 60); do N=$(catalog "SELECT COUNT(DISTINCT set_run) FROM backups WHERE set_run LIKE 'set$S-%'"); [ "$N" -ge 2 ] && break; sleep 0.5; done
eq "the scheduler starts a due set" "$N" 2
stop_manager
echo; echo "06: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
