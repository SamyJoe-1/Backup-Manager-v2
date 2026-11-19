#!/usr/bin/env bash
# Backup Manager installer — one command on a fresh Linux server (Debian, Ubuntu, RHEL/Rocky/Alma).
#
# Public repository:
#   curl -fsSL https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo bash
# Private repository (a GitHub token that can read it):
#   export GITHUB_TOKEN=github_pat_...
#   curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
#     https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo -E bash
# From a clone of the repository:
#   sudo ./install.sh
#
# Options (after `bash -s --` when piped, e.g. `| sudo bash -s -- --yes --domain backups.example.com`):
#   --yes                 no questions: defaults plus the options below
#   --domain NAME         serve the panel on https://NAME (certificate made automatically)
#   --email ADDRESS       e-mail for the certificate authority (optional)
#   --public              open the panel on http://<server IP>:PORT (no domain; not recommended)
#   --port N              port of the plain panel (default 8750)
#   --data-dir DIR        catalog and local backups (default /var/lib/backup-manager)
#   --run-as USER         backupmgr (default, a locked-down system user) or root
#   --docker-access       let the service use Docker (databases in containers, sandbox, restore tests)
#   --no-docker-access    never ask about Docker
#   --install-tools       install MariaDB backup tools when a MariaDB runs directly on this machine
#   --create-db-user      create a MariaDB/MySQL user for backups on this machine (needs root access to it)
#   --version vX.Y.Z      install that release instead of the newest
#   --from-source         build the program here instead of downloading a release
#   --binary FILE         install this ready-made program file
#   --agent-server URL    install as an AGENT of a panel on another server (no panel here)
#   --agent-token TOKEN   the agent token made in that panel (Users & access → Agents)
#   --agent-allow-http    accept a plain http:// panel address (only on a private network)
#   --dry-run             print what would happen, change nothing
#   --uninstall           remove the program (asks before touching data)
#
# Running it again upgrades the program and keeps every setting and backup.
# The whole script is one function called on the last line, so a half-downloaded copy runs nothing.

