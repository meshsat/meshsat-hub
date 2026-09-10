# MeshSat Hub authentik bootstrap (MESHSAT-936). Run through run-bootstrap.sh,
# which prepends MESHSAT_CSS and WEBHOOK_URL and pipes the result into
# `ak shell` on the authentik worker. Idempotent: every object is get_or_create
# by slug/name and re-pointed if it drifted. Never touches the omoikane Brand.
#
# Creates:
#   groups meshsat-platform-admin/owner/operator/viewer/pending (attribute meshsat_role)
#   ScopeMapping `meshsat` -> groups claim (names starting meshsat-)
#   OAuth2Provider "MeshSat Hub" (confidential, RS256, sub=user_uuid, STRICT redirect)
#   Application meshsat-hub
#   enrollment flow meshsat-enrollment (prompts, inactive user in meshsat-pending,
#     email verification, verified marker)
#   authentication flow meshsat-authentication (identification with the enrollment link)
#   Brand meshsat.net (title/logo/favicon/custom CSS, flows)
#   NotificationTransport webhook -> n8n, NotificationRule on user creation
# Prints the ---MESHSAT_OIDC_CONFIG--- block that run-bootstrap.sh stores in OpenBao.
import secrets

from authentik.brands.models import Brand
from authentik.core.models import Application, Group, User
from authentik.crypto.models import CertificateKeyPair
from authentik.events.models import (
    EventAction, NotificationRule, NotificationSeverity, NotificationTransport,
    NotificationWebhookMapping, TransportMode,
)
from authentik.flows.models import Flow, FlowDesignation, FlowStageBinding
from authentik.policies.event_matcher.models import EventMatcherPolicy
from authentik.policies.expression.models import ExpressionPolicy
from authentik.policies.models import PolicyBinding
from authentik.providers.oauth2.models import (
    ClientType, GrantType, OAuth2Provider, RedirectURI, RedirectURIMatchingMode, ScopeMapping,
)
from authentik.stages.email.models import EmailStage
from authentik.stages.identification.models import IdentificationStage
from authentik.stages.password.models import PasswordStage
from authentik.stages.prompt.models import FieldTypes, Prompt, PromptStage
from authentik.stages.user_login.models import UserLoginStage
from authentik.stages.user_write.models import UserCreationMode, UserWriteStage

HUB_URL = "https://hub.meshsat.net"
REDIRECT = f"{HUB_URL}/api/auth/oidc/callback"
MATRIX_ROOM = "https://matrix.to/#/#meshsat:matrix.nuclearlighters.net"
# authentik accounts that receive signup notifications and get the
# meshsat-platform-admin claim (Hub platform_admin). Operator accounts only.
PLATFORM_ADMIN_USERNAMES = ("meshsat-admin",)  # dedicated MeshSat identity (admin@meshsat.net); never an omoikane account
try:
    MESHSAT_CSS  # noqa: F821  (prepended by run-bootstrap.sh)
except NameError:
    MESHSAT_CSS = ""
try:
    WEBHOOK_URL  # noqa: F821
except NameError:
    WEBHOOK_URL = "https://n8n.nuclearlighters.net/webhook/meshsat-signup"

log = []


def note(msg):
    log.append(msg)


# ---------------------------------------------------------------- groups
groups = {}
for name, role in (
    ("meshsat-platform-admin", "platform-admin"),
    ("meshsat-owner", "owner"),
    ("meshsat-operator", "operator"),
    ("meshsat-viewer", "viewer"),
    ("meshsat-pending", "pending"),
):
    g, created = Group.objects.get_or_create(name=name, defaults={"attributes": {"meshsat_role": role}})
    if g.attributes.get("meshsat_role") != role:
        g.attributes["meshsat_role"] = role
        g.save()
    groups[name] = g
    note(f"group {name} {'created' if created else 'ok'}")

# The signup NotificationRule delivers to the members of meshsat-platform-admin;
# authentik sends nothing for a rule whose destination group is empty (that
# is how the first test signup on 2026-09-08 produced no webhook call). The
# operators of the shared authentik are the MeshSat platform admins.
for username in PLATFORM_ADMIN_USERNAMES:
    u = User.objects.filter(username=username, is_active=True).first()
    if u is None:
        note(f"platform admin {username} MISSING in authentik")
        continue
    if not groups["meshsat-platform-admin"].users.filter(pk=u.pk).exists():
        groups["meshsat-platform-admin"].users.add(u)
        note(f"platform admin {username} added to meshsat-platform-admin")
    else:
        note(f"platform admin {username} ok")
    # Operator accounts never pass the enrollment flow, so they carry no
    # email_verified marker; the Hub refuses unverified emails at JIT.
    if u.attributes.get("email_verified") not in (True, "true"):
        u.attributes["email_verified"] = "true"
        u.save()
        note(f"platform admin {username} marked email_verified")

