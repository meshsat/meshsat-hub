# Edge (VPS HAProxy) changes for the k8s migration (MESHSAT-944)

`patch-haproxy.py <auth|cutover|rollback> <live haproxy.cfg>` rewrites the meshsat backends of
one VPS config. The live file is the source of truth (edge repo snapshots lag), so every
apply fetches it first. Three VPS, in the plan's order **NO → CH → TX**, one at a time,
`show stat` green before the next.

```
V=notrf01vps01   # then chzrh01vps01, txhou01vps01
ssh -i ~/.ssh/one_key kyriakosp@$V 'sudo -S -p "" cat /etc/haproxy/haproxy.cfg' <<< "$SUDO_PW" > /tmp/hap-$V.live
python3 patch-haproxy.py cutover /tmp/hap-$V.live --out /tmp/hap-$V.new     # diff on stderr
scp -i ~/.ssh/one_key /tmp/hap-$V.new kyriakosp@$V:/tmp/haproxy.cfg.new
ssh -i ~/.ssh/one_key kyriakosp@$V 'sudo -S -p "" bash -s' <<EOS
$SUDO_PW
set -e
cp /etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg.bak-$(date +%Y%m%d)-MESHSAT-944
cp /tmp/haproxy.cfg.new /etc/haproxy/haproxy.cfg
haproxy -c -f /etc/haproxy/haproxy.cfg
grep -n "meshsat_auth\|no-k8s" /etc/haproxy/haproxy.cfg | head
systemctl reload haproxy
echo "show stat" | socat stdio /run/haproxy/admin.sock | grep meshsat_ | cut -d, -f1,2,18
EOS
```

Never `echo pw | sudo -S tee` into the live file (it emptied haproxy.cfg on two VPS once,
MESHSAT-784 history). `haproxy -c` passing proves syntax only; the `show stat` UP/DOWN
column and an end-to-end request prove the wiring.

| phase | what changes |
|---|---|
| `auth` (phase 3) | `backend meshsat_auth` → the three worker mesh IPs `:8443` with `sni str(auth.meshsat.net)`, health `/-/health/live/`; `use_backend`, `nbsrv` silent-drop guard, `is_authenticated_site` and `tier5a_host` entries for `auth.meshsat.net` |
| `cutover` (phase 5) | `auth` + `meshsat_hub` → `:8443` x3 (`sni hub.meshsat.net`, `/healthz`), `meshsat_mqtt` → `:9443` x3, `meshsat_reticulum` → `:4243` x3 (TCP passthrough; Tier 5b reject line untouched) |
| `rollback` | DMZ backends for hub/mqtt/reticulum restored (NL primary, GR backup); `meshsat_auth` stays |

After the change: re-snapshot the three configs into `infrastructure/nllei01/production/edge/vps/<vps>/haproxy/haproxy.cfg` and update the hostname/backend table in `edge/CLAUDE.md` (phase 6).
