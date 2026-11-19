#!/usr/bin/env bash
# Partial backups, crash recovery, cancel, retention, tamper detection, DDL during a backup, hold, schedule.
# Needs 01 to have run first (it creates the account, the target and the destination).
source "$(dirname "$0")/lib.sh"
read T D P < "$RUN/ids"
start_manager || exit 1; login
catalog() { python3 -c "import sqlite3,sys; c=sqlite3.connect('$RUN/data/catalog.db', timeout=20); r=c.execute(sys.argv[1]).fetchall(); c.commit(); print('\n'.join('|'.join(str(x) for x in row) for row in r))" "$1"; }
plan() { new_plan "{\"name\":\"$1\",\"targetId\":$T,\"destinationId\":$D,\"spec\":$2,\"schedule\":\"${3:-}\",\"keepDays\":${4:-0},\"keepLast\":${5:-0},\"enabled\":true}"; }
objects() { call GET "/backups/$1" | js "sorted(set(($2) for o in d['manifest']['objects']))"; }

echo "== partial backups"
PT=$(plan tables-only '{"content":"all","tables":["shop.accounts","shop.st*"]}')
B=$(run_plan $PT); eq "chosen tables: finished" "$(wait_backup $B)" done
eq "chosen tables: only those tables" "$(objects $B 'o["db"]+"."+o.get("table","")')" "['shop.accounts', 'shop.stats']"
PS=$(plan schema-only '{"content":"schema","databases":["shop"]}')
B=$(run_plan $PS); eq "schema only: finished" "$(wait_backup $B)" done
eq "schema only: no row files" "$(objects $B 'o["kind"]')" "['routines', 'schema', 'triggers', 'views']"
eq "schema only: zero rows" "$(call GET /backups/$B | js 'sum(o.get("rows",0) for o in d["manifest"]["objects"] if o["kind"]=="data")')" 0
curl -sS -b "$JAR" -o "$RUN/schema.sql" "$URL/api/backups/$B/download?nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_schema; CREATE DATABASE restore_schema CHARACTER SET utf8mb4'
check "schema only: loads into an empty database" bash -c "docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw restore_schema < '$RUN/schema.sql'"
eq "schema only: all tables created, all empty" "$(sql -e "SELECT CONCAT(COUNT(*),'/',COALESCE(SUM(table_rows),0)) FROM information_schema.tables WHERE table_schema='restore_schema' AND table_type='BASE TABLE'")" "8/0"
PD=$(plan rows-only '{"content":"data","tables":["shop.accounts"],"where":{"shop.accounts":"id <= 10"},"noEncrypt":true}')
B=$(run_plan $PD); eq "rows only + row filter: finished" "$(wait_backup $B)" done
eq "rows only: no schema files" "$(objects $B 'o["kind"]')" "['data']"
eq "row filter kept 10 rows" "$(call GET /backups/$B | js 'd["backup"]["rows"]')" 10
eq "unencrypted plan is marked so" "$(call GET /backups/$B | js 'd["backup"]["encrypted"]')" False
PN=$(plan nothing '{"content":"all","tables":["shop.no_such_table"]}')
B=$(run_plan $PN); eq "no matching table: fails clearly, no retry" "$(wait_backup $B 30)/$(call GET /backups/$B | js '"nothing matches" in d["backup"]["error"]')" "failed/True"

echo "== list: pagination, filter, search, sort"
eq "page size is respected" "$(call GET '/backups?pageSize=2&page=1' | js 'len(d["items"])')" 2
TOTAL=$(call GET '/backups?pageSize=2' | js 'd["total"]')
eq "pages do not overlap" "$(python3 - <<PY
import json,subprocess
ids=[]
for p in range(1, ($TOTAL+1)//2+1):
    out=subprocess.run(['curl','-sS','-b','$JAR','$URL/api/backups?pageSize=2&page=%d'%p],capture_output=True,text=True).stdout
    ids+=[b['id'] for b in json.loads(out)['items']]
print(len(ids)==len(set(ids))==$TOTAL)
PY
)" True
eq "status filter" "$(call GET '/backups?status=failed' | js 'all(b["status"]=="failed" for b in d["items"]) and d["total"]>=1')" True
eq "search by plan name" "$(call GET '/backups?search=schema-only' | js 'd["total"]')" 1
eq "search treats % literally" "$(call GET '/backups?search=%25' | js 'd["total"]')" 0
eq "sort by size ascending" "$(call GET '/backups?sort=size&dir=asc&pageSize=50' | js '[b["storedBytes"] for b in d["items"]]==sorted(b["storedBytes"] for b in d["items"])')" True