# ---------------------------------------------------------------- scope mapping
GROUPS_EXPR = 'return {"groups": [g.name for g in request.user.groups.all() if g.name.startswith("meshsat-")]}'
scope, created = ScopeMapping.objects.get_or_create(
    scope_name="meshsat",
    defaults={"name": "MeshSat groups", "description": "MeshSat Hub roles", "expression": GROUPS_EXPR},
)
if scope.expression != GROUPS_EXPR:
    scope.expression = GROUPS_EXPR
    scope.save()
note(f"scope mapping meshsat {'created' if created else 'ok'}")

# authentik's default 'email' mapping returns email_verified: False for everyone; the Hub
# refuses unverified emails at JIT (provision error). This mapping reports the marker the
# enrollment flow writes after the email stage (attributes.email_verified).
EMAIL_EXPR = (
    'return {"email": request.user.email, '
    '"email_verified": request.user.attributes.get("email_verified") in (True, "true")}'
)
email_scope, e_created = ScopeMapping.objects.get_or_create(
    name="MeshSat email",
    defaults={"scope_name": "email", "description": "email + verified flag from the enrollment marker", "expression": EMAIL_EXPR},
)
if email_scope.expression != EMAIL_EXPR or email_scope.scope_name != "email":
    email_scope.expression = EMAIL_EXPR
    email_scope.scope_name = "email"
    email_scope.save()
note(f"scope mapping MeshSat email {'created' if e_created else 'ok'}")

# ---------------------------------------------------------------- provider + application
cert = CertificateKeyPair.objects.filter(name__icontains="authentik Self-signed").first() or CertificateKeyPair.objects.first()
auth_flow = (
    Flow.objects.filter(slug="default-provider-authorization-implicit-consent").first()
    or Flow.objects.filter(designation=FlowDesignation.AUTHORIZATION).first()
)
assert auth_flow is not None, "no authorization-designation flow"
invalidation = (
    Flow.objects.filter(slug="default-provider-invalidation-flow").first()
    or Flow.objects.filter(designation=FlowDesignation.INVALIDATION).first()
)
redirects = [RedirectURI(matching_mode=RedirectURIMatchingMode.STRICT, url=REDIRECT)]
provider, p_created = OAuth2Provider.objects.get_or_create(
    name="MeshSat Hub",
    defaults={
        "client_type": ClientType.CONFIDENTIAL,
        "client_id": secrets.token_urlsafe(32)[:40],
        "client_secret": secrets.token_urlsafe(48),
        "signing_key": cert,
        "authorization_flow": auth_flow,
        "invalidation_flow": invalidation,
        "include_claims_in_id_token": True,
        "sub_mode": "user_uuid",
        "redirect_uris": redirects,
    },
)
provider.redirect_uris = redirects
provider.client_type = ClientType.CONFIDENTIAL
# authentik 2026.x rejects /authorize with invalid_request unless the grant is listed here.
provider.grant_types = [GrantType.AUTHORIZATION_CODE, GrantType.REFRESH_TOKEN]
if provider.authorization_flow_id != auth_flow.pk:
    provider.authorization_flow = auth_flow
if invalidation and provider.invalidation_flow_id != invalidation.pk:
    provider.invalidation_flow = invalidation
if cert and not provider.signing_key_id:
    provider.signing_key = cert
provider.include_claims_in_id_token = True
provider.save()
wanted = list(ScopeMapping.objects.filter(scope_name__in=["openid", "profile"], managed__isnull=False)) + [email_scope, scope]
assert len(wanted) == 4, f"default scope mappings missing: {[s.scope_name for s in wanted]}"
provider.property_mappings.set(wanted)
note(f"provider MeshSat Hub {'created' if p_created else 'ok'}")

app, a_created = Application.objects.get_or_create(
    slug="meshsat-hub",
    defaults={"name": "MeshSat Hub", "provider": provider, "meta_launch_url": HUB_URL + "/", "open_in_new_tab": False},
)
if app.provider_id != provider.pk:
    app.provider = provider
    app.save()
note(f"application meshsat-hub {'created' if a_created else 'ok'}")