main() {
  set -euo pipefail
  umask 022

  REPO="${BM_REPO:-SamyJoe-1/Backup-Manager-v2}"
  BRANCH="${BM_BRANCH:-main}"
  TOKEN="${GITHUB_TOKEN:-${BM_GITHUB_TOKEN:-}}"
  NAME="backup-manager"
  BIN_PATH="/usr/local/bin/$NAME"
  ETC_DIR="/etc/$NAME"
  DATA_DIR="/var/lib/$NAME"
  UNIT="/etc/systemd/system/$NAME.service"
  PORT=8750; DOMAIN=""; EMAIL=""; PUBLIC=0; YES=0; DRY=0; UNINSTALL=0; VERSION="latest"
  LOCAL_BINARY=""; FROM_SOURCE=0; RUN_AS=""; DOCKER_ACCESS=""; INSTALL_TOOLS=""; CREATE_DB_USER=""
  AGENT_SERVER=""; AGENT_TOKEN=""; HTTPS_MODE="none"
  SRC_DIR=""
  local self="${BASH_SOURCE[0]:-}"
  if [ -n "$self" ] && [ -f "$self" ]; then
    local d; d="$(cd "$(dirname "$self")" && pwd)"
    [ -f "$d/go.mod" ] && [ -d "$d/cmd/backup-manager" ] && SRC_DIR="$d"
  fi

  while [ $# -gt 0 ]; do
    case "$1" in
      --yes|-y) YES=1 ;;
      --domain) DOMAIN="${2:?--domain needs a value}"; shift ;;
      --email) EMAIL="${2:?--email needs a value}"; shift ;;
      --public) PUBLIC=1 ;;
      --port) PORT="${2:?--port needs a value}"; shift ;;
      --data-dir) DATA_DIR="${2:?--data-dir needs a value}"; shift ;;
      --run-as) RUN_AS="${2:?--run-as needs a value}"; shift ;;
      --docker-access) DOCKER_ACCESS=yes ;;
      --no-docker-access) DOCKER_ACCESS=no ;;
      --install-tools) INSTALL_TOOLS=yes ;;
      --create-db-user) CREATE_DB_USER=yes ;;
      --version) VERSION="${2:?--version needs a value}"; shift ;;
      --from-source) FROM_SOURCE=1 ;;
      --binary) LOCAL_BINARY="${2:?--binary needs a value}"; shift ;;
      --agent-server) AGENT_SERVER="${2:?--agent-server needs a value}"; shift ;;
      --agent-token) AGENT_TOKEN="${2:?--agent-token needs a value}"; shift ;;
      --agent-allow-http) AGENT_HTTP=1 ;;
      --dry-run) DRY=1 ;;
      --uninstall) UNINSTALL=1 ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
    shift
  done

  if [ -t 1 ]; then B=$'\033[1m'; G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[31m'; N=$'\033[0m'; else B=""; G=""; Y=""; R=""; N=""; fi

  # ---- can this machine run it at all? ----
  [ "$(uname -s)" = Linux ] || die "this installer supports Linux only"
  [ "$DRY" = 1 ] || [ "$(id -u)" = 0 ] || die "run it as root: put sudo before bash (and sudo -E to pass GITHUB_TOKEN)"
  case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "unsupported CPU: $(uname -m) (supported: x86_64, arm64)" ;; esac
  if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
    die "systemd is not running on this machine (it is needed to keep the service running).
       In a container or on a system without systemd, use the container image instead: see docs/INSTALL.md, 'Docker'."
  fi
  case "$PORT" in ''|*[!0-9]*) die "--port must be a number" ;; esac
  case "$DATA_DIR" in /*) ;; *) die "--data-dir must be an absolute path" ;; esac
  case "$RUN_AS" in ''|backupmgr|root) ;; *) die "--run-as must be backupmgr or root" ;; esac
  if [ -n "$AGENT_SERVER$AGENT_TOKEN" ] && { [ -z "$AGENT_SERVER" ] || [ -z "$AGENT_TOKEN" ]; }; then
    die "an agent needs both --agent-server and --agent-token"
  fi
  detect_pkg

  if [ "$UNINSTALL" = 1 ]; then uninstall; exit 0; fi

  say "${B}Backup Manager installer${N}"
  ensure_basics
  discover

  UPGRADE=0
  if [ -x "$BIN_PATH" ] && [ -f "$ETC_DIR/env" ]; then
    UPGRADE=1
    load_existing
    say ""
    say "An installation already exists: this run ${B}upgrades the program${N} and keeps every setting and backup."
  else
    questions
  fi

  summary
  confirm "Continue?" || die "stopped, nothing was changed"

  get_program
  if [ "$UPGRADE" = 0 ]; then
    create_user_and_folders
    write_env
  fi
  grant_docker
  install_tools
  write_unit
  run systemctl daemon-reload
  run systemctl enable "$NAME" >/dev/null 2>&1 || true
  run systemctl restart "$NAME"
  wait_healthy
  [ "$UPGRADE" = 1 ] || setup_nginx
  [ "$UPGRADE" = 1 ] || open_firewall
  [ "$UPGRADE" = 1 ] || create_db_user
  finish
}

usage() { sed -n '2,37p' "${BASH_SOURCE[0]:-/dev/null}" 2>/dev/null | sed 's/^# \{0,1\}//' || true; }
say() { printf '%s\n' "$*"; }
die() { printf '%s\n' "${R:-}error:${N:-} $*" >&2; exit 1; }
note() { printf '%s\n' "${Y:-}note:${N:-} $*"; }
run() { if [ "$DRY" = 1 ]; then say "  [dry-run] $*"; else "$@"; fi; }
as_service() { ( set -a; . "$ETC_DIR/env"; set +a; if [ "$RUN_AS" = root ]; then "$@"; else runuser -u backupmgr -- "$@"; fi ); }

# Questions are read from the terminal, so they work under `curl … | bash`.
interactive() { [ "$YES" = 0 ] && { true </dev/tty; } 2>/dev/null; }
ask() { # ask VAR "question" "default"
  local __var="$1" q="$2" def="$3" ans=""
  if ! interactive; then printf -v "$__var" '%s' "$def"; return; fi
  printf '%s [%s]: ' "$q" "$def" >/dev/tty; IFS= read -r ans </dev/tty || ans=""
  printf -v "$__var" '%s' "${ans:-$def}"
}
yesno() { # yesno "question" default(y|n)
  local a def="$2"
  if ! interactive; then [ "$def" = y ]; return; fi
  local hint="[Y/n]"; [ "$def" = n ] && hint="[y/N]"
  printf '%s %s: ' "$1" "$hint" >/dev/tty; IFS= read -r a </dev/tty || a=""
  a="${a:-$def}"; case "$a" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}
confirm() {
  [ "$YES" = 1 ] && return 0
  interactive || die "no terminal to ask on; run again with --yes (and the options you want)"
  yesno "$1" y
}

detect_pkg() {
  PKG=none
  if command -v apt-get >/dev/null 2>&1; then PKG=apt
  elif command -v dnf >/dev/null 2>&1; then PKG=dnf
  elif command -v yum >/dev/null 2>&1; then PKG=yum; fi
}
pkg_install() {
  case "$PKG" in
    apt) run env DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >/dev/null || { run apt-get update -q >/dev/null; run env DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >/dev/null; } ;;
    dnf|yum) run "$PKG" install -y -q "$@" >/dev/null ;;
    *) return 1 ;;
  esac
}

ensure_basics() {
  local need=()
  command -v curl >/dev/null 2>&1 || need+=(curl)
  command -v tar >/dev/null 2>&1 || need+=(tar)
  command -v sha256sum >/dev/null 2>&1 || need+=(coreutils)
  [ -f /etc/ssl/certs/ca-certificates.crt ] || [ -f /etc/pki/tls/certs/ca-bundle.crt ] || need+=(ca-certificates)
  if [ ${#need[@]} -gt 0 ]; then
    say "Installing what the installer itself needs: ${need[*]}"
    pkg_install "${need[@]}" || die "please install ${need[*]} and run again"
  fi
}

port_busy() { # port_busy N -> program name using it, or nothing
  command -v ss >/dev/null 2>&1 || return 1
  ss -ltnpH 2>/dev/null | awk -v p=":$1\$" '$4 ~ p {print $6; exit}' | sed -E 's/.*\(\("([^"]+)".*/\1/' || true
}
server_ip() {
  local ip; ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
  [ -n "$ip" ] || ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' || true)"
  printf '%s' "${ip:-YOUR_SERVER_IP}"
}

