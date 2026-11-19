#!/usr/bin/env bash
# Full backup while the database is being written to, then restore and compare.
source "$(dirname "$0")/lib.sh"
# keep .run/restore: the test database has it mounted
mkdir -p "$RUN/restore"; find "$RUN" -mindepth 1 -maxdepth 1 ! -name restore -exec rm -rf {} +
echo "== start, first account, access rules"
start_manager || exit 1
eq "API refuses anonymous callers" "$(curl -s -o /dev/null -w '%{http_code}' $URL/api/backups)" 401
LINK=$("$BIN" setup-link); TOKEN="${LINK##*=}"
eq "setup refuses a wrong code" "$(call POST /setup -o /dev/null -w '%{http_code}' -d '{"token":"nope","email":"a@b.co","password":"correct-horse-9"}')" 403
eq "setup refuses a short password" "$(call POST /setup -o /dev/null -w '%{http_code}' -d "{\"token\":\"$TOKEN\",\"email\":\"a@b.co\",\"password\":\"short\"}")" 400
eq "setup creates the owner" "$(call POST /setup -o /dev/null -w '%{http_code}' -d "{\"token\":\"$TOKEN\",\"email\":\"owner@example.com\",\"password\":\"correct-horse-9\"}")" 200
eq "setup cannot be run twice" "$(call POST /setup -o /dev/null -w '%{http_code}' -d "{\"token\":\"$TOKEN\",\"email\":\"x@b.co\",\"password\":\"correct-horse-9\"}")" 409
eq "changing request without the CSRF header is refused" "$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' -X POST $URL/api/hold -d '{"on":true}')" 403
eq "wrong password is refused" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-BM: 1' $URL/api/login -d '{"email":"owner@example.com","password":"wrong-password"}')" 401
eq "panel page is served" "$(curl -s -o /dev/null -w '%{http_code}' $URL/)" 200