# ---------------------------------------------------------------- enrollment flow
def prompt(name, field_key, label, ptype, order, required=True, placeholder="", sub_text="", initial=""):
    p, _ = Prompt.objects.get_or_create(
        name=name,
        defaults={"field_key": field_key, "label": label, "type": ptype, "order": order,
                  "required": required, "placeholder": placeholder, "sub_text": sub_text, "initial_value": initial},
    )
    changed = False
    for k, v in (("field_key", field_key), ("label", label), ("type", ptype), ("order", order),
                 ("required", required), ("placeholder", placeholder), ("sub_text", sub_text), ("initial_value", initial)):
        if getattr(p, k) != v:
            setattr(p, k, v)
            changed = True
    if changed:
        p.save()
    return p


p_account = [
    prompt("meshsat-enroll-username", "username", "Username", FieldTypes.USERNAME, 10, placeholder="callsign or handle"),
    prompt("meshsat-enroll-name", "name", "Full name", FieldTypes.TEXT, 20),
    prompt("meshsat-enroll-email", "email", "Email", FieldTypes.EMAIL, 30, sub_text="We send a verification link and the approval notice here."),
    prompt("meshsat-enroll-password", "password", "Password", FieldTypes.PASSWORD, 40, placeholder="12+ characters"),
    prompt("meshsat-enroll-password-repeat", "password_repeat", "Password (repeat)", FieldTypes.PASSWORD, 50),
]
p_details = [
    prompt("meshsat-enroll-organisation", "attributes.organisation", "Organisation or team", FieldTypes.TEXT, 10, placeholder="SAR unit, club, company, or just you"),
    prompt("meshsat-enroll-country", "attributes.country", "Country", FieldTypes.TEXT, 20),
    prompt("meshsat-enroll-callsign", "attributes.callsign", "Amateur radio callsign (optional)", FieldTypes.TEXT, 30, required=False),
    prompt("meshsat-enroll-hardware", "attributes.hardware", "Hardware you have or plan to use", FieldTypes.TEXT, 40, placeholder="MeshSat field kit, Pi bridge, Android, RockBLOCK, Meshtastic nodes"),
    prompt("meshsat-enroll-intended-use", "attributes.intended_use", "What do you want to do with MeshSat Hub?", FieldTypes.TEXT_AREA, 50),
    prompt("meshsat-enroll-matrix", "attributes.matrix_id", "Matrix ID (optional)", FieldTypes.TEXT, 60, required=False,
           placeholder="@you:example.org",
           sub_text=f"Support happens in the MeshSat room: {MATRIX_ROOM}. Give us your ID and we can reach you there."),
    prompt("meshsat-enroll-terms", "attributes.accepted_terms", "I agree to the Terms of Service and the Privacy Policy",
           FieldTypes.CHECKBOX, 70,
           sub_text="https://meshsat.net/terms/ and https://meshsat.net/privacy/"),
]
p_marker = [prompt("meshsat-enroll-verified-marker", "attributes.email_verified", "verified", FieldTypes.HIDDEN, 10, required=False, initial="true")]

# Matrix stopped being a gate at public launch (MESHSAT-995). Requiring someone
# to join a chat room before they may ask for an account is friction we cannot
# enforce anyway -- nobody checked the box against the room's member list. It is
# now an optional contact detail, validated for shape only when it is given.
#
# Agreeing to the terms is the gate that replaced it, and unlike the room this
# one is checkable: the acceptance and its timestamp go onto the user.
VALIDATION_EXPR = (
    'data = request.context.get("prompt_data", {}) or {}\n'
    'nested = data.get("attributes") if isinstance(data.get("attributes"), dict) else {}\n'
    'mx = (data.get("attributes.matrix_id") or nested.get("matrix_id") or "").strip()\n'
    'if mx and not regex_match(mx, r"^@[^:\\s]+:[^\\s]+$"):\n'
    '    ak_message("A Matrix ID looks like @you:example.org. Leave it empty if you would rather not.")\n'
    '    return False\n'
    'agreed = data.get("attributes.accepted_terms", nested.get("accepted_terms"))\n'
    'if agreed is not True and str(agreed).lower() not in ("true", "on", "1"):\n'
    '    ak_message("Please agree to the Terms of Service and the Privacy Policy to continue.")\n'
    '    return False\n'
    'return True\n'
)
validation, _ = ExpressionPolicy.objects.get_or_create(
    name="meshsat-enrollment-details-valid", defaults={"expression": VALIDATION_EXPR},
)
if validation.expression != VALIDATION_EXPR:
    validation.expression = VALIDATION_EXPR
    validation.save()