LOCAL_MARIADB=0; LOCAL_MYSQL=0; LOCAL_PG=0; HAS_DOCKER=0
discover() {
  say ""
  say "${B}Found on this machine${N}"
  local procs=""
  command -v ss >/dev/null 2>&1 && procs="$(ss -ltnpH 2>/dev/null || true)"
  if echo "$procs" | grep -q '"mariadbd"' || { command -v mariadbd >/dev/null 2>&1 && pgrep -x mariadbd >/dev/null 2>&1; }; then
    LOCAL_MARIADB=1; say "  - MariaDB installed directly ($(mariadb --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+-MariaDB' | sed -n 1p || true))"
  elif echo "$procs" | grep -q '"mysqld"' || pgrep -x mysqld >/dev/null 2>&1; then
    LOCAL_MYSQL=1; say "  - MySQL installed directly (SQL backups; physical backups support MariaDB only)"
  fi
  if echo "$procs" | grep -q '"postgres"' || pgrep -x postgres >/dev/null 2>&1; then LOCAL_PG=1; say "  - PostgreSQL installed directly"; fi
  if echo "$procs" | grep -q '"redis-server"'; then say "  - Redis on $(echo "$procs" | awk '/"redis-server"/ {print $4; exit}')"; fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    HAS_DOCKER=1
    local c; c="$(docker ps --format '{{.Names}} ({{.Image}})' 2>/dev/null | grep -iE 'maria|mysql|postgres|redis' || true)"
    if [ -n "$c" ]; then echo "$c" | while read -r l; do say "  - database container: $l"; done; else say "  - Docker (no database containers running)"; fi
  fi
  local p u; for p in 80 443; do u="$(port_busy $p)"; [ -z "$u" ] || say "  - port $p is used by $u"; done
  say "  - free disk at $(existing_parent "$DATA_DIR"): $(df -h --output=avail "$(existing_parent "$DATA_DIR")" 2>/dev/null | tail -1 | tr -d ' ')"
  say "  - memory: $(awk '/MemTotal/ {printf "%.1f GB", $2/1048576}' /proc/meminfo)"
}
existing_parent() { local d="$1"; while [ ! -d "$d" ]; do d="$(dirname "$d")"; done; printf '%s' "$d"; }

load_existing() {
  local v
  v="$(sed -n 's/^BM_DATA_DIR=//p' "$ETC_DIR/env" | head -1)"; DATA_DIR="${v:-$DATA_DIR}"
  v="$(sed -n 's/^BM_LISTEN=.*:\([0-9]*\)$/\1/p' "$ETC_DIR/env" | head -1)"; PORT="${v:-$PORT}"
  if [ -z "$RUN_AS" ]; then
    if grep -q '^User=' "$UNIT" 2>/dev/null; then RUN_AS="$(sed -n 's/^User=//p' "$UNIT" | head -1)"; else RUN_AS=root; fi
  fi
  grep -q '^BM_TLS_DOMAIN=' "$ETC_DIR/env" && HTTPS_MODE=builtin || true
  grep -q '^BM_AGENT_SERVER=' "$ETC_DIR/env" && AGENT_SERVER="$(sed -n 's/^BM_AGENT_SERVER=//p' "$ETC_DIR/env")" || true
  grep -q -- '--allow-http' "$UNIT" 2>/dev/null && AGENT_HTTP=1 || true
  [ -n "$DOCKER_ACCESS" ] || { id -nG backupmgr 2>/dev/null | grep -qw docker && DOCKER_ACCESS=yes || DOCKER_ACCESS=no; }
  INSTALL_TOOLS="${INSTALL_TOOLS:-no}"
}

