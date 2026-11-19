#!/usr/bin/env bash
# Agents (a second process acting as "another server") and the container image. Standalone.
# Needs bm-test-db and bm-test-minio (see README.md).
source "$(dirname "$0")/lib.sh"
mkdir -p "$RUN"; rm -rf "$RUN/data" "$RUN/keys" "$RUN/store" "$RUN/agentdata"
for c in $(docker ps -aq --filter label=bm-sandbox=1); do docker rm -fv "$c" >/dev/null; done
catalog() { python3 -c "import sqlite3,sys; c=sqlite3.connect('$RUN/data/catalog.db', timeout=20); r=c.execute(sys.argv[1]).fetchall(); c.commit(); print('\n'.join('|'.join(str(x) for x in row) for row in r))" "$1"; }
wait_job() { local s="" i; for i in $(seq 1 $(( ${2:-120} * 2 ))); do s=$(catalog "SELECT state FROM jobs WHERE id=$1"); case "$s" in done|failed|cancelled) echo "$s"; return;; esac; sleep 0.5; done; echo "timeout:$s"; }
export BM_AGENT_PICKUP_SECONDS=6 BM_AGENT_SILENCE_SECONDS=8
start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
catalog "INSERT INTO settings(k,v) VALUES('sandbox_memory','300m')" >/dev/null
agent_up() { BM_DATA_DIR="$RUN/agentdata" "$BIN" agent --server "$URL" --token "$ATOK" >>"$RUN/agent.log" 2>&1 & echo $! > "$RUN/agent.pid"; }
agent_down() { kill "${1:--TERM}" "$(cat "$RUN/agent.pid")" 2>/dev/null; sleep 1; }
agent_online() { local i; for i in $(seq 1 30); do [ "$(call GET /agents | js 'd[0]["online"]')" = True ] && return 0; sleep 0.5; done; return 1; }

echo "== agent sign-up and safety"
ATOK=$(call POST /agents -d '{"name":"db-server-2"}' | js 'd["token"]'); AID=$(call GET /agents | js 'd[0]["id"]')
eq "agent token is not stored in clear" "$(grep -a -c "$ATOK" "$RUN/data/catalog.db")" 0
eq "an unknown agent token is refused" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer bma_nope' $URL/api/agent/poll)" 401
eq "a user token cannot act as an agent" "$(TU=$("$BIN" token create --name x --role owner 2>/dev/null); curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TU" $URL/api/agent/poll)" 401
eq "the agent refuses a plain-http panel on another machine" "$("$BIN" agent --server http://panel.example.com --token x 2>&1 | grep -c 'must be https')" 1
eq "agent shows as never connected" "$(call GET /agents | js '(d[0]["online"], d[0]["lastSeen"])')" "(False, 0)"
agent_up; if agent_online; then ok "agent connects out and shows online with its host name ($(call GET /agents | js 'd[0]["hostname"]'))"; else bad "agent did not come online: $(tail -2 "$RUN/agent.log")"; fi

echo "== backups through the agent"
T=$(call POST /targets -d "{\"name\":\"remote-db\",\"kind\":\"mysql\",\"cfg\":{\"host\":\"127.0.0.1\",\"port\":33999,\"user\":\"bm_backup\",\"exec\":\"docker:bm-test-db\",\"agent\":$AID},\"secret\":\"bm-backup-pw\"}" | js 'd["id"]')
D=$(call POST /destinations -d '{"name":"s3","kind":"s3","cfg":{"endpoint":"127.0.0.1:39000","bucket":"bm-bucket","prefix":"agents","accessKey":"bmtestaccess","useSSL":false},"secret":"bmtest-secret-key-123"}' | js 'd["id"]')
eq "target test reports the agent" "$(call POST /targets/$T/test -d '{}' | js '"online" in d["version"]')" True
eq "an agent with targets cannot be deleted" "$(call DELETE /agents/$AID -o /dev/null -w '%{http_code}')" 400
plan() { new_plan "{\"name\":\"$1\",\"targetId\":$T,\"destinationId\":$D,\"spec\":$2,\"schedule\":\"\",\"enabled\":true}"; }
PL=$(plan remote-logical "{\"content\":\"all\",\"databases\":[\"shop\"],\"preHook\":\"hostname > $RUN/agent-hook.txt\"}")
start_load
B=$(run_plan $PL); eq "logical backup done by the agent, under load" "$(wait_backup $B 300)" done
stop_load
check "the agent, not the panel, did the work" grep -q "task $B-1 done" "$RUN/agent.log"
check "the plan's hook ran on the agent's machine" test -s "$RUN/agent-hook.txt"
eq "the panel has the full record (tables, rows, position)" "$(call GET /backups/$B | js '(d["backup"]["tables"]>=8, d["backup"]["rows"]>400000, bool(d["manifest"]["position"]), d["backup"]["encrypted"])')" "(True, True, True, True)"
curl -sS -b "$JAR" -o "$RUN/agent.sql" "$URL/api/backups/$B/download?db=shop&nodb=1"
sql -e 'DROP DATABASE IF EXISTS restore_agent; CREATE DATABASE restore_agent CHARACTER SET utf8mb4'
check "the panel can read the agent's backup from S3 with its own key; it restores" bash -c "docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw restore_agent < '$RUN/agent.sql'"
eq "restored copy is consistent and identical" "$(sql -e 'SELECT CONCAT(SUM(balance), "/", (SELECT COUNT(*) FROM restore_agent.ledger) = (SELECT n FROM restore_agent.stats)) FROM restore_agent.accounts')/$(sql -e 'CHECKSUM TABLE restore_agent.big' | cut -f2)" "2000000.00/1/$(sql -e 'CHECKSUM TABLE shop.big' | cut -f2)"
check "nothing of the task is left in the agent's own folder" bash -c "! grep -r -a -q -e 'bm-backup-pw' -e 'bmtest-secret-key-123' '$RUN/agentdata'"
PP=$(plan remote-physical '{"method":"physical","incrementalsPerFull":1}')
F=$(run_plan $PP); eq "physical full through the agent" "$(wait_backup $F 300)" done
start_load; sleep 3; stop_load
I=$(run_plan $PP); eq "physical incremental through the agent" "$(wait_backup $I 300)" done
eq "the incremental builds on the full (chain kept by the panel)" "$(call GET /backups/$I | js '(d["backup"]["method"], d["backup"]["parentId"])')" "('incremental', '$F')"
call POST /backups/$I/test -d '{}' >/dev/null
eq "restore test on the panel proves the agent's chain" "$(wait_job $(catalog "SELECT id FROM jobs WHERE kind='restore-test' ORDER BY id DESC LIMIT 1") 500)/$(call GET /backups/$I | js 'd["backup"]["testError"]')" "done/"