# Junk-address filter on the first stage (MESHSAT-995). Enrollment is public
# now, so the operator reviewing requests is the scarce resource: this exists to
# keep the queue readable, not to keep anyone out. It is deliberately not a
# security control -- every account still verifies its email and is approved by
# a person before it exists -- so it errs towards letting a real address through.
#
# The domain list comes in from run-bootstrap.sh (disposable-domains.txt) and is
# checked exact and one label up, so mail.mailinator.com is caught by
# mailinator.com without listing every subdomain.
EMAIL_EXPR_TMPL = (
    'BLOCKED = {domains}\n'
    'data = request.context.get("prompt_data", {{}}) or {{}}\n'
    'email = (data.get("email") or "").strip().lower()\n'
    'if "@" not in email:\n'
    '    return True\n'
    'domain = email.rsplit("@", 1)[1]\n'
    'parts = domain.split(".")\n'
    'candidates = [domain] + [".".join(parts[i:]) for i in range(1, max(1, len(parts) - 1))]\n'
    'if any(c in BLOCKED for c in candidates):\n'
    '    ak_message("That looks like a disposable address. Use one you can still read in a month, because that is where account and billing mail goes.")\n'
    '    return False\n'
    'if "." not in domain or domain.endswith("."):\n'
    '    ak_message("That email domain does not look right.")\n'
    '    return False\n'
    'return True\n'
)
_domains = sorted({
    line.strip().lower()
    for line in (DISPOSABLE_DOMAINS or "").splitlines()
    if line.strip() and not line.strip().startswith("#")
})
EMAIL_EXPR = EMAIL_EXPR_TMPL.format(domains=repr(frozenset(_domains)))
email_policy, _ = ExpressionPolicy.objects.get_or_create(
    name="meshsat-enrollment-email-allowed", defaults={"expression": EMAIL_EXPR},
)
if email_policy.expression != EMAIL_EXPR:
    email_policy.expression = EMAIL_EXPR
    email_policy.save()
note(f"disposable-email policy ok ({len(_domains)} domains)")


def prompt_stage(name, prompts, policies=()):
    st, _ = PromptStage.objects.get_or_create(name=name)
    st.fields.set(prompts)
    st.validation_policies.set(policies)
    return st


st_account = prompt_stage("meshsat-enrollment-account", p_account, [email_policy])
st_details = prompt_stage("meshsat-enrollment-details", p_details, [validation])
st_marker = prompt_stage("meshsat-enrollment-verified-marker", p_marker)

st_write, _ = UserWriteStage.objects.get_or_create(
    name="meshsat-enrollment-write",
    defaults={"user_creation_mode": UserCreationMode.ALWAYS_CREATE, "create_users_as_inactive": True,
              "create_users_group": groups["meshsat-pending"]},
)
st_write.user_creation_mode = UserCreationMode.ALWAYS_CREATE
st_write.create_users_as_inactive = True
st_write.create_users_group = groups["meshsat-pending"]
st_write.save()

# MeshSat mail must leave as MeshSat. The global settings on this authentik
# belong to omoikane, so a stage using them sends beta invitations from
# omoikane.coach and the recipient sees the wrong brand on the envelope
# (MESHSAT-970). These stages carry their own sender and relay.
MAIL = {
    "use_global_settings": False,
    "host": "smtp.nuclearlighters.net",
    "port": 25,
    "use_tls": False,
    "use_ssl": False,
    # "MeshSat Hub", not "MeshSat": the product the recipient signed up for is
    # the Hub, and the owner has twice had to point out mail going out as the
    # bare brand. It is what the From line shows in every mail client.
    "from_address": "MeshSat Hub <noreply@meshsat.net>",
    "timeout": 30,
}


def mail_stage(name, subject, template, **extra):
    """An EmailStage that sends as MeshSat rather than the shared default."""
    st, _ = EmailStage.objects.get_or_create(name=name, defaults={**MAIL, **extra, "subject": subject, "template": template})
    for k, v in {**MAIL, **extra, "subject": subject, "template": template}.items():
        setattr(st, k, v)
    st.save()
    return st


# MeshSat-branded, not authentik's stock template. The stock one carries the
# authentik logo, an authentik-blue button and a "Powered by authentik" footer,
# which is what a MeshSat Hub customer saw on the first mail they ever got from
# us. These live in k8s/scripts/authentik/templates/ and are mounted into the
# authentik worker at /templates (see that directory's README).
st_email = mail_stage("meshsat-enrollment-email", "Verify your email for MeshSat Hub",
                      "email/meshsat_account_confirmation.html", activate_user_on_success=False)