questions() {
  say ""
  if [ -n "$AGENT_SERVER" ]; then
    say "Installing as an ${B}agent${N} of the panel at $AGENT_SERVER (no panel on this machine)."
    case "$AGENT_SERVER" in
      https://*) ;;
      http://127.0.0.1*|http://localhost*) ;;
      http://*) [ "${AGENT_HTTP:-0}" = 1 ] || yesno "The panel address is plain http: database passwords will travel unprotected. Continue anyway?" n \
                  || die "use the panel's https:// address (or --agent-allow-http on a private network)"; AGENT_HTTP=1 ;;
      *) die "--agent-server must start with https://" ;;
    esac
  else
    # 1. how people reach the panel
    if [ -z "$DOMAIN" ] && [ "$PUBLIC" = 0 ] && interactive; then
      say "How will you open the panel?"
      say "  1) with a domain name and HTTPS (recommended; the domain must point to this server)"
      say "  2) only from this machine, through an SSH tunnel (safest, nothing opened)"
      say "  3) http://<server IP>:port without HTTPS (passwords travel unprotected)"
      local c; ask c "Choose 1, 2 or 3" "2"
      case "$c" in
        1) ask DOMAIN "Domain name (e.g. backups.example.com)" ""; [ -n "$DOMAIN" ] || die "a domain is needed for option 1"
           ask EMAIL "E-mail for certificate expiry notices (optional)" "" ;;
        3) PUBLIC=1 ;;
      esac
    fi
    if [ -n "$DOMAIN" ]; then
      case "$DOMAIN" in *.*) ;; *) die "'$DOMAIN' is not a domain name" ;; esac
      local u80 u443; u80="$(port_busy 80)"; u443="$(port_busy 443)"
      if [ -z "$u80" ] && [ -z "$u443" ]; then HTTPS_MODE=builtin
      elif [ "$u80" = nginx ] || [ "$u443" = nginx ]; then HTTPS_MODE=nginx
      else HTTPS_MODE=manual; fi
      local ip; ip="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1; exit}' || true)"
      [ -n "$ip" ] || note "$DOMAIN does not resolve yet; the certificate is made once it points to this server"
    fi
    ask PORT "Port of the panel on this machine" "$PORT"
    case "$PORT" in ''|*[!0-9]*) die "the port must be a number" ;; esac
    local busy; busy="$(port_busy "$PORT")"; [ -z "$busy" ] || die "port $PORT is already used by $busy; run again with --port N"
  fi
  ask DATA_DIR "Folder for the catalog and local backups" "$DATA_DIR"
  case "$DATA_DIR" in /*) ;; *) die "the folder must be an absolute path" ;; esac

  # 2. which account the service runs as
  if [ -z "$RUN_AS" ]; then
    RUN_AS=backupmgr
    if [ "$LOCAL_MARIADB" = 1 ] && interactive; then
      say ""
      say "A MariaDB runs directly on this machine. Physical (hot copy) backups of it must read its data folder,"
      say "which only root can do. SQL backups, folders you can read, Redis and databases in Docker do not need root."
      yesno "Run the service as root?" n && RUN_AS=root
    elif interactive; then
      say ""
      say "The service runs as a locked-down user 'backupmgr'. Folder backups can only read what that user may read"
      say "(not /root, not files only root can read). Choose root if you must back up such folders."
      yesno "Run the service as root instead?" n && RUN_AS=root
    fi
  fi

  # 3. Docker access
  if [ "$HAS_DOCKER" = 1 ] && [ "$RUN_AS" != root ] && [ -z "$DOCKER_ACCESS" ]; then
    DOCKER_ACCESS=no
    if interactive; then
      say ""
      say "Docker is installed. Access to it is needed for: databases in containers (physical backups, binary logs,"
      say "PostgreSQL), the row-search sandbox and restore tests. Access to Docker is as powerful as root."
      yesno "Let the service use Docker?" y && DOCKER_ACCESS=yes
    fi
  fi
  DOCKER_ACCESS="${DOCKER_ACCESS:-no}"

  # 4. tools for a database installed directly on this machine
  if [ "$LOCAL_MARIADB" = 1 ] && ! command -v mariabackup >/dev/null 2>&1 && [ -z "$INSTALL_TOOLS" ]; then
    INSTALL_TOOLS=no
    yesno "Install mariadb-backup (needed for physical backups of the local MariaDB)?" y && INSTALL_TOOLS=yes
  fi
  INSTALL_TOOLS="${INSTALL_TOOLS:-no}"

  # 5. a database user for backups
  if [ -z "$AGENT_SERVER" ] && [ -z "$CREATE_DB_USER" ] && { [ "$LOCAL_MARIADB" = 1 ] || [ "$LOCAL_MYSQL" = 1 ]; } && local_db_root_ok; then
    CREATE_DB_USER=no
    yesno "Create a database user 'bm_backup' on the local MariaDB/MySQL for backups?" y && CREATE_DB_USER=yes
  fi
  CREATE_DB_USER="${CREATE_DB_USER:-no}"
}

local_db_root_ok() { command -v mariadb >/dev/null 2>&1 && mariadb -N -e 'SELECT 1' >/dev/null 2>&1 || { command -v mysql >/dev/null 2>&1 && mysql -N -e 'SELECT 1' >/dev/null 2>&1; }; }

listen_addr() { if [ "$PUBLIC" = 1 ]; then printf '0.0.0.0:%s' "$PORT"; else printf '127.0.0.1:%s' "$PORT"; fi; }
public_url() {
  if [ -n "$DOMAIN" ]; then printf 'https://%s' "$DOMAIN"
  elif [ "$PUBLIC" = 1 ]; then printf 'http://%s:%s' "$(server_ip)" "$PORT"
  else printf 'http://127.0.0.1:%s' "$PORT"; fi
}

summary() {
  say ""
  say "${B}What will happen${N}"
  say "  - program       -> $BIN_PATH ($(program_source))"
  if [ "$UPGRADE" = 1 ]; then
    say "  - service       -> restarted with the new program; settings in $ETC_DIR kept"
    return
  fi
  if [ "$RUN_AS" = root ]; then say "  - runs as       -> root"; else say "  - runs as       -> system user 'backupmgr' (no login)"; fi
  [ "$DOCKER_ACCESS" = yes ] && [ "$RUN_AS" != root ] && say "  - Docker        -> 'backupmgr' joins the docker group"
  [ "$INSTALL_TOOLS" = yes ] && say "  - tools         -> install mariadb-backup"
  say "  - settings      -> $ETC_DIR (with the master key, readable by the service only)"
  say "  - data          -> $DATA_DIR"
  if [ -n "$AGENT_SERVER" ]; then
    say "  - agent of      -> $AGENT_SERVER (connects out; opens no port)"
  else
    say "  - panel         -> http://$(listen_addr)"
    case "$HTTPS_MODE" in
      builtin) say "  - HTTPS         -> https://$DOMAIN, certificate from Let's Encrypt (ports 80 and 443)" ;;
      nginx)   say "  - HTTPS         -> https://$DOMAIN through the nginx already running here" ;;
      manual)  say "  - HTTPS         -> ports 80/443 are used by $(port_busy 80)$(port_busy 443); you point it at 127.0.0.1:$PORT (docs/INSTALL.md)" ;;
    esac
  fi
  [ "$CREATE_DB_USER" = yes ] && say "  - database user -> bm_backup on the local MariaDB/MySQL"
  say "  - service       -> $NAME (starts on boot, restarts on failure)"
}

program_source() {
  if [ -n "$LOCAL_BINARY" ]; then echo "from $LOCAL_BINARY"
  elif [ "$FROM_SOURCE" = 1 ]; then echo "built here from source"
  else echo "release $VERSION of $REPO"; fi
}

# ---- getting the program ----

gh_get() { # gh_get URL OUTFILE [accept] — GitHub download, with the token when one is set
  local h=(-fsSL --retry 3 --connect-timeout 20)
  [ -n "$TOKEN" ] && h+=(-H "Authorization: Bearer $TOKEN")
  [ -n "${3:-}" ] && h+=(-H "Accept: $3")
  curl "${h[@]}" -o "$2" "$1"
}

get_program() {
  if [ -n "$LOCAL_BINARY" ]; then
    [ -f "$LOCAL_BINARY" ] || die "--binary: $LOCAL_BINARY not found"
    say "Using the program file $LOCAL_BINARY"
    run install -m 0755 "$LOCAL_BINARY" "$BIN_PATH"
  elif [ "$FROM_SOURCE" = 1 ]; then
    build_from_source
  elif ! download_release; then
    say ""
    note "no release could be downloaded (see above)."
    if [ -n "$SRC_DIR" ] || yesno "Build the program here from the source code instead (takes a few minutes)?" y; then
      build_from_source
    else
      die "nothing was installed"
    fi
  fi
  if [ "$DRY" = 0 ]; then
    "$BIN_PATH" version >/dev/null 2>&1 || die "the installed program does not run on this machine"
    command -v restorecon >/dev/null 2>&1 && restorecon -F "$BIN_PATH" 2>/dev/null || true
  fi
}

download_release() {
  local file="$NAME-linux-$ARCH" tmp
  say "Downloading $file ($VERSION) from $REPO…"
  if [ "$DRY" = 1 ]; then say "  [dry-run] download $file + SHA256SUMS, verify the checksum, install to $BIN_PATH"; return 0; fi
  tmp="$(mktemp -d)"
  local ok=1
  if [ -n "$TOKEN" ]; then
    # private repository: find the files through the API, then download them with the token
    local rel="https://api.github.com/repos/$REPO/releases/latest"
    [ "$VERSION" = latest ] || rel="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
    if gh_get "$rel" "$tmp/release.json" "application/vnd.github+json"; then
      local a s
      a="$(asset_url "$tmp/release.json" "$file" || true)"; s="$(asset_url "$tmp/release.json" SHA256SUMS || true)"
      if [ -n "$a" ] && [ -n "$s" ] && gh_get "$a" "$tmp/$file" application/octet-stream && gh_get "$s" "$tmp/SHA256SUMS" application/octet-stream; then ok=0; fi
    fi
  else
    local base="https://github.com/$REPO/releases/latest/download"
    [ "$VERSION" = latest ] || base="https://github.com/$REPO/releases/download/$VERSION"
    gh_get "$base/$file" "$tmp/$file" 2>/dev/null && gh_get "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null && ok=0
  fi
  if [ "$ok" != 0 ]; then
    rm -rf "$tmp"
    if [ -z "$TOKEN" ]; then note "download failed. If the repository is private, set GITHUB_TOKEN (and use sudo -E)."
    else note "download failed: the token cannot read the release, or the release $VERSION does not exist."; fi
    return 1
  fi
  if ! ( cd "$tmp" && grep " $file\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1 ); then
    rm -rf "$tmp"; die "checksum mismatch: the download is damaged or was tampered with. Nothing was installed."
  fi
  install -m 0755 "$tmp/$file" "$BIN_PATH.new" && mv -f "$BIN_PATH.new" "$BIN_PATH"
  rm -rf "$tmp"
  say "  checksum verified"
}

asset_url() { # asset_url release.json NAME -> API url of that asset (no JSON tool needed)
  tr ',' '\n' <"$1" | awk -v want="$2" '
    match($0, /"url": *"[^"]*\/releases\/assets\/[0-9]+"/) { u = substr($0, RSTART, RLENGTH); sub(/^"url": *"/, "", u); sub(/"$/, "", u) }
    match($0, /"name": *"[^"]*"/) { n = substr($0, RSTART, RLENGTH); sub(/^"name": *"/, "", n); sub(/"$/, "", n); if (n == want && u != "") { print u; exit } }'
}

build_from_source() {
  say "Building the program from source…"
  if [ "$DRY" = 1 ]; then say "  [dry-run] get the source and a Go toolchain, build, install to $BIN_PATH"; return; fi
  local work; work="$(mktemp -d /var/tmp/bm-build.XXXXXX)"
  local src="$SRC_DIR"
  if [ -z "$src" ]; then
    local url="https://github.com/$REPO/archive/refs/heads/$BRANCH.tar.gz"
    [ -n "$TOKEN" ] && url="https://api.github.com/repos/$REPO/tarball/$BRANCH"
    gh_get "$url" "$work/src.tgz" || { rm -rf "$work"; die "cannot download the source of $REPO (private? set GITHUB_TOKEN)"; }
    mkdir "$work/src" && tar -xzf "$work/src.tgz" -C "$work/src" --strip-components=1
    src="$work/src"
  fi
  local need goroot gobin
  need="$(sed -n 's/^go \([0-9.]*\).*/\1/p' "$src/go.mod")"
  if command -v go >/dev/null 2>&1 && go_at_least "$(go env GOVERSION | sed 's/^go//')" "$need"; then
    gobin="$(command -v go)"
  else
    say "  getting a Go toolchain (about 70 MB)"
    local list ver sum
    curl -fsSL --retry 3 "https://go.dev/dl/?mode=json" -o "$work/go.json" || { rm -rf "$work"; die "cannot reach go.dev to get Go"; }
    ver="$(tr ',' '\n' <"$work/go.json" | sed -n 's/.*"version": *"\(go[0-9.]*\)".*/\1/p' | sed -n 1p || true)"
    list="$(tr -d '\n' <"$work/go.json" | tr '{' '\n' | grep "\"filename\": *\"$ver.linux-$ARCH.tar.gz\"" | sed -n 1p || true)"
    sum="$(printf '%s' "$list" | sed -n 's/.*"sha256": *"\([0-9a-f]*\)".*/\1/p')"
    [ -n "$sum" ] || { rm -rf "$work"; die "cannot find Go $ver for linux-$ARCH"; }
    curl -fsSL --retry 3 "https://go.dev/dl/$ver.linux-$ARCH.tar.gz" -o "$work/go.tgz"
    echo "$sum  $work/go.tgz" | sha256sum -c - >/dev/null 2>&1 || { rm -rf "$work"; die "the Go download failed its checksum"; }
    tar -xzf "$work/go.tgz" -C "$work"
    goroot="$work/go"; gobin="$goroot/bin/go"
  fi
  # small servers: building needs about 1 GB; add a temporary swap file when memory is short
  local avail swapf=""
  avail="$(awk '/MemAvailable|SwapFree/ {s+=$2} END {print int(s/1024)}' /proc/meminfo)"
  if [ "$avail" -lt 1400 ] && [ "${BM_BUILD_SWAP:-yes}" != no ]; then
    swapf="/var/tmp/bm-build.swap"
    say "  only ${avail} MB of memory free: adding a temporary 2 GB swap file for the build"
    { fallocate -l 2G "$swapf" 2>/dev/null || dd if=/dev/zero of="$swapf" bs=1M count=2048 status=none; } && chmod 600 "$swapf" && mkswap "$swapf" >/dev/null && swapon "$swapf" || { rm -f "$swapf"; swapf=""; note "could not add swap; the build may run out of memory"; }
  fi
  local ver_text; ver_text="$(cat "$src/VERSION" 2>/dev/null || echo source)"
  say "  compiling (2–6 minutes on a small server)"
  local rc=0
  ( cd "$src" && GOPATH="$work/gopath" GOCACHE="$work/gocache" GOTOOLCHAIN=local CGO_ENABLED=0 GOFLAGS=-mod=mod \
      nice -n 10 "$gobin" build -p 1 -trimpath -ldflags "-s -w -X main.version=$ver_text" -o "$work/backup-manager" ./cmd/backup-manager ) >"$work/build.log" 2>&1 || rc=$?
  [ -z "$swapf" ] || { swapoff "$swapf" 2>/dev/null; rm -f "$swapf"; }
  if [ "$rc" != 0 ]; then tail -15 "$work/build.log" >&2; rm -rf "$work"; die "the build failed (log above)"; fi
  install -m 0755 "$work/backup-manager" "$BIN_PATH.new" && mv -f "$BIN_PATH.new" "$BIN_PATH"
  rm -rf "$work"
  say "  built $ver_text"
}

