#!/usr/bin/env bash
# S3-compatible storage: upload as a stream, read back, verify, resume download, delete. Needs 01 first and
#   docker run -d --name bm-test-minio --memory=300m -p 127.0.0.1:39000:9000 -e MINIO_ROOT_USER=bmtestaccess \
#     -e MINIO_ROOT_PASSWORD=bmtest-secret-key-123 --entrypoint sh pgsty/minio -c 'mkdir -p /data/bm-bucket && exec minio server /data'
source "$(dirname "$0")/lib.sh"
read T D P < "$RUN/ids"
start_manager || exit 1; login
S3='{"name":"s3","kind":"s3","cfg":{"endpoint":"127.0.0.1:39000","bucket":"bm-bucket","prefix":"prod/db","accessKey":"bmtestaccess","useSSL":false},"secret":"bmtest-secret-key-123"}'
DS=$(call POST /destinations -d "$S3" | js 'd["id"]')
eq "S3 destination test" "$(call POST /destinations/$DS/test -d '{}' | js 'd["ok"]')" True
BADS=$(call POST /destinations -d "${S3/bmtest-secret-key-123/wrong-secret}" | js 'd["id"]')
eq "wrong S3 secret is reported" "$(call POST /destinations/$BADS/test -o /dev/null -w '%{http_code}' -d '{}')" 400
eq "S3 secret never returned by the API" "$(call GET /destinations | grep -c 'bmtest-secret-key-123')" 0
PS3=$(new_plan "{\"name\":\"to-s3\",\"targetId\":$T,\"destinationId\":$DS,\"spec\":{\"content\":\"all\",\"databases\":[\"shop\"]},\"keepLast\":1,\"enabled\":true}")
start_load
B=$(run_plan $PS3); eq "backup to S3 under load" "$(wait_backup $B 300)" done
stop_load
N=$(docker exec bm-test-minio ls -R "/data/bm-bucket/prod/db/$B" | grep -c '\.enc')
if [ "$N" -ge 10 ]; then ok "objects are in the bucket under the prefix ($N)"; else bad "expected objects in the bucket, found $N"; fi
call POST /backups/$B/verify -d '{}' >/dev/null; for i in $(seq 1 60); do V=$(call GET /backups/$B | js 'd["backup"]["verifiedAt"]>0'); [ "$V" = True ] && break; sleep 1; done
eq "verify reads everything back from S3" "$(call GET /backups/$B | js '(d["backup"]["verifiedAt"]>0, d["backup"]["verifyError"])')" "(True, '')"
curl -sS -b "$JAR" -o "$RUN/s3.sql" "$URL/api/backups/$B/download?db=shop&nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_s3; CREATE DATABASE restore_s3 CHARACTER SET utf8mb4'
check "download from S3 restores" bash -c "docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw restore_s3 < '$RUN/s3.sql'"
eq "restored copy is consistent" "$(sql -e 'SELECT CONCAT(SUM(balance), "/", (SELECT COUNT(*) FROM restore_s3.ledger) = (SELECT n FROM restore_s3.stats)) FROM restore_s3.accounts')" "2000000.00/1"
eq "big table identical" "$(sql -e 'CHECKSUM TABLE restore_s3.big' | cut -f2)" "$(sql -e 'CHECKSUM TABLE shop.big' | cut -f2)"
RAW=$(call GET /backups/$B | js '[o["key"] for o in d["manifest"]["objects"] if o.get("table")=="big" and o["kind"]=="data"][0]')
SIZE=$(call GET /backups/$B | js '[o["storedBytes"] for o in d["manifest"]["objects"] if o.get("table")=="big" and o["kind"]=="data"][0]')
curl -sS -b "$JAR" -r 0-999999 -o "$RUN/raw.part" "$URL/api/backups/$B/download?raw=$RAW"
curl -sS -b "$JAR" -C - -o "$RUN/raw.part" "$URL/api/backups/$B/download?raw=$RAW"
eq "interrupted raw download resumes to the full size" "$(stat -c %s "$RUN/raw.part")" "$SIZE"
B2=$(run_plan $PS3); wait_backup $B2 300 >/dev/null; sleep 2
eq "retention removed the older backup's objects from the bucket" "$(docker exec bm-test-minio ls /data/bm-bucket/prod/db/ | grep -c "$B")" 0
docker stop bm-test-minio >/dev/null
B3=$(run_plan $PS3); sleep 1
eq "storage down: backup fails after retries, with the reason" "$(wait_backup $B3 120)/$(call GET /backups/$B3 | js 'len(d["backup"]["error"])>0')" "failed/True"
eq "the previous good backup is untouched" "$(call GET /backups/$B2 | js 'd["backup"]["status"]')" done
docker start bm-test-minio >/dev/null
stop_manager
echo; echo "03: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