st_write_marker, _ = UserWriteStage.objects.get_or_create(
    name="meshsat-enrollment-write-verified",
    defaults={"user_creation_mode": UserCreationMode.NEVER_CREATE, "create_users_as_inactive": True},
)
st_write_marker.user_creation_mode = UserCreationMode.NEVER_CREATE
st_write_marker.save()

enroll, e_created = Flow.objects.get_or_create(
    slug="meshsat-enrollment",
    defaults={"name": "MeshSat Hub sign-up", "title": "Create your MeshSat Hub account",
              "designation": FlowDesignation.ENROLLMENT},
)
enroll.title = "Create your MeshSat Hub account"
enroll.designation = FlowDesignation.ENROLLMENT
enroll.save()
for order, st in ((10, st_account), (20, st_details), (30, st_write), (40, st_email), (50, st_marker), (60, st_write_marker)):
    b, _ = FlowStageBinding.objects.get_or_create(target=enroll, stage=st, defaults={"order": order})
    if b.order != order:
        b.order = order
        b.save()
# Prune bindings this script does not own, but never the captcha: it is bound
# further down and only when Turnstile keys are supplied, so pruning by this list
# would SILENTLY REMOVE a working CAPTCHA every time somebody ran bootstrap from
# a shell that happened not to export them.
_keep = [st_account, st_details, st_write, st_email, st_marker, st_write_marker]
from authentik.stages.captcha.models import CaptchaStage as _CaptchaStage
_captcha = _CaptchaStage.objects.filter(name="meshsat-enrollment-captcha").first()
if _captcha:
    _keep.append(_captcha)
FlowStageBinding.objects.filter(target=enroll).exclude(stage__in=_keep).delete()

# The address the signup came from is kept on the user (attributes.signup_ip).
# It was originally how approval knew which address to admit to the edge
# allowlist; the edge opened at public launch and there is no list any more
# (MESHSAT-995), so this is now a record rather than a key: it is what tells you
# that fifty requests came from one host. Bound to the user_write binding, the
# policy runs right before the user row is written; ingress-nginx sets
# X-Forwarded-For from the VPS/relay hop (proxy-real-ip-cidr, phase 0).
#
# It also stamps when the terms were agreed to. A checkbox with no date is not
# a record of anything: what makes consent auditable is knowing which version
# of the documents was live at that moment.
SIGNUP_IP_EXPR = (
    'from datetime import datetime, timezone\n'
    'meta = request.http_request.META\n'
    'ip = (meta.get("HTTP_X_FORWARDED_FOR") or "").split(",")[0].strip() or meta.get("REMOTE_ADDR", "")\n'
    'pd = request.context.setdefault("prompt_data", {})\n'
    'pd["attributes.signup_ip"] = ip\n'
    'pd["attributes.terms_accepted_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")\n'
    'return True\n'
)
# ---------------------------------------------------------------- captcha
# Bound ahead of the first prompt when Turnstile keys are supplied, skipped
# entirely when they are not, so this script stays runnable without them.
# Keys come from https://dash.cloudflare.com/?to=/:account/turnstile (the
# "Managed" widget for auth.meshsat.net); pass them to run-bootstrap.sh as
# TURNSTILE_SITE_KEY and TURNSTILE_SECRET.
if TURNSTILE_SITE_KEY and TURNSTILE_SECRET:
    from authentik.stages.captcha.models import CaptchaStage
    st_captcha, _ = CaptchaStage.objects.get_or_create(name="meshsat-enrollment-captcha")
    st_captcha.public_key = TURNSTILE_SITE_KEY
    st_captcha.private_key = TURNSTILE_SECRET
    st_captcha.js_url = "https://challenges.cloudflare.com/turnstile/v0/api.js"
    st_captcha.api_url = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
    # Interactive, because the widget is in Turnstile's "managed" mode and
    # Cloudflare decides per visitor whether to ask for a click. A
    # non-interactive stage renders no widget at all, so a visitor who IS
    # challenged has nothing to click and simply cannot enroll -- a failure
    # that lands only on the people Cloudflare finds suspicious and never on
    # us. Verified on production: the widget renders as a "Verify you are
    # human" checkbox, so non-interactive would have stranded them.
    st_captcha.interactive = True
    st_captcha.save()
    b, _ = FlowStageBinding.objects.get_or_create(target=enroll, stage=st_captcha, defaults={"order": 5})
    if b.order != 5:
        b.order = 5
        b.save()
    note("captcha stage bound at order 5")
