#!/usr/bin/env bash
# Physical hot backups, incrementals, binary-log archive and point-in-time restore.
# Standalone: creates its own account. The test database must be started with -v "$PWD/.run/restore:/restore".
source "$(dirname "$0")/lib.sh"
RESTORE="$RUN/restore"; mkdir -p "$RUN"
catalog() { python3 -c "import sqlite3,sys; c=sqlite3.connect('$RUN/data/catalog.db', timeout=20); r=c.execute(sys.argv[1]).fetchall(); c.commit(); print('\n'.join('|'.join(str(x) for x in row) for row in r))" "$1"; }
field() { call GET "/backups/$1" | js "d['backup']['$2']"; }
wait_job() { local s="" i; for i in $(seq 1 $(( ${2:-120} * 2 ))); do s=$(catalog "SELECT state FROM jobs WHERE id=$1"); case "$s" in done|failed|cancelled) echo "$s"; return;; esac; sleep 0.5; done; echo "timeout:$s"; }
last_job() { catalog "SELECT id FROM jobs WHERE kind='$1' ORDER BY id DESC LIMIT 1"; }
# start a second server on a restored folder and run SQL on it
restored_up() { docker rm -f bm-test-restore >/dev/null 2>&1; docker exec bm-test-db chown -R mysql:mysql "/restore/$1"
  docker run -d --name bm-test-restore --memory=320m --memory-swap=320m --cpus=0.8 -v "$RESTORE/$1:/var/lib/mysql" mariadb:10.11 --innodb-buffer-pool-size=64M --performance-schema=OFF --skip-name-resolve --skip-log-bin >/dev/null
  local i; for i in $(seq 1 60); do docker exec bm-test-restore mariadb -uroot -pbmtest-root-pw -e 'select 1' >/dev/null 2>&1 && return 0; sleep 1; done; docker logs bm-test-restore 2>&1 | tail -5; return 1; }
rsql() { docker exec -i bm-test-restore mariadb -uroot -pbmtest-root-pw -N -B "$@"; }
consistent() { rsql -e 'SELECT CONCAT(SUM(balance), "/", (SELECT COUNT(*) FROM shop.ledger) = (SELECT n FROM shop.stats), "/", (SELECT COUNT(*) FROM shop.audit_trail) = (SELECT COUNT(*) FROM shop.ledger)) FROM shop.accounts'; }

