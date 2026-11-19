# Shared helpers for the integration scripts.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN="$HERE/.run"; BIN="$HERE/../bin/backup-manager"; URL="http://127.0.0.1:18750"; JAR="$RUN/cookies"
export BM_DATA_DIR="$RUN/data" BM_MASTER_KEY_FILE="$RUN/keys/master.key" BM_LISTEN="127.0.0.1:18750" BM_RETRY_BASE_SECONDS=2
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$*"; }
check() { local name="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }
eq()   { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }
sql()  { docker exec -i bm-test-db mariadb -uroot -pbmtest-root-pw -N -B "$@"; }
call() { local m="$1" p="$2"; shift 2; curl -sS -b "$JAR" -c "$JAR" -X "$m" -H 'X-BM: 1' -H 'Content-Type: application/json' "$URL/api$p" "$@"; }
js()   { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
kill_stale() { local p i; for p in $(pgrep -f 'bin/backup-manage[r] serve'); do kill "$p" 2>/dev/null; done
  for i in $(seq 1 30); do curl -s "$URL/api/state" >/dev/null 2>&1 || return 0; sleep 0.2; done; echo "port 18750 is still busy"; return 1; }
trap 'kill_stale >/dev/null 2>&1' EXIT
start_manager() { local i; kill_stale || return 1; "$BIN" serve >>"$RUN/manager.log" 2>&1 & echo $! >"$RUN/pid"; for i in $(seq 1 50); do curl -s "$URL/api/state" >/dev/null 2>&1 && return 0; sleep 0.2; done; echo "manager did not start"; tail -5 "$RUN/manager.log"; return 1; }
stop_manager() { [ -f "$RUN/pid" ] && kill "${1:--TERM}" "$(cat "$RUN/pid")" 2>/dev/null; sleep 0.7; rm -f "$RUN/pid"; return 0; }
# run_plan <planId> -> prints the backup id
run_plan() { call POST "/plans/$1/run" -d '{}' | js 'd["id"]'; }
# wait_backup <id> [seconds] -> prints final status
wait_backup() { local s="" i; for i in $(seq 1 $(( ${2:-180} * 2 ))); do s=$(call GET "/backups/$1" | js 'd["backup"]["status"]'); case "$s" in done|failed|cancelled) echo "$s"; return;; esac; sleep 0.5; done; echo "timeout:$s"; }
new_plan() { call POST /plans -d "$1" | js 'd["id"]'; }
# The write load is the only thing connected with shop as its default database, so it is stopped by that.
stop_load() { local id; for id in $(sql -e "SELECT id FROM information_schema.processlist WHERE db='shop' AND id<>CONNECTION_ID()"); do sql -e "KILL $id" 2>/dev/null; done; sleep 0.5; }
start_load() { stop_load; sql -e "CREATE USER IF NOT EXISTS 'loadgen'@'%' IDENTIFIED BY 'loadgen-pw'; GRANT ALL ON shop.* TO 'loadgen'@'%'"
  docker exec -d bm-test-db mariadb -uloadgen -ploadgen-pw shop -e 'CALL churn(100000000)'; sleep 1; }
login() { curl -s -c "$JAR" -X POST -H 'X-BM: 1' "$URL/api/login" -d '{"email":"owner@example.com","password":"correct-horse-9"}' >/dev/null; }