else:
    note("captcha skipped: no TURNSTILE_SITE_KEY/TURNSTILE_SECRET supplied")

# ---------------------------------------------------------------- signups pause switch
# An off switch for enrollment, because "we are being flooded" is not a moment
# to be editing flows. The policy always refuses; what changes is whether its
# binding is enabled, which is one boolean:
#
#   ./run-bootstrap.sh pause     # enrollment answers with the message below
#   ./run-bootstrap.sh resume
#
# The launch plan called this HUB_SIGNUPS_PAUSED, a Hub environment variable.
# The Hub is not in the signup path at all -- authentik is -- so an env var
# there would have paused nothing. It lives where the flow lives.
#
# Created disabled, and a re-run never touches `enabled` again: re-running the
# bootstrap while signups are paused must not quietly reopen them.
PAUSE_EXPR = (
    'ak_message("MeshSat Hub is not accepting new access requests at the moment. '
    'Write to hello@meshsat.net and we will tell you when it opens again.")\n'
    'return False\n'
)
pause_policy, _ = ExpressionPolicy.objects.get_or_create(name="meshsat-enrollment-paused", defaults={"expression": PAUSE_EXPR})
if pause_policy.expression != PAUSE_EXPR:
    pause_policy.expression = PAUSE_EXPR
    pause_policy.save()
pause_binding, pause_created = PolicyBinding.objects.get_or_create(
    policy=pause_policy, target=enroll, defaults={"order": 0, "enabled": False},
)
note(f"pause switch {'created (open)' if pause_created else ('ARMED - signups are paused' if pause_binding.enabled else 'ok (open)')}")

signup_ip_policy, _ = ExpressionPolicy.objects.get_or_create(name="meshsat-enrollment-signup-ip", defaults={"expression": SIGNUP_IP_EXPR})
if signup_ip_policy.expression != SIGNUP_IP_EXPR:
    signup_ip_policy.expression = SIGNUP_IP_EXPR
    signup_ip_policy.save()
write_binding = FlowStageBinding.objects.get(target=enroll, stage=st_write)
PolicyBinding.objects.get_or_create(policy=signup_ip_policy, target=write_binding, defaults={"order": 0, "enabled": True})
note("signup_ip policy bound to the enrollment user_write stage")
note(f"flow meshsat-enrollment {'created' if e_created else 'ok'}")

# ---------------------------------------------------------------- authentication flow (brand-specific, links enrollment)
default_auth = Flow.objects.get(slug="default-authentication-flow")
pw_stage = PasswordStage.objects.filter(name="default-authentication-password").first() or PasswordStage.objects.first()
login_stage = UserLoginStage.objects.filter(name="default-authentication-login").first() or UserLoginStage.objects.first()
mfa_binding = FlowStageBinding.objects.filter(target=default_auth, stage__name="default-authentication-mfa-validation").first()

ident, _ = IdentificationStage.objects.get_or_create(
    name="meshsat-authentication-identification",
    defaults={"user_fields": ["email", "username"], "password_stage": pw_stage, "enrollment_flow": enroll,
              "case_insensitive_matching": True, "show_matched_user": False, "pretend_user_exists": True},
)
ident.user_fields = ["email", "username"]
ident.password_stage = pw_stage
ident.enrollment_flow = enroll
ident.case_insensitive_matching = True
ident.save()

authn, n_created = Flow.objects.get_or_create(
    slug="meshsat-authentication",
    defaults={"name": "MeshSat Hub sign-in", "title": "Sign in to MeshSat Hub", "designation": FlowDesignation.AUTHENTICATION},
)
authn.title = "Sign in to MeshSat Hub"
authn.designation = FlowDesignation.AUTHENTICATION
authn.save()
bindings = [(10, ident)]
if mfa_binding:
    bindings.append((30, mfa_binding.stage))
bindings.append((100, login_stage))
for order, st in bindings:
    b, _ = FlowStageBinding.objects.get_or_create(target=authn, stage=st, defaults={"order": order})
    if b.order != order:
        b.order = order
        b.save()
note(f"flow meshsat-authentication {'created' if n_created else 'ok'}")