echo "== manager killed in the middle of a backup"
PSLOW=$(plan slow-big '{"content":"all","tables":["shop.big"],"rateMB":1}')
B=$(run_plan $PSLOW)
for i in $(seq 1 40); do [ -n "$(find "$RUN/store/$B" -name '*.part' 2>/dev/null)" ] && break; sleep 0.5; done
check "backup is writing" bash -c "[ -n \"\$(find '$RUN/store/$B' -name '*.part')\" ]"
stop_manager -KILL
eq "after the kill the record still says running" "$(catalog "SELECT status FROM backups WHERE id='$B'")" running
start_manager; login
eq "after restart the same backup completes" "$(wait_backup $B 240)" done
eq "it took a second attempt" "$(catalog "SELECT attempt FROM jobs WHERE backup_id='$B'")" 2
check "no partial files left" bash -c "[ -z \"\$(find '$RUN/store' -name '*.part')\" ]"
curl -sS -b "$JAR" -o "$RUN/big.sql" "$URL/api/backups/$B/download?nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_big; CREATE DATABASE restore_big'; sql restore_big < "$RUN/big.sql"
eq "the recovered backup restores identical" "$(sql -e 'CHECKSUM TABLE restore_big.big' | cut -f2)" "$(sql -e 'CHECKSUM TABLE shop.big' | cut -f2)"

echo "== cancel a running backup"
B=$(run_plan $PSLOW); sleep 4
J=$(catalog "SELECT id FROM jobs WHERE backup_id='$B'")
call POST /jobs/$J/cancel -d '{}' >/dev/null
eq "cancelled" "$(wait_backup $B 30)" cancelled
sleep 1; check "cancelled backup left no files" bash -c "[ ! -e '$RUN/store/$B' ]"

echo "== one job per database server at a time"
B1=$(run_plan $PSLOW); B2=$(run_plan $PT); sleep 3
eq "second backup of the same server waits" "$(call GET /backups/$B1 | js 'd["backup"]["status"]')/$(call GET /backups/$B2 | js 'd["backup"]["status"]')" "running/queued"
eq "run now refuses a plan that is already running" "$(call POST /plans/$PSLOW/run -o /dev/null -w '%{http_code}' -d '{}')" 400

echo "== a table is altered while the backup runs"
J=$(catalog "SELECT id FROM jobs WHERE backup_id='$B1'"); call POST /jobs/$J/cancel -d '{}' >/dev/null; wait_backup $B1 30 >/dev/null; wait_backup $B2 60 >/dev/null
PDDL=$(plan ddl-race '{"content":"all","databases":["shop"],"rateMB":1}')
wait_big() { local i; for i in $(seq 1 80); do [ -n "$(find "$RUN/store/$1" -name 'big.data*' 2>/dev/null)" ] && return; sleep 0.5; done; }
alter_ms() { local t0=$(date +%s%N); sql -e "$1"; echo $(( ($(date +%s%N)-t0)/1000000 )); }
# (a) instant change on a table the backup has not reached yet: not blocked, backup stays self-consistent
B=$(run_plan $PDDL); wait_big $B
MS=$(alter_ms 'ALTER TABLE shop.stats ADD COLUMN extra INT NULL')
if [ $MS -lt 3000 ]; then ok "instant ALTER on a not-yet-saved table was not blocked (${MS} ms)"; else bad "the ALTER waited ${MS} ms"; fi
eq "backup finished" "$(wait_backup $B 400)" done
curl -sS -b "$JAR" -o "$RUN/ddl.sql" "$URL/api/backups/$B/download?db=shop&table=stats&nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_ddl; CREATE DATABASE restore_ddl'
check "structure and rows of the altered table match (it restores)" bash -c "docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw restore_ddl < '$RUN/ddl.sql'"
# (b) a rebuilding change: the snapshot can no longer read the table, so the backup must retry, not save garbage
B=$(run_plan $PDDL); wait_big $B
MS=$(alter_ms 'ALTER TABLE shop.stats DROP COLUMN extra, ALGORITHM=COPY')
if [ $MS -lt 3000 ]; then ok "rebuilding ALTER on a not-yet-saved table was not blocked (${MS} ms)"; else bad "the ALTER waited ${MS} ms"; fi
eq "the backup noticed, retried and finished" "$(wait_backup $B 400)" done
eq "a retry was needed" "$(catalog "SELECT attempt FROM jobs WHERE backup_id='$B'")" 2
eq "final backup has the final structure" "$(call GET /backups/$B | js 'any("extra" in o.get("ddl","") for o in d["manifest"]["objects"] if o.get("table")=="stats")')" False