rm -rf "$RUN/data" "$RUN/keys" "$RUN/store"; docker exec bm-test-db sh -c 'rm -rf /restore/*'
start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
T0=$(call POST /targets -d '{"name":"no-tools","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup"},"secret":"bm-backup-pw"}' | js 'd["id"]')
T=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup","exec":"docker:bm-test-db"},"secret":"bm-backup-pw"}' | js 'd["id"]')
D=$(call POST /destinations -d "{\"name\":\"local\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
plan() { call POST /plans -d "{\"name\":\"$1\",\"targetId\":${3:-$T},\"destinationId\":$D,\"spec\":$2,\"schedule\":\"\",\"keepLast\":${4:-0},\"enabled\":true}"; }

echo "== plan rules"
eq "physical plan needs a place to run the tools" "$(plan x '{"method":"physical"}' $T0 | js '"run the backup tools" in d.get("error","")')" True
eq "physical plan cannot choose tables" "$(plan x '{"method":"physical","tables":["shop.big"]}' | js '"whole server" in d.get("error","")')" True
PP=$(plan physical '{"method":"physical","incrementalsPerFull":2}' | js 'd["id"]')
PB=$(plan binlogs '{"method":"binlog"}' | js 'd["id"]')

echo "== full physical backup during write load"
start_load
n0=$(sql -e 'SELECT n FROM shop.stats'); t0=$(date +%s)
F=$(run_plan $PP); eq "full backup finished" "$(wait_backup $F 300)" done
n1=$(sql -e 'SELECT n FROM shop.stats'); t1=$(date +%s)
if [ "$n1" -gt "$n0" ]; then ok "writes kept going during the backup ($(( (n1-n0)/((t1-t0)>0?(t1-t0):1) )) transfers/s, took $((t1-t0))s)"; else bad "writes stopped"; fi
eq "it is a full physical backup" "$(field $F method)/$(field $F parentId)" "physical/"
eq "binary log position recorded" "$(call GET /backups/$F | js 'bool(d["manifest"]["position"].get("binlog_file")) and int(d["manifest"]["position"]["binlog_position"])>0')" True
check "stored file is encrypted (no InnoDB page text)" bash -c "! grep -r -a -q 'shop/accounts' '$RUN/store/$F'"
check "tool left nothing behind in the container" bash -c "[ -z \"\$(docker exec bm-test-db sh -c 'ls -d /tmp/bm-* 2>/dev/null')\" ]"
eq "SQL download of a physical backup is refused clearly" "$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' "$URL/api/backups/$F/download")" 400

echo "== incrementals"
sleep 3; I1=$(run_plan $PP); eq "second run finished" "$(wait_backup $I1 300)" done
eq "second run is an incremental on the full" "$(field $I1 method)/$(field $I1 parentId)" "incremental/$F"
sleep 3; I2=$(run_plan $PP); eq "third run finished" "$(wait_backup $I2 300)" done
eq "third run builds on the second" "$(field $I2 method)/$(field $I2 parentId)" "incremental/$I1"
FS=$(field $F storedBytes); IS=$(field $I1 storedBytes)
if [ "$IS" -lt "$FS" ]; then ok "incremental is smaller than the full ($(( IS/1024 )) KB vs $(( FS/1024 )) KB)"; else bad "incremental ($IS) not smaller than full ($FS)"; fi
eq "a full that incrementals need cannot be deleted" "$(call DELETE /backups/$F -o /dev/null -w '%{http_code}')" 400
stop_load
SRC_BIG=$(sql -e 'CHECKSUM TABLE shop.big' | cut -f2); SRC_ODD=$(sql -e 'CHECKSUM TABLE shop.`odd types`' | cut -f2)

echo "== restore the chain (full + 2 incrementals) into a new folder"
eq "restore refuses a relative folder" "$(call POST /backups/$I2/restore -o /dev/null -w '%{http_code}' -d '{"dir":"restore/a"}')" 400
call POST /backups/$I2/restore -d '{"dir":"/restore/a"}' >/dev/null
eq "restore job finished" "$(wait_job $(last_job restore) 300)" done
check "restore note written" bash -c "grep -q '3 step' '$RESTORE/a/BM-RESTORE.txt'"
call POST /backups/$I2/restore -d '{"dir":"/restore/a"}' >/dev/null
eq "restore refuses a folder that is not empty" "$(wait_job $(last_job restore) 60)/$(catalog "SELECT error LIKE '%not empty%' FROM jobs WHERE id=$(last_job restore)")" "failed/1"
if restored_up a; then ok "a server starts on the restored folder"; else bad "server did not start on the restored folder"; fi
eq "restored copy is consistent (money, ledger, trigger table)" "$(consistent)" "2000000.00/1/1"
eq "big table identical" "$(rsql -e 'CHECKSUM TABLE shop.big' | cut -f2)" "$SRC_BIG"
eq "odd-types table identical" "$(rsql -e 'CHECKSUM TABLE shop.`odd types`' | cut -f2)" "$SRC_ODD"
RN=$(rsql -e 'SELECT n FROM shop.stats'); SN=$(sql -e 'SELECT n FROM shop.stats')
if [ "$RN" -gt "$n1" ] && [ "$RN" -le "$SN" ]; then ok "copy contains the changes of the incrementals (counter $RN, full had <= $n1)"; else bad "counter $RN not between $n1 and $SN"; fi
eq "views, routines, users came along" "$(rsql -e "SELECT CONCAT((SELECT COUNT(*) FROM information_schema.views WHERE table_schema='shop'),'/',(SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='shop'),'/',(SELECT COUNT(*) FROM mysql.user WHERE user='bm_backup'))")" "1/3/1"
docker rm -f bm-test-restore >/dev/null

echo "== point-in-time restore"
start_load; sleep 4; stop_load; sleep 1
eq "write load really stopped" "$(sql -e 'SELECT n FROM shop.stats'; sleep 1)" "$(sql -e 'SELECT n FROM shop.stats')"
N_AT=$(sql -e 'SELECT n FROM shop.stats'); sleep 2; STOP=$(date +%s); sleep 2
start_load; sleep 4; stop_load
N_END=$(sql -e 'SELECT n FROM shop.stats')
sql -e 'DROP TABLE shop.empty_table'   # the "mistake" made after the moment we want back
call POST /backups/$I2/restore -d "{\"dir\":\"/restore/early\",\"stopAt\":$STOP}" >/dev/null
eq "without archived logs a point-in-time restore fails with the reason" "$(wait_job $(last_job restore) 120)/$(catalog "SELECT error LIKE '%not archived%' FROM jobs WHERE id=$(last_job restore)")" "failed/1"
docker exec bm-test-db rm -rf /restore/early
call POST /plans/$PB/run -d '{}' >/dev/null; eq "binary-log archive ran" "$(wait_job $(last_job binlog) 120)" done
NB=$(call GET /targets/$T/binlogs | js 'len(d)'); if [ "$NB" -ge 1 ]; then ok "$NB binary log file(s) archived"; else bad "no binary logs archived"; fi
check "archived logs are encrypted" bash -c "! grep -r -a -q 'accounts' '$RUN/store/binlog'"
eq "the open log is marked to be fetched again" "$(call GET /targets/$T/binlogs | js 'd[-1]["closed"]')" False
call POST /backups/$I2/restore -d "{\"dir\":\"/restore/pitr\",\"stopAt\":$STOP}" >/dev/null
eq "point-in-time restore prepared" "$(wait_job $(last_job restore) 300)" done
if restored_up pitr; then ok "server starts on the restored folder"; else bad "server did not start"; fi
REPLAY=$(grep '^TZ=UTC' "$RESTORE/pitr/BM-RESTORE.txt" | sed 's#/restore/pitr#/var/lib/mysql#; s#| mariadb"#| mariadb -uroot -pbmtest-root-pw"#')
if docker exec bm-test-restore sh -c "$REPLAY" 2>"$RUN/replay.err"; then ok "binary logs replay without errors"; else bad "replay failed: $(head -3 "$RUN/replay.err")"; fi
eq "rolled forward to exactly the chosen moment (counter $N_AT, later it reached $N_END)" "$(rsql -e 'SELECT n FROM shop.stats')" "$N_AT"
eq "still consistent after the replay" "$(consistent)" "2000000.00/1/1"
eq "the table dropped after that moment is back" "$(rsql -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='shop' AND table_name='empty_table'")" 1
docker rm -f bm-test-restore >/dev/null
sql -e 'CREATE TABLE shop.empty_table (id INT PRIMARY KEY, x TEXT) ENGINE=InnoDB'
call POST /plans/$PB/run -d '{}' >/dev/null; wait_job $(last_job binlog) 120 >/dev/null
eq "second archive run re-fetches only the open log" "$(call GET /targets/$T/binlogs | js 'len(d)')" "$NB"