go_at_least() { # go_at_least HAVE NEED (dotted versions)
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]
}

# ---- setting it up ----

create_user_and_folders() {
  if [ "$RUN_AS" != root ]; then
    local nologin; nologin="$(command -v nologin || echo /usr/sbin/nologin)"
    id backupmgr >/dev/null 2>&1 || run useradd --system --home-dir "$DATA_DIR" --no-create-home --shell "$nologin" backupmgr
  fi
  local owner="${RUN_AS:-backupmgr}"
  run install -d -m 0750 -o "$owner" -g "$owner" "$ETC_DIR"
  run install -d -m 0750 -o "$owner" -g "$owner" "$DATA_DIR"
  run install -d -m 0750 -o "$owner" -g "$owner" "$DATA_DIR/backups"
}

write_env() {
  local e="# Backup Manager settings. Change, then: systemctl restart $NAME
BM_DATA_DIR=$DATA_DIR
BM_MASTER_KEY_FILE=$ETC_DIR/master.key
BM_REPO=$REPO
"
  if [ -n "$AGENT_SERVER" ]; then
    e+="BM_AGENT_SERVER=$AGENT_SERVER
BM_AGENT_TOKEN=$AGENT_TOKEN
"
  else
    e+="BM_LISTEN=$(listen_addr)
BM_PUBLIC_URL=$(public_url)
"
    if [ "$HTTPS_MODE" = builtin ]; then
      e+="BM_TLS_DOMAIN=$DOMAIN
"
      [ -z "$EMAIL" ] || e+="BM_TLS_EMAIL=$EMAIL
"
    fi
  fi
  write_file "$ETC_DIR/env" 0600 "$e"
  run chown "${RUN_AS:-backupmgr}:${RUN_AS:-backupmgr}" "$ETC_DIR/env"
}

