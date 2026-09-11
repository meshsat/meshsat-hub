#!/usr/bin/env python3
"""Prove the shipped refund path on production, end to end, then purge.

This drives the Hub's own API and the real billing system. It creates one
tenant, one payment, one refund; watches the drainer produce a real credit note
with a real number; checks the PDF, the customer email, the plan and the
threshold meter; and then removes every artefact and re-asserts that the
counters are back where they started, so the first real customer document is
still a clean MSH2026-0001 / MSHCN2026-0001.
"""
import json, os, subprocess, sys, time, urllib.request, urllib.error

SP = os.path.dirname(os.path.abspath(__file__))
HUB = "https://hub.meshsat.net"
IN = "https://invoiceninja.nuclearlighters.net/api/v1"
IN_TOKEN = open(os.path.join(SP, ".in-token")).read().strip()
IN_PW = open(os.path.join(SP, ".in-password")).read().strip()

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("  PASS  " if ok else "  FAIL  ") + name + (("  -- " + str(detail)) if detail else ""))


def sh(*a, **kw):
    return subprocess.run(a, capture_output=True, text=True, **kw).stdout.strip()


CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-F|", "-c"]


def kubectl(*a):
    return sh("kubectl", "--context", "notrf01", "-n", "meshsat-hub", *a)


def hubpods():
    return [l.split("/")[1] for l in kubectl("get", "pods", "-o", "name").splitlines()
            if l.startswith("pod/hub-")]


def podenv(v):
    return sh(*CTX, "exec", hubpods()[0], "--", "printenv", v)


def psql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    bad = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if bad:
        print("   SQL ERR:", bad[-1][:160])
    return r.stdout.strip()