echo "== cancel, agent down, agent killed"
PS=$(plan remote-slow '{"content":"all","tables":["shop.big"],"rateMB":1}')
BS=$(run_plan $PS); sleep 7
call POST /jobs/$(catalog "SELECT id FROM jobs WHERE backup_id='$BS'")/cancel -d '{}' >/dev/null
eq "cancel reaches the agent" "$(wait_backup $BS 60)/$(sleep 3; grep -c "task $BS-1: the panel asked to stop" "$RUN/agent.log")" "cancelled/1"
eq "the cancelled backup left nothing in the bucket" "$(docker exec bm-test-minio ls /data/bm-bucket/agents/ | grep -c "$BS")" 0
BK=$(run_plan $PS); sleep 7
agent_down -KILL
for i in $(seq 1 60); do E=$(call GET /backups/$BK | js 'd["backup"]["error"]'); echo "$E" | grep -q 'silent\|did not pick up' && break; sleep 0.5; done
eq "an agent that dies mid-backup is noticed" "$(echo "$E" | grep -c 'went silent')" 1
agent_up; agent_online
eq "after the agent is back, the retry completes the backup" "$(wait_backup $BK 300)/$(catalog "SELECT attempt>1 FROM jobs WHERE backup_id='$BK'")" "done/1"
agent_down
BD=$(run_plan $PL)
eq "with the agent off, the backup fails with a clear reason" "$(wait_backup $BD 120)/$(call GET /backups/$BD | js '"did not pick up" in d["backup"]["error"]')" "failed/True"
eq "the earlier good backups are untouched" "$(call GET /backups/$B | js 'd["backup"]["status"]')" done
stop_manager

echo "== container image"
docker rm -f bm-test-image >/dev/null 2>&1; docker volume rm bm-test-vol >/dev/null 2>&1
if (cd "$HERE/.." && docker build -q -t backup-manager:test . >"$RUN/build.log" 2>&1); then ok "image builds"; else bad "image build failed: $(tail -3 "$RUN/build.log")"; fi
docker run -d --name bm-test-image --memory=200m -p 127.0.0.1:18770:8750 -v bm-test-vol:/data backup-manager:test >/dev/null
for i in $(seq 1 40); do curl -s http://127.0.0.1:18770/healthz >/dev/null 2>&1 && break; sleep 0.5; done
eq "the container answers" "$(curl -s http://127.0.0.1:18770/healthz)" ok
eq "it does not run as root" "$(docker exec bm-test-image id -u)" 870
LINK=$(docker exec bm-test-image backup-manager setup-link)
eq "first account through the printed link" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-BM: 1' http://127.0.0.1:18770/api/setup -d "{\"token\":\"${LINK##*=}\",\"email\":\"me@example.com\",\"password\":\"long-enough-pw\"}")" 200
for i in $(seq 1 45); do [ "$(docker inspect -f '{{.State.Health.Status}}' bm-test-image)" = healthy ] && break; sleep 1; done
eq "its health check passes" "$(docker inspect -f '{{.State.Health.Status}}' bm-test-image)" healthy
docker rm -f bm-test-image >/dev/null; docker run -d --name bm-test-image --memory=200m -p 127.0.0.1:18770:8750 -v bm-test-vol:/data backup-manager:test >/dev/null
for i in $(seq 1 40); do curl -s http://127.0.0.1:18770/healthz >/dev/null 2>&1 && break; sleep 0.5; done
eq "a new container on the same volume keeps the account and the key" "$(curl -s http://127.0.0.1:18770/api/state | js 'd["setupNeeded"]')" False
docker rm -f bm-test-image >/dev/null; docker volume rm bm-test-vol >/dev/null; docker rmi -f backup-manager:test >/dev/null 2>&1
echo; echo "08: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
