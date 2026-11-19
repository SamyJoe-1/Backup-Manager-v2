#!/usr/bin/env bash
# Installs Backup Manager on fresh "VPS" containers that run real systemd, in every supported way,
# and checks the result. Run images.sh first and scripts/release.sh (for dist/).
# Usage: ./run.sh [scenario ...]   scenarios: refuse plain mariadb source nginx rocky agent nosystemd
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; REPO="$(cd "$HERE/../.." && pwd)"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$*"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$*"; }
eq()  { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }
has() { if printf '%s' "$2" | grep -q -- "$3"; then ok "$1"; else bad "$1: no '$3' in: $(printf '%s' "$2" | tail -3 | tr '\n' ' ' | cut -c1-300)"; fi; }
js()  { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
C=""
vps() { # vps NAME IMAGE: start a fresh machine with the repository mounted read-only
  C="bm-vps-$1"; docker rm -fv "$C" >/dev/null 2>&1
  docker run -d --name "$C" --hostname "$1" --privileged --cgroupns=private --memory=${MEM:-700m} \
    -v "$REPO:/repo:ro" "bm-vps:$2" >/dev/null || return 1
  local i; for i in $(seq 1 60); do docker exec "$C" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && return 0; sleep 1; done
  echo "systemd did not come up in $C"; return 1
}
x()   { docker exec "$C" bash -c "$1"; }                     # run on the machine
pipe() { docker exec "$C" bash -c "cat /repo/install.sh | bash -s -- $1" 2>&1; }   # like curl … | sudo bash -s -- …
api() { local m="$1" p="$2"; shift 2; docker exec "$C" curl -sS -b /tmp/jar -c /tmp/jar -X "$m" -H 'X-BM: 1' -H 'Content-Type: application/json' "http://127.0.0.1:${PORT:-8750}/api$p" "$@"; }
first_account() { # create the account from the printed link, then sign in
  local link tok; link="$(printf '%s' "$1" | grep -oE 'https?://[^ ]*#setup=[0-9a-f]+' | head -1)"; tok="${link##*=}"
  eq "the printed link creates the account" "$(api POST /setup -o /dev/null -w '%{http_code}' -d "{\"token\":\"$tok\",\"email\":\"admin@example.com\",\"password\":\"long-enough-pw\"}")" 200
}
wait_backup() { local s i; for i in $(seq 1 ${2:-240}); do s=$(api GET "/backups/$1" | js 'd["backup"]["status"]'); case "$s" in done|failed|cancelled) echo "$s"; return;; esac; sleep 1; done; echo "timeout:$s"; }
backup_error() { api GET "/backups/$1" | js 'd["backup"]["error"]'; }
done_vps() { docker rm -fv "$C" >/dev/null 2>&1; }
DIST=/repo/dist

sc_refuse() {
  echo "== refusals, dry run, half-downloaded script (Debian 12)"
  vps refuse debian12 || return
  has "not root: refuses and says how" "$(docker exec -u nobody "$C" bash -c 'cat /repo/install.sh | bash -s -- --yes' 2>&1)" "run it as root"
  docker exec "$C" bash -c "head -c 5000 /repo/install.sh | bash -s -- --yes --binary $DIST/backup-manager-linux-amd64" >/dev/null 2>&1
  eq "a cut-off download installs nothing" "$(x 'ls /usr/local/bin/backup-manager /etc/backup-manager 2>/dev/null | wc -l')" 0
  has "unknown option is refused" "$(pipe '--frob')" "unknown option"
  has "piped without a terminal and without --yes: stops and says to use --yes" "$(pipe "--binary $DIST/backup-manager-linux-amd64")" "run again with --yes"
  local out; out="$(pipe "--yes --dry-run --binary $DIST/backup-manager-linux-amd64")"
  has "dry run lists the steps" "$out" "dry run: nothing was changed"
  eq "dry run changes nothing" "$(x 'ls /usr/local/bin/backup-manager /etc/backup-manager /etc/systemd/system/backup-manager.service 2>/dev/null | wc -l; id backupmgr >/dev/null 2>&1 && echo user')" 0
  has "--help prints the options" "$(x 'bash /repo/install.sh --help')" "--agent-server URL"
  done_vps
}

