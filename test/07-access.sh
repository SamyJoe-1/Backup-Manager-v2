#!/usr/bin/env bash
# Roles, API tokens, two-factor sign-in, command-line client, metrics, self-update. Standalone.
source "$(dirname "$0")/lib.sh"
mkdir -p "$RUN"; rm -rf "$RUN/data" "$RUN/keys" "$RUN/store" "$RUN/up"
start_manager || exit 1
BM_PASSWORD='correct-horse-9' "$BIN" admin create --email owner@example.com >/dev/null; login
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
as() { local jar="$1" m="$2" p="$3"; shift 3; curl -sS -b "$jar" -c "$jar" -X "$m" -H 'X-BM: 1' -H 'Content-Type: application/json' "$URL/api$p" "$@"; }
signin() { curl -s -o /dev/null -w '%{http_code}' -c "$1" -X POST -H 'X-BM: 1' "$URL/api/login" -d "{\"email\":\"$2\",\"password\":\"$3\",\"code\":\"${4:-}\"}"; }
totp() { python3 -c "
import hmac,hashlib,base64,struct,time,sys
k=base64.b32decode(sys.argv[1]+'='*((8-len(sys.argv[1])%8)%8)); c=int(time.time())//30+int(sys.argv[2])
h=hmac.new(k,struct.pack('>Q',c),hashlib.sha1).digest(); o=h[-1]&15
print('%06d'%((struct.unpack('>I',h[o:o+4])[0]&0x7fffffff)%1000000))" "$1" "${2:-0}"; }
T=$(call POST /targets -d '{"name":"test-db","kind":"mysql","cfg":{"host":"127.0.0.1","port":33999,"user":"bm_backup"},"secret":"bm-backup-pw"}' | js 'd["id"]')
D=$(call POST /destinations -d "{\"name\":\"local\",\"kind\":\"local\",\"cfg\":{\"path\":\"$RUN/store\"}}" | js 'd["id"]')
P=$(new_plan "{\"name\":\"small\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{\"content\":\"all\",\"tables\":[\"shop.stats\"]},\"schedule\":\"\",\"enabled\":true}")
PX=$(new_plan "{\"name\":\"broken\",\"targetId\":$T,\"destinationId\":$D,\"spec\":{\"content\":\"all\",\"tables\":[\"shop.nope\"]},\"schedule\":\"\",\"enabled\":true}")
B=$(run_plan $P); wait_backup $B 60 >/dev/null

echo "== roles"
eq "owner adds a viewer and an operator" "$(call POST /users -o /dev/null -w '%{http_code}' -d '{"email":"view@example.com","password":"viewer-pass-1","role":"viewer"}')$(call POST /users -o /dev/null -w '%{http_code}' -d '{"email":"op@example.com","password":"operator-pass-1","role":"operator"}')" 200200
eq "an unknown role is refused" "$(call POST /users -o /dev/null -w '%{http_code}' -d '{"email":"x@example.com","password":"viewer-pass-1","role":"god"}')" 400
V="$RUN/v.jar"; O="$RUN/o.jar"; signin "$V" view@example.com viewer-pass-1 >/dev/null; signin "$O" op@example.com operator-pass-1 >/dev/null
eq "viewer can look at backups" "$(as $V GET /backups -o /dev/null -w '%{http_code}')" 200
eq "viewer cannot run, download, browse rows or see settings" "$(as $V POST /plans/$P/run -o /dev/null -w '%{http_code}' -d '{}')$(as $V GET /backups/$B/download -o /dev/null -w '%{http_code}')$(as $V GET "/backups/$B/rows?db=shop&table=stats" -o /dev/null -w '%{http_code}')$(as $V GET /settings -o /dev/null -w '%{http_code}')" 403403403403
eq "operator can run and download" "$(as $O POST /plans/$P/run -o /dev/null -w '%{http_code}' -d '{}')$(as $O GET /backups/$B/download -o /dev/null -w '%{http_code}')" 200200
sleep 2
eq "operator cannot change plans, targets, accounts, delete or restore" "$(as $O POST /plans -o /dev/null -w '%{http_code}' -d '{}')$(as $O POST /targets -o /dev/null -w '%{http_code}' -d '{}')$(as $O GET /users -o /dev/null -w '%{http_code}')$(as $O DELETE /backups/$B -o /dev/null -w '%{http_code}')$(as $O POST /backups/$B/restore-table -o /dev/null -w '%{http_code}' -d '{}')$(as $O POST /users -o /dev/null -w '%{http_code}' -d '{"email":"evil@example.com","password":"viewer-pass-1","role":"owner"}')" 403403403403403403
eq "the refusal says why" "$(as $V POST /plans/$P/run -d '{}' | js '"viewer" in d["error"]')" True
OWNID=$(call GET /users | js '[u["id"] for u in d if u["email"]=="owner@example.com"][0]'); VID=$(call GET /users | js '[u["id"] for u in d if u["email"]=="view@example.com"][0]')
eq "the only owner cannot be demoted or deleted" "$(call PUT /users/$OWNID -o /dev/null -w '%{http_code}' -d '{"role":"viewer"}')$(call DELETE /users/$OWNID -o /dev/null -w '%{http_code}')" 400400
eq "deleting an account ends its session at once" "$(call DELETE /users/$VID -o /dev/null -w '%{http_code}')$(as $V GET /backups -o /dev/null -w '%{http_code}')" 200401

echo "== API tokens"
TV=$(call POST /tokens -d '{"name":"grafana","role":"viewer"}' | js 'd["token"]'); TO=$(call POST /tokens -d '{"name":"deploy","role":"operator"}' | js 'd["token"]')
eq "token is not stored in clear" "$(grep -a -c "$TO" "$RUN/data/catalog.db")" 0
eq "a token works without a browser session" "$(code -H "Authorization: Bearer $TV" $URL/api/backups)" 200
eq "viewer token cannot start a backup; operator token can" "$(code -X POST -H "Authorization: Bearer $TV" $URL/api/plans/$P/run -d '{}')$(code -X POST -H "Authorization: Bearer $TO" $URL/api/plans/$P/run -d '{}')" 403200
eq "a wrong token is refused" "$(code -H 'Authorization: Bearer bm_nope' $URL/api/backups)" 401
TID=$(call GET /tokens | js '[t["id"] for t in d if t["name"]=="grafana"][0]')
eq "a revoked token stops working" "$(call DELETE /tokens/$TID -o /dev/null -w '%{http_code}')$(code -H "Authorization: Bearer $TV" $URL/api/backups)" 200401

echo "== two-factor sign-in"
SECRET=$(call POST /me/totp/start -d '{}' | js 'd["secret"]')
eq "a wrong code does not switch it on" "$(call POST /me/totp/enable -o /dev/null -w '%{http_code}' -d '{"code":"000000"}')" 400
REC=$(call POST /me/totp/enable -d "{\"code\":\"$(totp $SECRET)\"}" | js '",".join(d["recovery"])')
eq "switching it on gives 8 recovery codes" "$(echo "$REC" | tr ',' '\n' | wc -l)" 8
eq "the key is not stored in clear" "$(grep -a -c "$SECRET" "$RUN/data/catalog.db")" 0
N="$RUN/n.jar"
eq "password alone no longer signs in" "$(signin $N owner@example.com correct-horse-9)" 401
eq "a wrong code is refused" "$(signin $N owner@example.com correct-horse-9 123456)" 401
eq "the right code signs in" "$(signin $N owner@example.com correct-horse-9 $(totp $SECRET))" 200
R1=$(echo "$REC" | cut -d, -f1)
eq "a recovery code signs in once, not twice" "$(signin $N owner@example.com correct-horse-9 $R1)$(signin $N owner@example.com correct-horse-9 $R1)" 200401
eq "switching it off needs the password" "$(call POST /me/totp/disable -o /dev/null -w '%{http_code}' -d '{"password":"wrong"}')$(call POST /me/totp/disable -o /dev/null -w '%{http_code}' -d '{"password":"correct-horse-9"}')" 403200
eq "after switching off, the password is enough again" "$(signin $N owner@example.com correct-horse-9)" 200
eq "password change needs the current one" "$(call POST /me/password -o /dev/null -w '%{http_code}' -d '{"current":"nope","new":"another-horse-10"}')" 403
eq "password change signs other devices out" "$(call POST /me/password -o /dev/null -w '%{http_code}' -d '{"current":"correct-horse-9","new":"another-horse-10"}')$(as $N GET /backups -o /dev/null -w '%{http_code}')$(call GET /backups -o /dev/null -w '%{http_code}')" 200401200

echo "== metrics and health"
eq "health check is open, metrics are not" "$(code $URL/healthz)$(code $URL/metrics)" 200401
M=$(curl -s -H "Authorization: Bearer $TO" $URL/metrics)
eq "metrics report backups and per-plan times" "$(echo "$M" | grep -c -E '^backup_manager_backups [1-9]|^backup_manager_plan_last_success_timestamp_seconds\{plan="small"\} [1-9]')" 2

echo "== command line"
CT=$("$BIN" token create --name cli --role operator 2>/dev/null)
export BM_URL="$URL" BM_TOKEN="$CT"
eq "plans lists the plans" "$("$BIN" plans | grep -c -E 'small|broken')" 2
OUT=$("$BIN" run --wait small 2>&1); RC=$?
eq "run --wait reports success with exit code 0" "$RC/$(echo "$OUT" | grep -c '^done:')" "0/1"
"$BIN" run --wait broken >/dev/null 2>&1; eq "run --wait on a failing plan exits non-zero" "$?" 1
eq "backups and jobs list" "$("$BIN" backups | grep -c ' done ')/$("$BIN" jobs | head -1 | cut -c1-3)" "$("$BIN" api GET '/backups?status=done' | js 'd["total"]')/JOB"
eq "without a token the client explains what to do" "$(BM_TOKEN= "$BIN" plans 2>&1 | grep -c 'token create')" 1
unset BM_URL BM_TOKEN

echo "== self-update"
mkdir -p "$RUN/up/rel" "$RUN/up/inst"; cp "$BIN" "$RUN/up/inst/backup-manager"; (cat "$BIN"; echo newer-build) > "$RUN/up/rel/backup-manager-linux-amd64"; echo 9.9.9 > "$RUN/up/rel/VERSION"
(cd "$RUN/up/rel" && sha256sum backup-manager-linux-amd64 > SHA256SUMS)
(cd "$RUN/up/rel" && exec python3 -m http.server 18760 --bind 127.0.0.1 >/dev/null 2>&1) & WS=$!; sleep 1
export BM_RELEASE_BASE=http://127.0.0.1:18760
X="$RUN/up/inst/backup-manager"; OLD=$(sha256sum < "$X")
eq "check sees the newer release without changing anything" "$("$X" upgrade --check | grep -c 9.9.9)/$([ "$(sha256sum < "$X")" = "$OLD" ] && echo same)" "1/same"
echo "0000000000000000000000000000000000000000000000000000000000000000  backup-manager-linux-amd64" > "$RUN/up/rel/SHA256SUMS"
"$X" upgrade >/dev/null 2>"$RUN/up/err"; eq "a download that does not match its checksum is refused, program untouched" "$?/$(grep -c 'checksum mismatch' "$RUN/up/err")/$([ "$(sha256sum < "$X")" = "$OLD" ] && echo same)" "1/1/same"
(cd "$RUN/up/rel" && sha256sum backup-manager-linux-amd64 > SHA256SUMS)
eq "upgrade replaces the program" "$("$X" upgrade | grep -c 'upgraded')/$([ "$(sha256sum < "$X")" = "$(sha256sum < "$RUN/up/rel/backup-manager-linux-amd64")" ] && echo new)" "1/new"
eq "the new program runs and the old one is kept" "$("$X" version)/$([ "$(sha256sum < "$X.old")" = "$OLD" ] && echo kept)" "0.1.0-dev/kept"
eq "a second upgrade says it is up to date" "$("$X" upgrade | grep -c 'already up to date')" 1
kill $WS 2>/dev/null; unset BM_RELEASE_BASE
stop_manager
echo; echo "07: $PASS passed, $FAIL failed"; [ "$FAIL" = 0 ]
