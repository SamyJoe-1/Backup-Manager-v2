#!/usr/bin/env bash
# Look inside backups: sandbox, row search, CSV, single-table restore, restore tests (logical and physical).
# Standalone: creates its own account. Needs Docker and the test database (see README.md).
source "$(dirname "$0")/lib.sh"
mkdir -p "$RUN"; rm -rf "$RUN/data" "$RUN/keys" "$RUN/store"
for c in $(docker ps -aq --filter label=bm-sandbox=1); do docker rm -fv "$c" >/dev/null; done
catalog() { python3 -c "import sqlite3,sys; c=sqlite3.connect('$RUN/data/catalog.db', timeout=20); r=c.execute(sys.argv[1]).fetchall(); c.commit(); print('\n'.join('|'.join(str(x) for x in row) for row in r))" "$1"; }
wait_job() { local s="" i; for i in $(seq 1 $(( ${2:-120} * 2 ))); do s=$(catalog "SELECT state FROM jobs WHERE id=$1"); case "$s" in done|failed|cancelled) echo "$s"; return;; esac; sleep 0.5; done; echo "timeout:$s"; }
last_job() { catalog "SELECT id FROM jobs WHERE kind='$1' ORDER BY id DESC LIMIT 1"; }
job_error() { catalog "SELECT error FROM jobs WHERE id=$1"; }
rows() { local b="$1"; shift; curl -sS -G -b "$JAR" "$URL/api/backups/$b/rows" "$@"; }
explore() { call POST "/backups/$1/explore" -d "{\"db\":\"$2\",\"table\":\"$3\"}"; }
load() { local r; r=$(explore "$@"); [ "$(echo "$r" | js 'd["ready"]')" = True ] && { echo ready; return; }; wait_job "$(echo "$r" | js 'd["jobId"]')" 240; }
sandboxes() { docker ps --filter label=bm-sandbox=1 --format '{{.Names}}' | grep -c "$1"; }