write_unit() {
  local user_lines="" caps="" protect="ProtectHome=read-only" start="$BIN_PATH serve"
  [ "$RUN_AS" = root ] && protect="" || user_lines="User=backupmgr
Group=backupmgr"
  [ "$HTTPS_MODE" = builtin ] && [ "$RUN_AS" != root ] && caps="AmbientCapabilities=CAP_NET_BIND_SERVICE"
  # the agent reads BM_AGENT_TOKEN from the settings file; systemd fills in the server address
  [ -z "$AGENT_SERVER" ] || start="$BIN_PATH agent --server \${BM_AGENT_SERVER}$([ "${AGENT_HTTP:-0}" = 1 ] && echo ' --allow-http')"
  write_file "$UNIT" 0644 "[Unit]
Description=Backup Manager$([ -z "$AGENT_SERVER" ] || echo ' (agent)')
Documentation=https://github.com/$REPO
After=network-online.target docker.service
Wants=network-online.target

[Service]
$user_lines
EnvironmentFile=$ETC_DIR/env
ExecStart=$start
Restart=always
RestartSec=3
UMask=0077
$caps
# a backup must never starve the databases on the same machine
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
CPUWeight=50
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
$protect
ReadWritePaths=$DATA_DIR $ETC_DIR

[Install]
WantedBy=multi-user.target
"
}

