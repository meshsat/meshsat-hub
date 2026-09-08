#!/usr/bin/env bash
# Admit an approved MeshSat beta tester's address on the three VPS (MESHSAT-978).
# Adds the address to the running HAProxy (no reload) and to
# /etc/haproxy/meshsat-whitelist.lst so it survives reloads; idempotent.
# Usage: whitelist-ip.sh <ip> [<ip> ...]      (needs SCANNER_SUDO_PASS in the env,
#        the sudo recipe from MESHSAT-784; hosts: notrf01vps01 chzrh01vps01 txhou01vps01)
#        whitelist-ip.sh --remove <ip>          removes it again
set -euo pipefail
HOSTS="${VPS_HOSTS:-notrf01vps01 chzrh01vps01 txhou01vps01}"
FILE=/etc/haproxy/meshsat-whitelist.lst
SOCK=/run/haproxy/admin.sock
: "${SCANNER_SUDO_PASS:?export SCANNER_SUDO_PASS}"
mode=add; [ "${1:-}" = "--remove" ] && { mode=del; shift; }
[ $# -ge 1 ] || { echo "usage: $0 [--remove] <ip> [...]"; exit 2; }
for ip in "$@"; do
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$ || "$ip" =~ ^[0-9a-fA-F:]+(/[0-9]+)?$ ]] || { echo "not an address: $ip"; exit 2; }
  for h in $HOSTS; do
    printf '%s\n' "$SCANNER_SUDO_PASS" | ssh -o BatchMode=yes "$h" "sudo -S -p '' bash -s" <<EOS
set -e
touch $FILE
if [ "$mode" = add ]; then
  grep -qxF "$ip" $FILE || echo "$ip" >> $FILE
  echo "add acl $FILE $ip" | socat stdio unix-connect:$SOCK >/dev/null
else
  sed -i "\\#^$ip\$#d" $FILE
  echo "del acl $FILE $ip" | socat stdio unix-connect:$SOCK >/dev/null || true
fi
echo "\$(hostname): $mode $ip -> \$(echo "show acl $FILE" | socat stdio unix-connect:$SOCK | grep -c "$ip") entries"
EOS
  done
done