sc_plain() {
  echo "== plain VPS, piped install, upgrade, reboot, uninstall (Debian 12)"
  vps plain debian12 || return
  local out; out="$(pipe "--yes --binary $DIST/backup-manager-linux-amd64")"
  has "installer finishes" "$out" "Backup Manager is installed"
  eq "service is active and enabled" "$(x 'systemctl is-active backup-manager; systemctl is-enabled backup-manager' | tr '\n' ' ')" "active enabled "
  eq "runs as the locked-down user" "$(x 'ps -o user= -C backup-manager | head -1')" backupmgr
  eq "settings, key, env are owner-only" "$(x 'stat -c "%a %U" /etc/backup-manager /etc/backup-manager/env /etc/backup-manager/master.key' | tr '\n' ' ')" "750 backupmgr 600 backupmgr 600 backupmgr "
  eq "panel listens on this machine only" "$(x 'ss -ltn | grep -c "127.0.0.1:8750"; ss -ltn | grep -c "0.0.0.0:8750"' | tr '\n' ' ')" "1 0 "
  has "the self-check ran and passed" "$out" "0 failed"
  has "it explains the SSH tunnel" "$out" "ssh -L 8750:127.0.0.1:8750"
  has "it warns to keep the master key" "$out" "Keep a copy of /etc/backup-manager/master.key"
  first_account "$out"
  local KEY; KEY="$(x 'cat /etc/backup-manager/master.key')"
  docker restart "$C" >/dev/null; sleep 3; for i in $(seq 1 60); do x 'curl -fs http://127.0.0.1:8750/healthz' >/dev/null 2>&1 && break; sleep 1; done
  eq "after a reboot the service is back by itself" "$(x 'curl -fs http://127.0.0.1:8750/healthz')" ok
  out="$(pipe "--yes --binary $DIST/backup-manager-linux-amd64")"
  has "running it again upgrades" "$out" "Upgraded; all settings and backups were kept"
  eq "upgrade keeps the key and the account" "$([ "$(x 'cat /etc/backup-manager/master.key')" = "$KEY" ] && echo same)/$(x 'curl -s http://127.0.0.1:8750/api/state' | js 'd["setupNeeded"]')" "same/False"
  out="$(pipe "--uninstall --yes")"
  eq "uninstall removes program and service, keeps key and data" "$(x 'ls /usr/local/bin/backup-manager /etc/systemd/system/backup-manager.service 2>/dev/null | wc -l; ls /etc/backup-manager/master.key /var/lib/backup-manager/catalog.db | wc -l' | tr '\n' ' ')" "0 2 "
  pipe "--yes --binary $DIST/backup-manager-linux-amd64" >/dev/null
  eq "installing again over kept data keeps the account" "$(x 'curl -s http://127.0.0.1:8750/api/state' | js 'd["setupNeeded"]')" False
  done_vps
}