grant_docker() {
  [ "$DOCKER_ACCESS" = yes ] && [ "$RUN_AS" != root ] || return 0
  getent group docker >/dev/null 2>&1 || { note "there is no docker group; Docker access was not granted"; return 0; }
  run usermod -aG docker backupmgr
}

install_tools() {
  [ "$INSTALL_TOOLS" = yes ] || return 0
  say "Installing mariadb-backup…"
  pkg_install mariadb-backup || note "mariadb-backup could not be installed; install the package that matches your MariaDB server"
}

wait_healthy() {
  [ "$DRY" = 0 ] || return 0
  if [ -n "$AGENT_SERVER" ]; then
    sleep 3
    systemctl is-active --quiet "$NAME" || { journalctl -u "$NAME" -n 15 --no-pager >&2 || true; die "the agent did not start (log above)"; }
    return 0
  fi
  local i
  for i in $(seq 1 60); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && return 0; sleep 0.5; done
  journalctl -u "$NAME" -n 20 --no-pager >&2 || true
  die "the service did not start (log above). Fix it and run the installer again."
}

setup_nginx() {
  [ "$HTTPS_MODE" = nginx ] || return 0
  local conf
  if [ -d /etc/nginx/sites-available ]; then conf=/etc/nginx/sites-available/$NAME.conf; else conf=/etc/nginx/conf.d/$NAME.conf; fi
  say "Adding $DOMAIN to nginx ($conf)…"
  write_file "$conf" 0644 "# Backup Manager panel — written by its installer
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;
    client_max_body_size 10m;
    location / {
        proxy_pass http://127.0.0.1:$PORT;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_buffering off;             # live progress and large downloads stream through
        proxy_read_timeout 3600s;
    }
}
"
  [ ! -d /etc/nginx/sites-enabled ] || run ln -sf "$conf" /etc/nginx/sites-enabled/$NAME.conf
  if [ "$DRY" = 0 ] && ! nginx -t >/dev/null 2>&1; then
    rm -f "$conf" /etc/nginx/sites-enabled/$NAME.conf
    note "nginx did not accept the new site (nginx -t); it was removed. Set it up by hand: docs/INSTALL.md"
    HTTPS_MODE=manual; return 0
  fi
  run systemctl reload nginx
  if ! command -v certbot >/dev/null 2>&1; then
    if yesno "Install certbot to get the HTTPS certificate?" y; then
      case "$PKG" in apt) pkg_install certbot python3-certbot-nginx ;; dnf|yum) pkg_install certbot python3-certbot-nginx ;; esac || true
    fi
  fi
  if command -v certbot >/dev/null 2>&1; then
    local mail=(--register-unsafely-without-email); [ -z "$EMAIL" ] || mail=(-m "$EMAIL")
    if run certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos "${mail[@]}" --redirect >/dev/null 2>&1; then
      say "  certificate installed; nginx renews it"
    else
      note "certbot could not get a certificate yet (does $DOMAIN point here?). Later: certbot --nginx -d $DOMAIN"
    fi
  else
    note "HTTPS is not set up yet: install certbot and run  certbot --nginx -d $DOMAIN"
  fi
}