start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
catalog "INSERT INTO settings(k,v) VALUES('sandbox_memory','300m')" >/dev/null
T=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup","exec":"docker:bm-test-db"},"secret":"bm-backup-pw"}' | js 'd["id"]')
D=$(call POST /destinations -d "{\"name\":\"local\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
plan() { call POST /plans -d "{\"name\":\"$1\",\"targetId\":$T,\"destinationId\":$D,\"spec\":$2,\"schedule\":\"\",\"enabled\":true}" | js 'd["id"]'; }
start_load; sleep 3
PL=$(plan logical '{"content":"all","databases":["shop"]}')
B=$(run_plan $PL); eq "logical backup taken under load" "$(wait_backup $B 300)" done
stop_load
LEDGER_ROWS=$(call GET /backups/$B | js '[o["rows"] for o in d["manifest"]["objects"] if o.get("table")=="ledger" and o["kind"]=="data"][0]')

echo "== sandbox and row search"
eq "rows of a table that is not loaded are refused" "$(rows $B -o /dev/null -w '%{http_code}' -d db=shop -d table=accounts)" 400
eq "loading a table into the sandbox" "$(load $B shop accounts)" done
SB="bm-sandbox-explore-$(echo $B | tr 'A-Z' 'a-z')"
eq "one sandbox container exists" "$(sandboxes "$SB")" 1
eq "sandbox listens on this machine only" "$(docker port $SB 3306/tcp | cut -d: -f1)" 127.0.0.1
eq "sandbox has a memory limit" "$(docker inspect -f '{{.HostConfig.Memory}}' $SB)" 314572800
eq "all rows are there" "$(rows $B -d db=shop -d table=accounts | js 'd["total"]')" 2000
eq "page 2 of 10 starts at the 11th row, in primary-key order" "$(rows $B -d db=shop -d table=accounts -d page=2 -d pageSize=10 | js '(len(d["rows"]), d["rows"][0][0])')" "(10, '11')"
eq "sort by a column, descending" "$(rows $B -d db=shop -d table=accounts -d sort=id -d dir=desc -d pageSize=3 | js '[r[0] for r in d["rows"]]')" "['2000', '1999', '1998']"
eq "filter: id = 5" "$(rows $B -d db=shop -d table=accounts --data-urlencode 'filters=[{"column":"id","op":"eq","value":"5"}]' | js '(d["total"], d["rows"][0][0])')" "(1, '5')"
eq "two filters together" "$(rows $B -d db=shop -d table=accounts --data-urlencode 'filters=[{"column":"id","op":"ge","value":"1990"},{"column":"id","op":"lt","value":"1995"}]' | js 'd["total"]')" 5
eq "SQL typed into the search box is treated as text" "$(rows $B -d db=shop -d table=accounts --data-urlencode "search=' OR 1=1 -- " | js 'd["total"]')" 0
eq "an unknown column is refused" "$(rows $B -o /dev/null -w '%{http_code}' -d db=shop -d table=accounts --data-urlencode 'filters=[{"column":"id` OR 1=1 -- ","op":"eq","value":"1"}]')" 400
eq "a table name with a space loads too" "$(load $B shop 'odd types')" done
eq "the second table reuses the same sandbox" "$(sandboxes bm-sandbox-explore)" 1
eq "text search finds the emoji row" "$(rows $B -d db=shop --data-urlencode 'table=odd types' --data-urlencode 'search=emoji' | js 'd["total"]')" 1
eq "NULL and binary cells are shown as such" "$(rows $B -d db=shop --data-urlencode 'table=odd types' --data-urlencode 'filters=[{"column":"id","op":"eq","value":"1"}]' | js '(d["rows"][0][[c["name"] for c in d["columns"]].index("bin")], [r for r in [1]] and None)')" "('0x00ff2700', None)"
eq "is-NULL filter" "$(rows $B -d db=shop --data-urlencode 'table=odd types' --data-urlencode 'filters=[{"column":"t","op":"null","value":""}]' | js 'd["total"]')" 1
rows $B -o "$RUN/accounts.csv" -d db=shop -d table=accounts -d format=csv --data-urlencode 'filters=[{"column":"id","op":"le","value":"100"}]'
eq "CSV export has the header and the filtered rows" "$(wc -l < "$RUN/accounts.csv")/$(head -c 3 "$RUN/accounts.csv" | od -An -tx1 | tr -d ' \n')/$(sed -n 1p "$RUN/accounts.csv" | tr -d '\357\273\277\r')" "101/efbbbf/id,balance,updated_at"
eq "the sandbox account used for searching cannot write" "$(docker exec $SB sh -c 'mariadb -ubm_read -p"$(grep -o "bm_read.*BY .[0-9a-f]*" /tmp/bm-init.sql | grep -o "[0-9a-f]*$")" -e "DELETE FROM shop.accounts" 2>&1' | grep -c 'command denied')" 1
eq "sandbox state lists the loaded tables" "$(call GET /backups/$B/explore | js 'sorted(t["name"] for t in d["tables"])')" "['accounts', 'odd types']"

echo "== restore one table under a new name"
RT() { local d="$1"; shift; call POST /backups/${RB:-$B}/restore-table -d "$d" "$@"; }
eq "a bad new name is refused" "$(RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"root\",\"password\":\"x\",\"toTable\":\"bad name;\"}" -o /dev/null -w '%{http_code}')" 400
RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"root\",\"password\":\"wrong\",\"toTable\":\"ledger_copy\"}" >/dev/null
eq "wrong password: fails, creates nothing" "$(wait_job $(last_job table-restore) 60)/$(sql -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_name='ledger_copy'")" "failed/0"
RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"bm_backup\",\"password\":\"bm-backup-pw\",\"toTable\":\"ledger_copy\"}" >/dev/null
eq "read-only user: fails, creates nothing" "$(wait_job $(last_job table-restore) 60)/$(sql -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_name='ledger_copy'")" "failed/0"
BEFORE=$(sql -e 'CHECKSUM TABLE shop.ledger' | cut -f2)
RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"root\",\"password\":\"bmtest-root-pw\",\"toTable\":\"ledger_copy\"}" >/dev/null
J=$(last_job table-restore); eq "table restored as a new table" "$(wait_job $J 240)" done
eq "the copy has exactly the rows the backup recorded" "$(sql -e 'SELECT COUNT(*) FROM shop.ledger_copy')" "$LEDGER_ROWS"
eq "the original table was not touched" "$(sql -e 'CHECKSUM TABLE shop.ledger' | cut -f2)" "$BEFORE"
eq "foreign key came along under a new name" "$(sql -e "SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema='shop' AND table_name='ledger_copy' AND constraint_type='FOREIGN KEY' AND constraint_name LIKE 'fk_from_r%'")" 1
eq "the password is not kept in the job record" "$(catalog "SELECT payload LIKE '%secret%' OR payload LIKE '%bmtest-root-pw%' FROM jobs WHERE id=$J")" 0
RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"root\",\"password\":\"bmtest-root-pw\",\"toTable\":\"ledger_copy\"}" >/dev/null
eq "an existing name is refused and the existing table survives" "$(wait_job $(last_job table-restore) 60)/$(sql -e 'SELECT COUNT(*) FROM shop.ledger_copy')" "failed/$LEDGER_ROWS"
RT "{\"db\":\"shop\",\"table\":\"odd types\",\"targetId\":$T,\"user\":\"root\",\"password\":\"bmtest-root-pw\",\"toDb\":\"other\",\"toTable\":\"odd_copy\"}" >/dev/null
eq "tricky table restored into another database" "$(wait_job $(last_job table-restore) 120)" done
eq "tricky table copy is identical to the source" "$(sql -e 'CHECKSUM TABLE other.odd_copy' | cut -f2)" "$(sql -e 'CHECKSUM TABLE shop.`odd types`' | cut -f2)"
sql -e 'DROP TABLE shop.ledger_copy, other.odd_copy'

echo "== restore tests"
call POST /backups/$B/test -d '{}' >/dev/null; J=$(last_job restore-test)
eq "restore test of the logical backup passes" "$(wait_job $J 400)" done
eq "result recorded on the backup" "$(call GET /backups/$B | js '(d["backup"]["testedAt"]>0, d["backup"]["testError"], "rows counted and matching" in d["backup"]["testDetail"])')" "(True, '', True)"
eq "the test sandbox was removed" "$(sandboxes bm-sandbox-test)" 0
PT=$(plan auto-test '{"content":"all","tables":["shop.accounts","shop.stats"],"testEvery":1}')
B2=$(run_plan $PT); wait_backup $B2 120 >/dev/null; sleep 1
eq "a plan can test every backup by itself" "$(wait_job $(catalog "SELECT id FROM jobs WHERE kind='restore-test' AND backup_id='$B2'") 240)/$(call GET /backups/$B2 | js 'd["backup"]["testError"]')" "done/"
F=$(find "$RUN/store/$B2" -name 'accounts.schema*'); python3 -c "import sys; f=open(sys.argv[1],'r+b'); f.seek(40); b=f.read(1); f.seek(40); f.write(bytes([b[0]^1])); f.close()" "$F"
call POST /backups/$B2/test -d '{}' >/dev/null
eq "a damaged backup fails its restore test" "$(wait_job $(last_job restore-test) 240)/$(call GET /backups/$B2 | js 'd["backup"]["testError"]!=""')" "failed/True"
eq "failed test leaves no sandbox behind" "$(sandboxes bm-sandbox-test)" 0

echo "== physical backup: restore test and row search"
PP=$(plan physical '{"method":"physical","incrementalsPerFull":1}')
PF=$(run_plan $PP); eq "physical full" "$(wait_backup $PF 300)" done
start_load; sleep 3; stop_load
PI=$(run_plan $PP); eq "physical incremental" "$(wait_backup $PI 300)" done
N_SRC=$(sql -e 'SELECT n FROM shop.stats')
call POST /backups/$PI/test -d '{}' >/dev/null
eq "restore test of the incremental (full + changes) passes" "$(wait_job $(last_job restore-test) 500)" done
eq "it started a server and checked the tables" "$(call GET /backups/$PI | js '"tables checked" in d["backup"]["testDetail"]')" True
eq "single-table restore from a physical backup is refused with the way out" "$(RB=$PI RT "{\"db\":\"shop\",\"table\":\"ledger\",\"targetId\":$T,\"user\":\"root\",\"password\":\"bmtest-root-pw\",\"toTable\":\"x1\"}" >/dev/null; wait_job $(last_job table-restore) 60; job_error $(last_job table-restore) | grep -c 'sandbox')" "failed
1"
eq "open the physical backup in the sandbox" "$(load $PI '' '')" done
eq "its tables can be listed" "$(call GET /backups/$PI/explore | js '"big" in [t["name"] for t in d["tables"]]')" True
eq "search one row out of 400,000" "$(rows $PI -d db=shop -d table=big --data-urlencode 'filters=[{"column":"id","op":"eq","value":"123456"}]' | js 'd["rows"][0][2]')" "$(sql -e 'SELECT n FROM shop.big WHERE id=123456')"
eq "the copy holds the changes of the incremental" "$(rows $PI -d db=shop -d table=stats | js 'd["rows"][0][1]')" "$N_SRC"

echo "== clean-up rules"
eq "closing a sandbox removes its container" "$(call DELETE /backups/$PI/explore >/dev/null; sandboxes "explore-$(echo $PI | tr 'A-Z' 'a-z')")" 0
catalog "UPDATE sandboxes SET last_used=1" >/dev/null
for i in $(seq 1 90); do [ "$(sandboxes bm-sandbox-explore)" = 0 ] && break; sleep 1; done
eq "an idle sandbox is removed by itself" "$(sandboxes bm-sandbox-explore)/$(catalog 'SELECT COUNT(*) FROM sandboxes')" "0/0"
eq "rows are refused again once the sandbox is gone" "$(rows $B -o /dev/null -w '%{http_code}' -d db=shop -d table=accounts)" 400
eq "database still healthy" "$(sql -e 'SELECT SUM(balance) FROM shop.accounts')" 2000000.00
stop_manager
echo; echo "05: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