echo "== target, destination, plan"
T=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup"},"secret":"bm-backup-pw"}' | js 'd["id"]')
D=$(call POST /destinations -d "{\"name\":\"local\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
eq "target test connects" "$(call POST /targets/$T/test -d '{}' | js 'd["ok"]')" True
eq "destination test writes" "$(call POST /destinations/$D/test -d '{}' | js 'd["ok"]')" True
check "password is not readable in the catalog file" bash -c "! grep -a -q 'bm-backup-pw' '$RUN/data/catalog.db'"
eq "API never returns the password" "$(call GET /targets | grep -c 'bm-backup-pw')" 0
eq "table list for the picker" "$(call GET /targets/$T/tables | js 'len([t for t in d if t["db"]=="shop"])')" 9
P=$(new_plan "{\"name\":\"full\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{\"content\":\"all\"},\"schedule\":\"\",\"enabled\":true}")
echo "$T $D $P" > "$RUN/ids"

echo "== write load: money transfers in transactions"
start_load

n0=$(sql -e 'SELECT n FROM shop.stats'); sleep 6; n1=$(sql -e 'SELECT n FROM shop.stats')
BEFORE=$(( (n1-n0)/6 )); echo "  transfers per second before the backup: $BEFORE"

echo "== full backup during the load"
B=$(run_plan $P); echo "$B" > "$RUN/full_id"
m0=$(sql -e 'SELECT n FROM shop.stats'); t0=$(date +%s)
STATUS=$(wait_backup $B 300)
m1=$(sql -e 'SELECT n FROM shop.stats'); t1=$(date +%s)
eq "backup finished" "$STATUS" done
DURING=$(( (m1-m0) / ( (t1-t0) > 0 ? (t1-t0) : 1 ) )); echo "  transfers per second during the backup: $DURING (took $((t1-t0))s)"
if [ "$DURING" -gt 0 ]; then ok "writes kept going during the backup"; else bad "writes stopped during the backup"; fi
LOCKS=$(sql -e "SELECT COUNT(*) FROM information_schema.processlist WHERE state LIKE '%lock%'")
call GET /backups/$B > "$RUN/full.json"
python3 - "$RUN/full.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1])); b=d["backup"]; m=d["manifest"]
print("  %d tables, %d rows, %.1f MB of SQL -> %.1f MB stored, encrypted=%s" % (b["tables"], b["rows"], b["rawBytes"]/1e6, b["storedBytes"]/1e6, b["encrypted"]))
print("  position:", m["position"])
PY
eq "manifest has the binary log position" "$(js 'bool(d["manifest"]["position"].get("gtid_binlog_pos") or d["manifest"]["position"].get("binlog_snapshot_file"))' < "$RUN/full.json")" True
check "stored files are encrypted (no table text inside)" bash -c "! grep -r -a -q -E 'CREATE TABLE|INSERT INTO' '$RUN/store'"
check "no partial files left" bash -c "[ -z \"\$(find '$RUN/store' -name '*.part')\" ]"

echo "== restore into a new database and compare"
curl -sS -b "$JAR" -o "$RUN/shop.sql" -w '' "$URL/api/backups/$B/download?db=shop&nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_shop; CREATE DATABASE restore_shop CHARACTER SET utf8mb4'
if sql restore_shop < "$RUN/shop.sql" 2>"$RUN/restore.err"; then ok "the downloaded .sql loads without errors"; else bad "restore failed: $(head -3 "$RUN/restore.err")"; fi
# consistency: the backup was taken while money moved, so these must hold exactly in the copy
eq "money total is intact in the copy" "$(sql -e 'SELECT SUM(balance) FROM restore_shop.accounts')" 2000000.00
eq "ledger rows = transfer counter (one consistent moment)" "$(sql -e 'SELECT (SELECT COUNT(*) FROM restore_shop.ledger) = (SELECT n FROM restore_shop.stats)')" 1
eq "trigger table matches the ledger" "$(sql -e 'SELECT (SELECT COUNT(*) FROM restore_shop.audit_trail) = (SELECT COUNT(*) FROM restore_shop.ledger)')" 1
eq "ledger debits match balances" "$(sql -e 'SELECT COUNT(*) FROM (SELECT a.id FROM restore_shop.accounts a LEFT JOIN (SELECT id, SUM(x) s FROM (SELECT from_id id, -amount x FROM restore_shop.ledger UNION ALL SELECT to_id, amount FROM restore_shop.ledger) u GROUP BY id) l ON l.id=a.id WHERE a.balance <> 1000 + COALESCE(l.s,0)) bad')" 0
# static tables must be bit-identical
for t in 'odd types' big legacy_myisam empty_table; do
  eq "table '$t' is identical" "$(sql -e "CHECKSUM TABLE restore_shop.\`$t\`" | cut -f2)" "$(sql -e "CHECKSUM TABLE shop.\`$t\`" | cut -f2)"
done
eq "generated columns recomputed" "$(sql -e 'SELECT CONCAT(gen,"/",gens) FROM restore_shop.`odd types` WHERE id=1')" "-256/-127"
eq "view restored" "$(sql -e "SELECT COUNT(*) FROM information_schema.views WHERE table_schema='restore_shop'")" 1
eq "trigger restored" "$(sql -e "SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='restore_shop'")" 1
eq "routines restored" "$(sql -e "SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='restore_shop'")" 3
eq "restored function works" "$(sql -e 'SELECT restore_shop.double_it(21)')" 42
eq "auto-increment continues" "$(sql -e "SELECT (SELECT AUTO_INCREMENT FROM information_schema.tables WHERE table_schema='restore_shop' AND table_name='ledger') > (SELECT MAX(id) FROM restore_shop.ledger)")" 1

echo "== single table download"
curl -sS -b "$JAR" -o "$RUN/accounts.sql" "$URL/api/backups/$B/download?db=shop&table=accounts&nodb=1"
eq "one-table file has only that table" "$(grep -c '^CREATE TABLE' "$RUN/accounts.sql")" 1
eq "rows-only part has no CREATE" "$(curl -sS -b "$JAR" "$URL/api/backups/$B/download?db=shop&table=accounts&part=data" | grep -c 'CREATE TABLE')" 0
stop_load; stop_manager
echo; echo "01: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