open_firewall() {
  [ -z "$AGENT_SERVER" ] || return 0
  local ports=()
  [ "$HTTPS_MODE" = builtin ] && ports=(80 443)
  [ "$PUBLIC" = 1 ] && ports+=("$PORT")
  [ ${#ports[@]} -gt 0 ] || return 0
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q 'Status: active'; then
    if yesno "Open port(s) ${ports[*]} in the firewall (ufw)?" y; then for p in "${ports[@]}"; do run ufw allow "$p/tcp" >/dev/null; done; fi
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    if yesno "Open port(s) ${ports[*]} in the firewall (firewalld)?" y; then
      for p in "${ports[@]}"; do run firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done; run firewall-cmd --reload >/dev/null
    fi
  fi
}

DB_USER_PASS=""
create_db_user() {
  [ "$CREATE_DB_USER" = yes ] || return 0
  local cli=mariadb; command -v mariadb >/dev/null 2>&1 || cli=mysql
  if [ "$DRY" = 1 ]; then say "  [dry-run] create database user bm_backup with backup rights"; return 0; fi
  local_db_root_ok || { note "could not sign in to the local database as root; create the backup user by hand (docs/INSTALL.md)"; return 0; }
  DB_USER_PASS="$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  local base="SELECT, SHOW VIEW, TRIGGER, EVENT, PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT, REPLICATION SLAVE"
  local h
  for h in localhost 127.0.0.1; do
    "$cli" -e "CREATE USER IF NOT EXISTS 'bm_backup'@'$h' IDENTIFIED BY '$DB_USER_PASS'; ALTER USER 'bm_backup'@'$h' IDENTIFIED BY '$DB_USER_PASS'; GRANT $base ON *.* TO 'bm_backup'@'$h';" \
      || { note "creating the database user failed"; DB_USER_PASS=""; return 0; }
    "$cli" -e "GRANT SLAVE MONITOR ON *.* TO 'bm_backup'@'$h'" 2>/dev/null || true   # MariaDB 10.5.9+, for backups of replicas
  done
  umask 077; printf 'user=bm_backup\npassword=%s\nhost=127.0.0.1\n' "$DB_USER_PASS" >"$ETC_DIR/db-user.txt"; umask 022
}

finish() {
  say ""
  say "${G}${B}Backup Manager is installed.${N}  $([ "$DRY" = 1 ] && echo '(dry run: nothing was changed)')"
  [ "$DRY" = 1 ] && return 0
  say "Version: $("$BIN_PATH" version)"
  if [ -n "$AGENT_SERVER" ]; then
    say "This machine is now an agent of $AGENT_SERVER. In that panel: Databases & folders → Add → 'Runs on' this agent."
    say "Logs: journalctl -u $NAME -f"
    return 0
  fi
  if [ "$UPGRADE" = 1 ]; then
    say "Upgraded; all settings and backups were kept."
  else
    local link; link="$(as_service "$BIN_PATH" setup-link 2>/dev/null || true)"
    say ""
    say "${B}1. Create your account${N} — open this link once:"
    say "     $link"
    case "$HTTPS_MODE" in
      builtin) say "   (the certificate is fetched on the first visit; that first page can take a few seconds)" ;;
      manual)  say "   Point your web server for $DOMAIN at http://127.0.0.1:$PORT first (docs/INSTALL.md, 'Your own web server')." ;;
    esac
    if [ -z "$DOMAIN" ] && [ "$PUBLIC" = 0 ]; then
      say "   The panel listens on this machine only. From your computer run:"
      say "     ssh -L $PORT:127.0.0.1:$PORT root@$(server_ip)"
      say "   keep it open, then open the link above in your browser."
    fi
    [ "$PUBLIC" = 1 ] && note "the panel is on plain HTTP: anyone on the network path can read what you type."
    if [ -n "$DB_USER_PASS" ]; then
      say ""
      say "${B}2. Database user for backups${N} (also saved in $ETC_DIR/db-user.txt):"
      say "     host 127.0.0.1, port 3306, user bm_backup, password $DB_USER_PASS"
    fi
    say ""
    say "${B}Next${N}: add a database, a destination and a plan — guide: https://github.com/$REPO/blob/$BRANCH/docs/USER-GUIDE.md"
    say ""
    say "${Y}${B}Keep a copy of $ETC_DIR/master.key somewhere else.${N}${Y} Without it, encrypted backups cannot be read.${N}"
  fi
  say ""
  say "${B}Self-check${N} (backup-manager doctor):"
  as_service "$BIN_PATH" doctor 2>&1 | sed 's/^/  /' || true
  say ""
  say "Service: systemctl status $NAME   ·   Logs: journalctl -u $NAME -f   ·   Settings: $ETC_DIR/env"
}

write_file() { # write_file PATH MODE CONTENT
  if [ "$DRY" = 1 ]; then say "  [dry-run] write $1 (mode $2)"; return; fi
  local tmp; tmp="$(mktemp)"; printf '%s' "$3" >"$tmp"; install -m "$2" "$tmp" "$1"; rm -f "$tmp"
}

uninstall() {
  say "${B}Uninstall Backup Manager${N}"
  confirm "Stop the service and remove the program?" || die "stopped, nothing was changed"
  run systemctl disable --now "$NAME" 2>/dev/null || true
  run rm -f "$UNIT" "$BIN_PATH" "$BIN_PATH.old"
  run systemctl daemon-reload
  for f in /etc/nginx/sites-enabled/$NAME.conf /etc/nginx/sites-available/$NAME.conf /etc/nginx/conf.d/$NAME.conf; do
    if [ -e "$f" ]; then run rm -f "$f"; NGINX_TOUCHED=1; fi
  done
  [ -z "${NGINX_TOUCHED:-}" ] || { run systemctl reload nginx 2>/dev/null || true; say "Removed the nginx site (its certificate stays in certbot)."; }
  if [ -f "$ETC_DIR/env" ]; then local v; v="$(sed -n 's/^BM_DATA_DIR=//p' "$ETC_DIR/env" | head -1)"; DATA_DIR="${v:-$DATA_DIR}"; fi
  say "Kept: $ETC_DIR (master key) and $DATA_DIR (catalog and local backups)."
  if [ "$YES" = 0 ] && interactive && yesno "Also delete them? Local backups are lost for good, and without the key no backup can be read." n; then
    local word; ask word "Type DELETE to confirm" ""
    if [ "$word" = DELETE ]; then
      run rm -rf "$ETC_DIR" "$DATA_DIR"; run userdel backupmgr 2>/dev/null || true; say "Deleted."
    else say "Not deleted."; fi
  fi
  say "Done. The database user bm_backup (if one was made) is left in your database."
}

main "$@"