sc_mariadb() {
  echo "== VPS with MariaDB installed directly: root mode, tools, database user, real backups (Debian 12)"
  MEM=900m vps mariadb debian12 || return
  x 'mkdir -p /etc/mysql/mariadb.conf.d && printf "[mysqld]\ninnodb_buffer_pool_size=32M\nperformance_schema=OFF\nlog_bin=binlog\n" >/etc/mysql/mariadb.conf.d/99-small.cnf
     apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq mariadb-server >/dev/null && systemctl start mariadb
     mariadb -e "CREATE DATABASE shop; CREATE TABLE shop.orders (id INT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2)); SET SESSION max_recursive_iterations=10000; INSERT INTO shop.orders (total) WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<5000) SELECT i FROM n;"' >/dev/null 2>&1
  eq "test MariaDB is up" "$(x 'mariadb -N -e "SELECT COUNT(*) FROM shop.orders"')" 5000
  local out; out="$(pipe "--yes --run-as root --install-tools --create-db-user --binary $DIST/backup-manager-linux-amd64")"
  has "installer saw the local MariaDB" "$out" "MariaDB installed directly"
  has "installer finishes" "$out" "Backup Manager is installed"
  eq "runs as root (needed to read the data folder)" "$(x 'ps -o user= -C backup-manager | head -1')" root
  eq "mariadb-backup was installed" "$(x 'command -v mariabackup >/dev/null && echo yes')" yes
  local pw; pw="$(x "sed -n 's/^password=//p' /etc/backup-manager/db-user.txt")"
  has "the database user and password are printed" "$out" "user bm_backup, password $pw"
  eq "the user file is owner-only" "$(x 'stat -c %a /etc/backup-manager/db-user.txt')" 600
  first_account "$out"
  api POST /login -o /dev/null -d '{"email":"admin@example.com","password":"long-enough-pw"}'
  local T D
  T=$(api POST /targets -d "{\"name\":\"local mariadb\",\"kind\":\"mysql\",\"cfg\":{\"host\":\"127.0.0.1\",\"port\":3306,\"user\":\"bm_backup\",\"exec\":\"local\"},\"secret\":\"$pw\"}" | js 'd["id"]')
  D=$(api POST /destinations -d '{"name":"disk","kind":"local","cfg":{"path":"/var/lib/backup-manager/backups"}}' | js 'd["id"]')
  eq "connection test with the created user" "$(api POST /targets/$T/test -d '{}' | js 'd["ok"]')" True
  plan() { api POST /plans -d "{\"name\":\"$1\",\"targetId\":$2,\"destinationId\":$D,\"spec\":$3,\"schedule\":\"\",\"enabled\":true}" | js 'd["id"]'; }
  local P B
  P=$(plan sql $T '{"content":"all"}'); B=$(api POST /plans/$P/run -d '{}' | js 'd["id"]')
  eq "SQL backup with the created user" "$(wait_backup $B)" done
  P=$(plan physical $T '{"method":"physical","incrementalsPerFull":1}'); B=$(api POST /plans/$P/run -d '{}' | js 'd["id"]')
  local st; st="$(wait_backup $B 400)"; eq "physical backup with the created user and installed tools" "$st" done; [ "$st" = done ] || echo "    $(backup_error $B | cut -c1-300)"
  local I; I=$(api POST /plans/$P/run -d '{}' | js 'd["id"]'); eq "incremental" "$(wait_backup $I 400)" done
  api POST "/backups/$I/restore" -o /dev/null -d '{"dir":"/var/lib/bm-restore/shop"}'
  local i; for i in $(seq 1 120); do [ -f /dev/null ] && x 'test -f /var/lib/bm-restore/shop/BM-RESTORE.txt' 2>/dev/null && break; sleep 1; done
  eq "the restored copy holds the data" "$(x 'test -f /var/lib/bm-restore/shop/shop/orders.ibd && echo yes')" yes
  eq "physical restore built a data folder" "$(x 'test -f /var/lib/bm-restore/shop/BM-RESTORE.txt && test -d /var/lib/bm-restore/shop/shop && echo yes')" yes
  local TF; TF=$(api POST /targets -d '{"name":"etc","kind":"files","cfg":{"paths":["/etc/backup-manager","/etc/mysql"]}}' | js 'd["id"]')
  P=$(plan files $TF '{}'); B=$(api POST /plans/$P/run -d '{}' | js 'd["id"]')
  eq "folder backup of root-only files (root mode)" "$(wait_backup $B)/$(api GET /backups/$B | js 'len(d["manifest"].get("warnings") or [])')" "done/0"
  done_vps
}