echo "== verify and tamper detection"
B=$(run_plan $PT); wait_backup $B >/dev/null
call POST /backups/$B/verify -d '{}' >/dev/null; sleep 3
eq "good backup verifies" "$(call GET /backups/$B | js '(d["backup"]["verifiedAt"]>0, d["backup"]["verifyError"])')" "(True, '')"
F=$(find "$RUN/store/$B" -name 'accounts.data*'); python3 -c "import sys; f=open(sys.argv[1],'r+b'); f.seek(60); b=f.read(1); f.seek(60); f.write(bytes([b[0]^1])); f.close()" "$F"
call POST /backups/$B/verify -d '{}' >/dev/null; sleep 3
eq "one flipped bit is detected" "$(call GET /backups/$B | js 'd["backup"]["verifyError"]!=""')" True
curl -sS -b "$JAR" -o "$RUN/tampered.sql" "$URL/api/backups/$B/download" 2>/dev/null; RC=$?
if [ $RC -ne 0 ] || grep -q 'EXPORT FAILED' "$RUN/tampered.sql"; then ok "download of a damaged backup fails loudly"; else bad "damaged backup downloaded as if fine"; fi
eq "raw download refuses files outside the backup" "$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' "$URL/api/backups/$B/download?raw=../../data/catalog.db")" 400
RAW=$(call GET /backups/$B | js 'd["manifest"]["objects"][0]["key"]')
eq "raw download supports resume (Range)" "$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' -H 'Range: bytes=10-' "$URL/api/backups/$B/download?raw=$RAW")" 206

echo "== retention"
PR=$(plan keep-two '{"content":"all","tables":["shop.stats"]}' "" 0 2)
for i in 1 2 3 4; do B=$(run_plan $PR); wait_backup $B >/dev/null; IDS[$i]=$B; sleep 1.1; done
sleep 1
eq "only the newest 2 are kept" "$(catalog "SELECT COUNT(*) FROM backups WHERE plan_id=$PR AND deleted_at=0")" 2
check "files of removed backups are gone" bash -c "[ ! -e '$RUN/store/${IDS[1]}' ] && [ ! -e '$RUN/store/${IDS[2]}' ] && [ -e '$RUN/store/${IDS[4]}' ]"
eq "the reason is recorded" "$(catalog "SELECT COUNT(*) FROM backups WHERE plan_id=$PR AND delete_reason LIKE 'retention%'")" 2
call POST /backups/${IDS[3]}/pin -d '{"pinned":true}' >/dev/null
for i in 5 6; do B=$(run_plan $PR); wait_backup $B >/dev/null; IDS[$i]=$B; sleep 1.1; done; sleep 1
check "a pinned backup survives retention" bash -c "[ -e '$RUN/store/${IDS[3]}' ]"
eq "pinned backup cannot be deleted by hand either" "$(call DELETE /backups/${IDS[3]} -o /dev/null -w '%{http_code}')" 400

echo "== hold / release, schedule"
"$BIN" hold >/dev/null
B=$(run_plan $PT); sleep 4
eq "during a hold the backup waits" "$(call GET /backups/$B | js 'd["backup"]["status"]')" queued
"$BIN" release >/dev/null
eq "after release it runs" "$(wait_backup $B 30)" done
PSCH=$(plan scheduled '{"content":"schema","tables":["shop.stats"]}' "@every 6h")
eq "a scheduled plan gets a next run time" "$(call GET /plans | js "[p['nextRunAt']>0 for p in d if p['id']==$PSCH][0]")" True
eq "a too-frequent schedule is refused" "$(call POST /plans -o /dev/null -w '%{http_code}' -d "{\"name\":\"x\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{\"content\":\"all\"},\"schedule\":\"* * * * *\"}")" 400
catalog "UPDATE plans SET next_run_at=1 WHERE id=$PSCH" >/dev/null
for i in $(seq 1 50); do N=$(catalog "SELECT COUNT(*) FROM backups WHERE plan_id=$PSCH AND trigger='schedule' AND status='done'"); [ "$N" -ge 1 ] && break; sleep 0.5; done
eq "the scheduler started the due plan once" "$N" 1
eq "and moved the next run into the future" "$(catalog "SELECT next_run_at > strftime('%s','now') FROM plans WHERE id=$PSCH")" 1

echo "== activity log"
eq "downloads and deletions are logged" "$(call GET /audit | js 'len({a["action"] for a in d["items"]} & {"backup.download","backup.delete","backup.start","login"})>=3')" True
stop_manager
echo; echo "02: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