echo "== next full, chain retention, cancel"
B4=$(run_plan $PP); eq "fourth run finished" "$(wait_backup $B4 300)" done
eq "after 2 incrementals the next run is a new full" "$(field $B4 method)" physical
catalog "UPDATE plans SET keep_last=1 WHERE id=$PP" >/dev/null
B5=$(run_plan $PP); wait_backup $B5 300 >/dev/null
eq "retention removed the old chain as a whole, kept the new one" "$(catalog "SELECT GROUP_CONCAT(deleted_at>0) FROM (SELECT deleted_at FROM backups WHERE plan_id=$PP ORDER BY created_at, id)")" "1,1,1,0,0"
check "old chain's files are gone" bash -c "[ ! -e '$RUN/store/$F' ] && [ ! -e '$RUN/store/$I2' ] && [ -e '$RUN/store/$B4' ]"
PSLOW=$(plan slow-physical '{"method":"physical","rateMB":1}' | js 'd["id"]')
BS=$(run_plan $PSLOW); sleep 6
eq "mariabackup is running in the container" "$(docker exec bm-test-db sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " " | grep -c -- "--stream=mbstrea[m]"')" 1
call POST /jobs/$(catalog "SELECT id FROM jobs WHERE backup_id='$BS'")/cancel -d '{}' >/dev/null
eq "cancelled" "$(wait_backup $BS 60)" cancelled
sleep 2
eq "cancel also stopped the tool inside the container" "$(docker exec bm-test-db sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " " | grep -c -- "--stream=mbstrea[m]"')" 0
check "cancelled backup left no files, here or in the container" bash -c "[ ! -e '$RUN/store/$BS' ] && [ -z \"\$(docker exec bm-test-db sh -c 'ls -d /tmp/bm-* 2>/dev/null')\" ]"
eq "database still healthy after everything" "$(sql -e 'SELECT SUM(balance) FROM shop.accounts')" 2000000.00
stop_manager
echo; echo "04: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
