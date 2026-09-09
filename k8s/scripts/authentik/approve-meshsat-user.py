# Approve (or reject) a MeshSat Hub beta request (MESHSAT-936).
# Run through run-bootstrap.sh:  ./run-bootstrap.sh approve <email> <owner|operator|viewer>
#                                 ./run-bootstrap.sh reject  <email>
# Approve: activates the user, swaps meshsat-pending for the role group and
# mails the activation notice (Hub URL + Matrix room). Reject: deletes the
# inactive account. The first Hub login then creates the tenant (JIT, MR 14).
import sys

from django.core.mail import send_mail

from authentik.core.models import Group, User

try:
    ARGS  # noqa: F821  (prepended by run-bootstrap.sh)
except NameError:
    ARGS = sys.argv[1:]

action, email = ARGS[0], ARGS[1].strip().lower()
role = ARGS[2] if len(ARGS) > 2 else "owner"
assert action in ("approve", "reject"), action
assert role in ("owner", "operator", "viewer"), role

user = User.objects.filter(email__iexact=email).first()
assert user is not None, f"no user with email {email}"
pending = Group.objects.get(name="meshsat-pending")

if action == "reject":
    assert not user.is_active or pending in user.ak_groups.all(), "refusing to delete an active non-pending user"
    user.delete()
    print(f"rejected and deleted {email}")
else:
    verified = user.attributes.get("email_verified") in (True, "true")
    if not verified:
        print(f"WARNING: {email} has not completed email verification")
    target = Group.objects.get(name=f"meshsat-{role}")
    user.ak_groups.remove(pending)
    user.ak_groups.add(target)
    user.is_active = True
    user.attributes["meshsat_approved"] = True
    user.save()
    signup_ip = user.attributes.get("signup_ip") or ""
    if signup_ip:
        print(f"signed up from {signup_ip}")
    body = (
        f"Hi {user.name or user.username},\n\n"
        "Your MeshSat Hub beta access is approved.\n\n"
        "Sign in: https://hub.meshsat.net/ (Sign in with MeshSat ID)\n"
        "Support and the other beta testers: https://matrix.to/#/#meshsat:matrix.nuclearlighters.net\n"
        "Docs: https://meshsat.net/docs/\n\n"
        "Your first sign-in creates your organisation on the Hub; the Fleet page walks you through adding a bridge.\n\n"
        "MeshSat"
    )
    html = (
        '<p><img src="https://meshsat.net/images/logo.png" alt="MeshSat" width="240"></p>'
        f"<p>Hi {user.name or user.username},</p><p>Your <b>MeshSat Hub</b> beta access is approved.</p>"
        '<p><a href="https://hub.meshsat.net/">Sign in with MeshSat ID</a></p>'
        '<p>Support and the other beta testers: <a href="https://matrix.to/#/#meshsat:matrix.nuclearlighters.net">#meshsat on Matrix</a><br>'
        'Docs: <a href="https://meshsat.net/docs/">meshsat.net/docs</a></p>'
        "<p>Your first sign-in creates your organisation on the Hub; the Fleet page walks you through adding a bridge.</p>"
    )
    # The account is active from here; a mail failure must not look like a failed approval.
    try:
        sent = send_mail("Your MeshSat Hub beta access is approved", body, None, [user.email], html_message=html, fail_silently=False)
        print(f"approved {email} as {role}; activation email sent={sent}")
    except Exception as exc:  # noqa: BLE001
        print(f"approved {email} as {role}; activation email FAILED ({exc!r}) - tell them by hand")