# ---------------------------------------------------------------- recovery flow
# Everyone signs in through authentik and authentik holds the password, so a
# forgotten one is its problem to solve. Nothing existed: the brand set
# authentication, invalidation and user settings but never flow_recovery, so
# "Forgot password?" did not render and a locked-out beta tester needed an
# administrator. Needs working mail, which is why it comes after MESHSAT-970.
p_recovery = [
    prompt("meshsat-recovery-password", "password", "New password", FieldTypes.PASSWORD, 10, placeholder="12+ characters"),
    prompt("meshsat-recovery-password-repeat", "password_repeat", "New password (repeat)", FieldTypes.PASSWORD, 20),
]
st_recovery_prompt = prompt_stage("meshsat-recovery-password-prompt", p_recovery)

# pretend_user_exists keeps this from telling a stranger which addresses are
# registered: an unknown one gets the same "check your mail" as a known one.
st_recovery_ident, _ = IdentificationStage.objects.get_or_create(
    name="meshsat-recovery-identification",
    defaults={"user_fields": ["email", "username"], "case_insensitive_matching": True, "pretend_user_exists": True},
)
st_recovery_ident.user_fields = ["email", "username"]
st_recovery_ident.case_insensitive_matching = True
st_recovery_ident.pretend_user_exists = True
st_recovery_ident.save()

st_recovery_email = mail_stage("meshsat-recovery-email", "Reset your MeshSat Hub password",
                               "email/meshsat_password_reset.html", activate_user_on_success=True)

st_recovery_write, _ = UserWriteStage.objects.get_or_create(
    name="meshsat-recovery-write",
    defaults={"user_creation_mode": UserCreationMode.NEVER_CREATE},
)
st_recovery_write.user_creation_mode = UserCreationMode.NEVER_CREATE
st_recovery_write.save()

recovery, r_created = Flow.objects.get_or_create(
    slug="meshsat-recovery",
    defaults={"name": "MeshSat Hub password reset", "title": "Reset your MeshSat Hub password",
              "designation": FlowDesignation.RECOVERY},
)
recovery.title = "Reset your MeshSat Hub password"
recovery.designation = FlowDesignation.RECOVERY
recovery.save()
for order, st in ((10, st_recovery_ident), (20, st_recovery_email), (30, st_recovery_prompt), (40, st_recovery_write)):
    b, _ = FlowStageBinding.objects.get_or_create(target=recovery, stage=st, defaults={"order": order})
    if b.order != order:
        b.order = order
        b.save()
FlowStageBinding.objects.filter(target=recovery).exclude(
    stage__in=[st_recovery_ident, st_recovery_email, st_recovery_prompt, st_recovery_write]).delete()

# The sign-in page only shows "Forgot password?" when its identification stage
# knows where to send it.
ident.recovery_flow = recovery
ident.save()
note(f"flow meshsat-recovery {'created' if r_created else 'ok'}")

# ---------------------------------------------------------------- Hub service account
# The Hub approves beta requests itself (MESHSAT-978), which needs an API
# identity of its own. The token is printed once so run-bootstrap.sh can store
# it; it is not regenerated on a re-run, so re-running this is safe.
from authentik.core.models import Token, TokenIntents, UserTypes  # noqa: E402

approver, a_created = User.objects.get_or_create(
    username="meshsat-hub-approver",
    defaults={"name": "MeshSat Hub approver", "type": UserTypes.SERVICE_ACCOUNT, "is_active": True},
)
approver.type = UserTypes.SERVICE_ACCOUNT
approver.is_active = True
approver.save()
admins = Group.objects.filter(name="authentik Admins").first()
if admins:
    approver.ak_groups.add(admins)
approver_token = Token.objects.filter(identifier="meshsat-hub-approver-token").first()
if approver_token is None:
    approver_token = Token.objects.create(
        identifier="meshsat-hub-approver-token", user=approver, intent=TokenIntents.INTENT_API,
        description="MeshSat Hub: approve beta requests", expiring=False,
    )
note(f"service account meshsat-hub-approver {'created' if a_created else 'ok'}")

# ---------------------------------------------------------------- brand
brand, b_created = Brand.objects.get_or_create(
    domain="meshsat.net",
    defaults={"default": False, "branding_title": "MeshSat Hub",
              "branding_logo": "https://meshsat.net/images/logo.svg", "branding_favicon": "https://meshsat.net/favicon.svg"},
)
brand.default = False
brand.branding_title = "MeshSat Hub"
brand.branding_logo = "https://meshsat.net/images/logo.svg"
brand.branding_favicon = "https://meshsat.net/favicon.svg"
if MESHSAT_CSS:
    brand.branding_custom_css = MESHSAT_CSS
brand.flow_authentication = authn
if invalidation:
    brand.flow_invalidation = Flow.objects.filter(slug="default-invalidation-flow").first() or invalidation
