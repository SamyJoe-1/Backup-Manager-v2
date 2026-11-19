#!/usr/bin/env bash
# The server running Backup Manager is lost. With only the master key and the backup destination,
# a new server reads every old backup. Standalone. Needs bm-test-db (see README.md).
source "$(dirname "$0")/lib.sh"
mkdir -p "$RUN"; rm -rf "$RUN/data" "$RUN/keys" "$RUN/store" "$RUN/new" "$RUN/out"
start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
T=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup"},"secret":"bm-backup-pw"}' | js 'd["id"]')
D=$(call POST /destinations -d "{\"name\":\"offsite\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
P=$(new_plan "{\"name\":\"shop\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{\"content\":\"all\",\"databases\":[\"shop\"]},\"schedule\":\"\",\"enabled\":true}")
B=$(run_plan $P); eq "a normal backup" "$(wait_backup $B 300)" done

echo "== protecting the manager itself"
"$BIN" catalog-backup "$RUN/data/catalog.backup.db" >/dev/null
eq "catalog-backup makes a consistent, owner-only copy" "$(python3 -c "import sqlite3; print(sqlite3.connect('$RUN/data/catalog.backup.db').execute('PRAGMA integrity_check').fetchone()[0])")/$(stat -c %a "$RUN/data/catalog.backup.db")" "ok/600"
TC=$(call POST /targets -d "{\"name\":\"manager catalog\",\"kind\":\"files\",\"cfg\":{\"paths\":[\"$RUN/data/catalog.backup.db\"]}}" | js 'd["id"]')
PC=$(new_plan "{\"name\":\"catalog\",\"targetId\":$TC,\"destinationId\":$D,\"spec\":{},\"schedule\":\"\",\"enabled\":true}")
BC=$(run_plan $PC); eq "a folder plan stores the catalog copy at the destination" "$(wait_backup $BC 60)" done
stop_manager
cp "$RUN/keys/master.key" "$RUN/master.key.offline"

echo "== the server is lost; a new one has only the key and the destination"
rm -rf "$RUN/data" "$RUN/keys"
NEWD="$RUN/new"; mkdir -p "$NEWD/data" "$NEWD/keys" "$RUN/out"; cp "$RUN/master.key.offline" "$NEWD/keys/master.key"
export BM_DATA_DIR="$NEWD/data" BM_MASTER_KEY_FILE="$NEWD/keys/master.key"
TAR=$(ls "$RUN/store/$BC"/files.tar.zst.enc)
(cd "$RUN/out" && "$BIN" decrypt --in "$TAR" >/dev/null)
eq "decrypt opens the catalog archive with only the key" "$(tar -tf "$RUN/out/files.tar" | grep -c 'catalog.backup.db$')" 1
tar -xf "$RUN/out/files.tar" -C "$RUN/out"; cp "$(find "$RUN/out" -name catalog.backup.db | head -1)" "$NEWD/data/catalog.db"
start_manager || exit 1; login
eq "the new server lists the old backups" "$(call GET "/backups?plan=$P" | js '[b["id"] for b in d["items"]]')" "['$B']"
curl -sS -b "$JAR" -o "$RUN/out/shop.sql" "$URL/api/backups/$B/download?db=shop&nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_dr; CREATE DATABASE restore_dr CHARACTER SET utf8mb4'
check "and its old backup restores" bash -c "docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw restore_dr < '$RUN/out/shop.sql'"
eq "with the same data" "$(sql -e 'CHECKSUM TABLE restore_dr.big' | cut -f2)" "$(sql -e 'CHECKSUM TABLE shop.big' | cut -f2)"
stop_manager

echo "== without even a catalog: one table straight from the destination"
F=$(ls "$RUN/store/$B"/shop/accounts.data.sql.zst.enc)
(cd "$RUN/out" && rm -f accounts.data.sql && "$BIN" decrypt --in "$F" >/dev/null)
eq "decrypt finds meta.json by itself and writes every row" "$(grep -c '^(' "$RUN/out/accounts.data.sql")" 2000
cp /dev/null "$NEWD/keys/master.key"; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$NEWD/keys/master.key"
has_err=$(cd "$RUN/out" && "$BIN" decrypt --in "$F" --out x.sql 2>&1)
eq "a different master key is refused clearly" "$(echo "$has_err" | grep -c 'does not open this backup')" 1
F2="$RUN/out/cut.enc"; head -c 2000 "$F" > "$F2"; cp "$RUN/store/$B/meta.json" "$RUN/out/meta.json"; cp "$RUN/master.key.offline" "$NEWD/keys/master.key"
eq "a cut file is refused, nothing half-written is left" "$(cd "$RUN/out" && "$BIN" decrypt --in cut.enc --out cut.sql 2>&1 | grep -c damaged)/$(ls "$RUN/out/cut.sql" 2>/dev/null | wc -l)" "1/0"
unset BM_DATA_DIR BM_MASTER_KEY_FILE
echo; echo "09: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