sc_source() {
  echo "== from a clone, build from source, built-in HTTPS (Ubuntu 24.04)"
  MEM=1500m vps source ubuntu24 || return
  x 'cp -r /repo /root/Backup-Manager-v2 && rm -rf /root/Backup-Manager-v2/bin /root/Backup-Manager-v2/dist'
  local out; out="$(x 'cd /root/Backup-Manager-v2 && BM_BUILD_SWAP=no ./install.sh --yes --from-source --domain bm.example.test --email ops@example.com' 2>&1)"
  has "built from source" "$out" "built $(cat "$REPO/VERSION")"
  has "installer finishes" "$out" "Backup Manager is installed"
  eq "the built program reports its version" "$(x 'backup-manager version')" "$(cat "$REPO/VERSION")"
  eq "HTTPS is built in: env has the domain and e-mail" "$(x 'grep -c -E "^BM_TLS_DOMAIN=bm.example.test|^BM_TLS_EMAIL=ops@example.com" /etc/backup-manager/env')" 2
  eq "the locked-down user may bind ports 80 and 443" "$(x 'grep -c CAP_NET_BIND_SERVICE /etc/systemd/system/backup-manager.service')/$(x 'ps -o user= -C backup-manager | head -1')" "1/backupmgr"
  eq "ports 80 and 443 are served" "$(x 'ss -ltnp | grep backup-manager | grep -cE ":(80|443) "')" 2
  eq "port 80 sends visitors to https" "$(x "curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: bm.example.test' http://127.0.0.1/backups")" "301 https://bm.example.test/backups"
  has "the setup link uses the https address" "$out" "https://bm.example.test/#setup="
  # own certificate instead of Let's Encrypt (no real domain here)
  x 'openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=bm.example.test -keyout /etc/backup-manager/key.pem -out /etc/backup-manager/cert.pem >/dev/null 2>&1 || (apt-get update -qq && apt-get install -y -qq openssl >/dev/null && openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=bm.example.test -keyout /etc/backup-manager/key.pem -out /etc/backup-manager/cert.pem >/dev/null 2>&1)
     chown backupmgr /etc/backup-manager/*.pem; sed -i "/^BM_TLS_/d" /etc/backup-manager/env
     printf "BM_TLS_CERT=/etc/backup-manager/cert.pem\nBM_TLS_KEY=/etc/backup-manager/key.pem\n" >>/etc/backup-manager/env; systemctl restart backup-manager; sleep 2'
  eq "own certificate: https answers" "$(x 'curl -sk https://127.0.0.1/healthz')" ok
  eq "own certificate: it is the one given" "$(x 'echo | openssl s_client -connect 127.0.0.1:443 2>/dev/null | openssl x509 -noout -subject' | grep -o 'bm.example.test')" bm.example.test
  first_account "$out"
  eq "sign-in cookie is marked secure over https" "$(x "curl -sk -D - -o /dev/null -X POST -H 'X-BM: 1' https://127.0.0.1/api/login -d '{\"email\":\"admin@example.com\",\"password\":\"long-enough-pw\"}' | grep -i set-cookie | grep -ci secure")" 1
  done_vps
}

sc_nginx() {
  echo "== VPS that already runs nginx on port 80 (Ubuntu 22.04)"
  MEM=900m vps nginx ubuntu22 || return
  x 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx >/dev/null && systemctl start nginx' >/dev/null 2>&1
  eq "nginx is serving port 80" "$(x 'ss -ltnp | grep -c "\"nginx\""' | head -1 | awk '{print ($1>0)}')" 1
  local out; out="$(pipe "--yes --domain bm.example.test --binary $DIST/backup-manager-linux-amd64")"
  has "installer finishes" "$out" "Backup Manager is installed"
  has "it noticed nginx and used it" "$out" "through the nginx already running here"
  eq "a site was added and nginx accepts it" "$(x 'test -f /etc/nginx/sites-enabled/backup-manager.conf && nginx -t 2>&1 | grep -c successful')" 1
  eq "the domain reaches the panel through nginx" "$(x "curl -s -H 'Host: bm.example.test' http://127.0.0.1/healthz")" ok
  eq "the panel itself stays private" "$(x 'ss -ltn | grep -c "127.0.0.1:8750"')" 1
  has "certificate step reported honestly (no real domain here)" "$out" "certbot"
  pipe "--uninstall --yes" >/dev/null
  eq "uninstall removes the nginx site and nginx still works" "$(x 'ls /etc/nginx/sites-enabled/backup-manager.conf 2>/dev/null | wc -l; nginx -t 2>&1 | grep -c successful' | tr '\n' ' ')" "0 1 "
  done_vps
}

sc_rocky() {
  echo "== RHEL family, panel on the server IP (Rocky 9)"
  vps rocky rocky9 || return
  local out; out="$(pipe "--yes --public --port 9911 --binary $DIST/backup-manager-linux-amd64")"
  has "installer finishes on dnf systems" "$out" "Backup Manager is installed"
  eq "panel listens on every interface, chosen port" "$(x 'ss -ltn | grep -cE "(0.0.0.0|\*|\[::\]):9911 "')" 1
  has "it warns about plain http" "$out" "plain HTTP"
  local ip; ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$C")"
  eq "reachable from another machine" "$(curl -s --max-time 5 "http://$ip:9911/healthz")" ok
  has "the setup link uses the server IP" "$out" "http://$ip:9911/#setup="
  done_vps
}

sc_agent() {
  echo "== agent on a second server (Debian 12 panel, Ubuntu 24.04 agent)"
  vps panel debian12 || return
  local P=$C out; out="$(pipe "--yes --public --binary $DIST/backup-manager-linux-amd64")"
  PORT=8750 first_account "$out"
  api POST /login -o /dev/null -d '{"email":"admin@example.com","password":"long-enough-pw"}'
  local tok; tok="$(api POST /agents -d '{"name":"second-server"}' | js 'd["token"]')"
  local ip; ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$P")"
  vps agent ubuntu24 || return
  local A=$C
  has "an http panel address is refused without the explicit switch" "$(pipe "--yes --agent-server http://$ip:8750 --agent-token $tok --binary $DIST/backup-manager-linux-amd64")" "use the panel's https:// address"
  out="$(pipe "--yes --agent-server http://$ip:8750 --agent-token $tok --agent-allow-http --binary $DIST/backup-manager-linux-amd64")"
  has "agent install finishes" "$out" "This machine is now an agent"
  eq "agent service runs, opens no port" "$(x 'systemctl is-active backup-manager'; x 'ss -ltnp | grep -c backup-manager')" "active
0"
  eq "agent token kept owner-only" "$(x 'stat -c %a /etc/backup-manager/env')" 600
  C=$P
  local i; for i in $(seq 1 30); do [ "$(api GET /agents | js 'd[0]["online"]')" = True ] && break; sleep 1; done
  eq "the panel sees the agent online, with its host name" "$(api GET /agents | js '(d[0]["online"], d[0]["hostname"])')" "(True, 'agent')"
  local aid T D PL B
  aid="$(api GET /agents | js 'd[0]["id"]')"
  T=$(api POST /targets -d "{\"name\":\"agent files\",\"kind\":\"files\",\"cfg\":{\"paths\":[\"/etc/os-release\"],\"agent\":$aid}}" | js 'd["id"]')
  D=$(api POST /destinations -d '{"name":"agent disk","kind":"local","cfg":{"path":"/var/lib/backup-manager/backups"}}' | js 'd["id"]')
  PL=$(api POST /plans -d "{\"name\":\"via agent\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{},\"schedule\":\"\",\"enabled\":true}" | js 'd["id"]')
  B=$(api POST /plans/$PL/run -d '{}' | js 'd["id"]')
  eq "a backup runs on the agent's machine" "$(wait_backup $B 90)" done
  C=$A; eq "the file landed on the agent's disk" "$(x "ls /var/lib/backup-manager/backups/$B | grep -c files.tar")" 1
  C=$P; done_vps; C=$A; done_vps
}

sc_nosystemd() {
  echo "== machine without systemd"
  docker rm -f bm-vps-nosd >/dev/null 2>&1
  local out; out="$(docker run --rm --name bm-vps-nosd -v "$REPO:/repo:ro" debian:bookworm-slim bash -c 'cat /repo/install.sh | bash -s -- --yes' 2>&1)"
  has "refuses and points to the container image" "$out" "use the container image"
}

all="refuse plain mariadb source nginx rocky agent nosystemd"
for s in ${*:-$all}; do "sc_$s"; done
echo; echo "installer: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