brand.flow_user_settings = Flow.objects.filter(slug="default-user-settings-flow").first()
brand.flow_recovery = recovery
brand.save()
note(f"brand meshsat.net {'created' if b_created else 'ok'}")

# ---------------------------------------------------------------- signup notification -> n8n (Matrix + YouTrack) and operator email
mapping, _ = NotificationWebhookMapping.objects.get_or_create(
    name="meshsat-signup-webhook",
    defaults={"expression": (
        'ctx = notification.event.context if notification.event else {}\n'
        'pk = (ctx.get("model") or {}).get("pk")\n'
        'u = ak_user_by(pk=pk) if pk else None\n'
        'user = {"pk": str(pk), "email": u.email, "username": u.username, "name": u.name,\n'
        '        "attributes": u.attributes, "groups": [g.name for g in u.ak_groups.all()]} if u else {}\n'
        'return {"event": "user_created", "severity": notification.severity, "user": user}\n'
    )},
)
transport, t_created = NotificationTransport.objects.get_or_create(
    name="meshsat-signup-n8n",
    defaults={"mode": TransportMode.WEBHOOK, "webhook_url": WEBHOOK_URL, "webhook_mapping_body": mapping, "send_once": True},
)
transport.mode = TransportMode.WEBHOOK
transport.webhook_url = WEBHOOK_URL
transport.webhook_mapping_body = mapping
transport.save()
# Deliberately NOT the shared email transport. A NotificationTransport in
# email mode has no sender of its own and falls back to the instance-wide
# AUTHENTIK_EMAIL__FROM, which belongs to omoikane, so MeshSat alerts went out
# as omoikane@nuclearlighters.net -- another brand's address on our mail, and
# the infrastructure domain in front of a reader who should never see it. The
# n8n transport reaches Matrix and YouTrack, which is where these are actually
# read, and it carries no sender at all.
if email_transport := NotificationTransport.objects.filter(mode=TransportMode.EMAIL).first():
    note(f"leaving the shared email transport ({email_transport.name}) off this rule; it has no sender of its own")

matcher, _ = EventMatcherPolicy.objects.get_or_create(
    name="meshsat-user-created",
    defaults={"action": EventAction.MODEL_CREATED, "model": "authentik_core.user"},
)

# A user object is created for plenty of reasons that are not somebody asking
# for beta access: service accounts, anything the API makes. Matching only the
# model and the action mailed the operator about all of them. A signup is an
# account that landed inactive in the pending group, so say that.
SIGNUP_ONLY_EXPR = (
    'event = request.context.get("event")\n'
    'ctx = getattr(event, "context", None) or {}\n'
    'pk = (ctx.get("model") or {}).get("pk")\n'
    'if not pk:\n'
    '    return False\n'
    'u = ak_user_by(pk=pk)\n'
    'if u is None or u.type == "service_account":\n'
    '    return False\n'
    'return u.ak_groups.filter(name="meshsat-pending").exists()\n'
)
signup_only, _ = ExpressionPolicy.objects.get_or_create(
    name="meshsat-is-a-signup", defaults={"expression": SIGNUP_ONLY_EXPR},
)
if signup_only.expression != SIGNUP_ONLY_EXPR:
    signup_only.expression = SIGNUP_ONLY_EXPR
    signup_only.save()
rule, r_created = NotificationRule.objects.get_or_create(
    name="meshsat-new-signup",
    defaults={"severity": NotificationSeverity.NOTICE, "destination_group": groups["meshsat-platform-admin"]},
)
rule.destination_group = groups["meshsat-platform-admin"]
rule.save()
rule.transports.set([transport])
PolicyBinding.objects.get_or_create(policy=matcher, target=rule, defaults={"order": 0, "enabled": True})
PolicyBinding.objects.get_or_create(policy=signup_only, target=rule, defaults={"order": 10, "enabled": True})
note(f"notification rule meshsat-new-signup {'created' if r_created else 'ok'}")

print("---MESHSAT_BOOTSTRAP_LOG---")
for line in log:
    print(line)
print("---MESHSAT_OIDC_CONFIG---")
print(f"HUB_OIDC_CLIENT_ID={provider.client_id}")
print(f"HUB_OIDC_CLIENT_SECRET={provider.client_secret}")
print("HUB_AUTHENTIK_TOKEN=" + approver_token.key)
print(f"ISSUER=https://auth.meshsat.net/application/o/{app.slug}/")
print(f"ENROLLMENT=https://auth.meshsat.net/if/flow/{enroll.slug}/")
print("---END---")