def api(method, path, body=None, token=None, tenant=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(HUB + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    if tenant:
        req.add_header("X-Tenant-ID", tenant)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw[:300]


def inja(method, path, body=None, password=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(IN + path, data=data, method=method)
    req.add_header("X-API-TOKEN", IN_TOKEN)
    req.add_header("X-Requested-With", "XMLHttpRequest")
    req.add_header("Accept", "application/json")
    if password:
        req.add_header("X-Api-Password", IN_PW)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read()
            if raw[:4] == b"%PDF":
                return r.status, raw
            return r.status, (json.loads(raw.decode()) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:400]


def main():
    admin = podenv("HUB_AUTH_TOKEN")
    if not admin:
        print("the Hub pod has no HUB_AUTH_TOKEN", file=sys.stderr)
        return 2

    tenant = "t-refund-e2e"
    # Deliver to the relay's own mailbox rather than a real address. Invoice
    # Ninja mails the invoice here and the Hub mails the credit note here, so
    # the two land side by side and the rehearsal can check they look like the
    # same company -- which is the thing that was wrong (MESHSAT-1019).
    email = os.environ.get("REHEARSAL_TO", "root@localhost")
    stamp = str(int(time.time()))

    mbox_before = sh("ssh", "-i", os.path.expanduser("~/.ssh/one_key"), "-o", "BatchMode=yes",
                     "root@nllei01smtp-dkim01", "wc -c < /var/mail/root 2>/dev/null || echo 0") or "0"

    print("=== 0. counters before ===")
    _, comp = inja("GET", "/companies/Wpmbk5ezJn")
    before = comp["data"]["settings"]
    inv_before = int(before["invoice_number_counter"])
    cr_before = int(before["credit_number_counter"])
    print(f"  invoice={inv_before} credit={cr_before}")

    print("=== 1. seed a paid tenant and a receipt the normal way ===")
    # A tenant with a country, so the VAT gate lets the receipt through, and an
    # owner user so the receipt has somewhere to go.
    psql(f"""INSERT INTO tenants (id, slug, name, plan, status, billing_country, created_at, updated_at)
             VALUES ('{tenant}', '{tenant}', 'Refund E2E', 'crew', 'active', 'NL', now(), now())
             ON CONFLICT (id) DO UPDATE SET plan='crew', billing_country='NL', status='active';""")
    psql(f"""UPDATE tenants SET plan_expires_at = now() + interval '32 days' WHERE id='{tenant}';""")
    expiry_before = psql(f"SELECT plan_expires_at FROM tenants WHERE id='{tenant}';")

    key = f"refund-e2e-{stamp}"
    psql(f"""INSERT INTO receipts (id, tenant_id, delivery_key, transaction_id, country, email, name,
                amount_cents, currency, plan, tier_name, paid_at, status, next_attempt_at,
                created_at, updated_at)
             VALUES ('rcpt-{stamp}', '{tenant}', '{key}', 'txn-{stamp}', 'NL', '{email}', 'Refund E2E',
                900, 'EUR', 'crew', 'Crew', now(), 'pending', now(), now(), now());""")
    receipt_id = f"rcpt-{stamp}"

    print("=== 2. wait for the receipt drainer to issue the invoice ===")
    inv_no = ""
    for _ in range(24):
        row = psql(f"SELECT status || '|' || invoice_number || '|' || invoice_ref FROM receipts WHERE id='{receipt_id}';")
        if row.startswith("issued|"):
            _, inv_no, inv_ref = row.split("|")
            break
        time.sleep(10)
    check("the payment produced an invoice", inv_no.startswith("MSH"), inv_no or row)
    if not inv_no:
        return finish(tenant, receipt_id, inv_before, cr_before)

    print("=== 3. record the refund through the admin API ===")
    st, body = api("POST", f"/api/admin/receipts/{receipt_id}/refund",
                   {"reason": "e2e rehearsal, 14-day withdrawal"}, token=admin, tenant="default")
    check("recording a refund returns 201", st == 201, f"{st} {body}")
    refund_id = (body or {}).get("id", "")
    check("the refund defaults to the whole payment",
          (body or {}).get("amount_cents") == 900, body)
    check("the refund inherits the payment's country and currency",
          (body or {}).get("country") == "NL" and (body or {}).get("currency") == "EUR", body)

    st, body = api("POST", f"/api/admin/receipts/{receipt_id}/refund", {}, token=admin, tenant="default")
    check("a second refund of the same payment is refused", st == 409, f"{st} {body}")

    st, body = api("POST", f"/api/admin/receipts/{receipt_id}/refund",
                   {"amount_cents": 5000}, token=admin, tenant="default")
    check("a refund larger than the payment is refused", st == 400, f"{st} {body}")

    print("=== 4. wait for the credit note ===")
    credit_no, credit_ref = "", ""
    for _ in range(24):
        row = psql(f"SELECT status || '|' || credit_number || '|' || credit_ref || '|' || last_error FROM refunds WHERE id='{refund_id}';")
        if row.startswith("issued|"):
            _, credit_no, credit_ref, _ = row.split("|", 3)
            break
        if row.startswith("blocked|"):
            break
        time.sleep(10)
    check("the refund produced a credit note", credit_no.startswith("MSHCN"), credit_no or row)

    print("=== 5. the document itself ===")
    if credit_ref:
        st, pdf = inja("GET", f"/credits/{credit_ref}/download")
        ok = st == 200 and isinstance(pdf, bytes) and pdf[:4] == b"%PDF"
        check("the credit note renders as a PDF", ok, st)
        if ok:
            open(os.path.join(SP, "credit-e2e.pdf"), "wb").write(pdf)
            text = sh("pdftotext", "-layout", os.path.join(SP, "credit-e2e.pdf"), "-")
            check("the PDF is a CREDIT carrying its number", "CREDIT" in text and credit_no in text)
            check("the PDF names the invoice it corrects", inv_no in text, inv_no)
            check("the VAT is reversed at the same split the invoice charged",
                  "7,44" in text and "1,56" in text and "9,00" in text)
            check("the PDF carries the VAT number and the legal entity",
                  "NL005411721B30" in text and "Elli.Z.G." in text)
            imgs = sh("pdfimages", "-list", os.path.join(SP, "credit-e2e.pdf"))
            check("exactly one image, so no whitelabel badge leaked back in",
                  len([l for l in imgs.splitlines() if l.strip().startswith("1 ")]) == 1)

    print("=== 6. the ledger nets to zero ===")
    st, invd = inja("GET", f"/invoices/{inv_ref}?include=payments")
    if st == 200:
        d = invd["data"]
        check("the invoice is settled, not left permanently unpaid", float(d.get("balance", -1)) == 0, d.get("balance"))
        refunded = sum(float(p.get("refunded", 0)) for p in d.get("payments", []))
        check("the money is recorded as having gone back out", refunded == 9.0, refunded)
    st, crd = inja("GET", f"/credits/{credit_ref}")
    if st == 200:
        check("the credit note is applied, not left sitting as an unused credit",
              float(crd["data"].get("balance", -1)) == 0, crd["data"].get("balance"))

    print("=== 7. the plan and the meter ===")
    expiry_after = psql(f"SELECT plan_expires_at FROM tenants WHERE id='{tenant}';")
    check("a full refund took back the paid period",
          expiry_after and expiry_after < expiry_before, f"{expiry_before} -> {expiry_after}")
    check("the refund did not downgrade the plan itself",
          psql(f"SELECT plan FROM tenants WHERE id='{tenant}';") == "crew")
    check("devices and bridges were not touched",
          psql(f"SELECT count(*) FROM devices WHERE tenant_id='{tenant}';") == "0")

    st, thr = api("GET", "/api/admin/vat/threshold", token=admin, tenant="default")
    check("the threshold meter reports refunds separately", st == 200 and "refunded_cents" in (thr or {}), thr)

    print("=== 8. the customer's copy ===")
    time.sleep(8)
    raw = sh("ssh", "-i", os.path.expanduser("~/.ssh/one_key"), "-o", "BatchMode=yes",
             "root@nllei01smtp-dkim01", f"tail -c +{int(mbox_before) + 1} /var/mail/root")
    msgs = [m for m in raw.split("\nFrom ") if m.strip()]
    print(f"  {len(msgs)} message(s), {len(raw)} bytes since the run started")

    receipt_msg = next((m for m in msgs if "invoices@" in m or inv_no in m), "")
    credit_msg = next((m for m in msgs if credit_no and credit_no in m and "Content-Type: application/pdf" in m), "")

    check("the customer received the credit note", bool(credit_msg),
          "subjects: " + "; ".join(l for m in msgs for l in m.split("\n") if l.startswith("Subject:")))
    if credit_msg:
        flat = credit_msg.replace("=\r\n", "").replace("=\n", "")
        check("it came from MeshSat, NOT the other company on the instance",
              "billing@meshsat.net" in credit_msg and "ellizg.com" not in credit_msg)
        check("it is DKIM signed for meshsat.net", "d=meshsat.net" in credit_msg)
        check("the PDF travels with the words that explain it",
              "Content-Type: application/pdf" in credit_msg and "JVBERi0" in credit_msg)
        check("the letter and the document are both shown, not offered as a choice",
              "multipart/mixed" in credit_msg and "multipart/alternative" in credit_msg)
        check("the Hub message carries the same brand shell as the receipt",
              "data:image/png;base64," in flat and
              ('alt=3D"MeshSat Hub"' in flat or 'alt="MeshSat Hub"' in flat))
        check("a plain-text reader still gets the whole message",
              "Content-Type: text/plain" in credit_msg and "The MeshSat team" in credit_msg)
        longest = max((len(l.rstrip("\r")) for l in credit_msg.split("\n")), default=0)
        check("no line over SMTP's 998 octets", longest <= 998, longest)

    if receipt_msg:
        rflat = receipt_msg.replace("=\r\n", "").replace("=\n", "")
        check("the receipt and the Hub notice use the SAME shell",
              ("data:image/png;base64," in rflat) ==
              ("data:image/png;base64," in credit_msg.replace("=\r\n", "").replace("=\n", "")),
              "receipt branded: %s" % ("data:image/png;base64," in rflat))
    else:
        print("  (no invoice email in the mailbox; Invoice Ninja may send it to the contact only)")
    return finish(tenant, receipt_id, inv_before, cr_before)


def finish(tenant, receipt_id, inv_before, cr_before):
    print("=== 9. purge every artefact ===")
    # The billing system first: purging the CLIENT takes its invoices, payments
    # and credits with it (Quirk 3, deletes are otherwise soft).
    st, found = inja("GET", "/clients?per_page=20&id_number=t-refund-e2e")
    purged = 0
    for c in (found or {}).get("data", []) if isinstance(found, dict) else []:
        # Belt and braces: never purge anything that is not the rehearsal.
        if c.get("id_number") != "t-refund-e2e":
            print(f"  !! refusing to purge client {c['id']} -- not the rehearsal")
            continue
        st, _ = inja("POST", f"/clients/{c['id']}/purge", {}, password=True)
        purged += 1
        print(f"  purged client {c['id']}: {st}")
    check("the rehearsal client was purged", purged >= 1, purged)
    # Only rewind the counters if the rehearsal is the ONLY thing that used
    # them. A real payment landing mid-run would have taken the next number,
    # and rewinding past it would hand a second customer the same invoice
    # number out of a gapless series. The infra doc's rule is the safe one:
    # counter == max(number) + 1 over whatever is actually left.
    _, remaining_inv = inja("GET", "/invoices?per_page=100")
    _, remaining_cr = inja("GET", "/credits?per_page=100")
    live_inv = [i for i in (remaining_inv or {}).get("data", []) if i.get("number")]
    live_cr = [c for c in (remaining_cr or {}).get("data", []) if c.get("number")]
    if live_inv or live_cr:
        print("  !! documents remain on the company; NOT rewinding the counters")
        for d in live_inv + live_cr:
            print("     left behind:", d.get("number"))
        check("counters left alone because real documents exist", True)
    else:
        _, comp = inja("GET", "/companies/Wpmbk5ezJn")
        s = dict(comp["data"]["settings"])
        s["invoice_number_counter"] = inv_before
        s["credit_number_counter"] = cr_before
        s["client_number_counter"] = 1
        s["payment_number_counter"] = 1
        inja("PUT", "/companies/Wpmbk5ezJn", {"settings": s})
    _, comp = inja("GET", "/companies/Wpmbk5ezJn")
    a = comp["data"]["settings"]
    if not (live_inv or live_cr):
        check("the invoice counter is back where it started",
              int(a["invoice_number_counter"]) == inv_before, a["invoice_number_counter"])
        check("the credit note counter is back where it started",
              int(a["credit_number_counter"]) == cr_before, a["credit_number_counter"])
    for res in ("clients", "invoices", "credits", "payments"):
        _, d = inja("GET", f"/{res}?per_page=100")
        n = len(d.get("data", [])) if isinstance(d, dict) else -1
        check(f"no {res} left on the company", n == 0, n)
    check("the branded wrapper survived the counter reset",
          "$body" in (a.get("email_style_custom") or "") and
          len(a.get("email_style_custom") or "") > 30000, len(a.get("email_style_custom") or ""))
    check("the credit-note template survived", bool(a.get("email_template_credit")))

    psql(f"DELETE FROM refunds WHERE tenant_id='{tenant}';")
    psql(f"DELETE FROM receipts WHERE tenant_id='{tenant}';")
    psql(f"DELETE FROM tenants WHERE id='{tenant}';")
    check("the rehearsal tenant is gone", psql(f"SELECT count(*) FROM tenants WHERE id='{tenant}';") == "0")

    print(f"\n=== {len(PASS)}/{len(PASS) + len(FAIL)} ===")
    for f in FAIL:
        print("  FAILED: " + f)
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
