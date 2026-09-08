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
from authentik.core.models import Application, Group
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

# ---------------------------------------------------------------- scope mapping
scope, created = ScopeMapping.objects.get_or_create(
    scope_name="meshsat",
    defaults={
        "name": "MeshSat groups",
        "description": "MeshSat Hub roles",
        "expression": 'return {"groups": [g.name for g in request.user.ak_groups.all() if g.name.startswith("meshsat-")]}',
    },
)
note(f"scope mapping meshsat {'created' if created else 'ok'}")

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
wanted = list(ScopeMapping.objects.filter(scope_name__in=["openid", "profile", "email"])) + [scope]
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
    prompt("meshsat-enroll-matrix", "attributes.matrix_id", "Your Matrix ID", FieldTypes.TEXT, 60, placeholder="@you:example.org",
           sub_text=f"Beta support happens in the MeshSat room: {MATRIX_ROOM}"),
    prompt("meshsat-enroll-joined-matrix", "attributes.joined_matrix", "I have joined the MeshSat Matrix room", FieldTypes.CHECKBOX, 70),
]
p_marker = [prompt("meshsat-enroll-verified-marker", "attributes.email_verified", "verified", FieldTypes.HIDDEN, 10, required=False, initial="true")]

VALIDATION_EXPR = (
    'data = request.context.get("prompt_data", {}) or {}\n'
    'nested = data.get("attributes") if isinstance(data.get("attributes"), dict) else {}\n'
    'mx = (data.get("attributes.matrix_id") or nested.get("matrix_id") or "").strip()\n'
    'joined = data.get("attributes.joined_matrix", nested.get("joined_matrix"))\n'
    'if not regex_match(mx, r"^@[^:\\s]+:[^\\s]+$"):\n'
    '    ak_message("Enter your Matrix ID as @user:server")\n'
    '    return False\n'
    'if joined is not True and str(joined).lower() not in ("true", "on", "1"):\n'
    '    ak_message("Please join the MeshSat Matrix room first; that is where beta support happens")\n'
    '    return False\n'
    'return True\n'
)
validation, _ = ExpressionPolicy.objects.get_or_create(
    name="meshsat-enrollment-details-valid", defaults={"expression": VALIDATION_EXPR},
)
if validation.expression != VALIDATION_EXPR:
    validation.expression = VALIDATION_EXPR
    validation.save()


def prompt_stage(name, prompts, policies=()):
    st, _ = PromptStage.objects.get_or_create(name=name)
    st.fields.set(prompts)
    st.validation_policies.set(policies)
    return st


st_account = prompt_stage("meshsat-enrollment-account", p_account)
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

st_email, _ = EmailStage.objects.get_or_create(
    name="meshsat-enrollment-email",
    defaults={"use_global_settings": True, "activate_user_on_success": False,
              "subject": "Verify your email for MeshSat Hub", "template": "email/account_confirmation.html"},
)
st_email.use_global_settings = True
st_email.activate_user_on_success = False
st_email.subject = "Verify your email for MeshSat Hub"
st_email.save()

st_write_marker, _ = UserWriteStage.objects.get_or_create(
    name="meshsat-enrollment-write-verified",
    defaults={"user_creation_mode": UserCreationMode.NEVER_CREATE, "create_users_as_inactive": True},
)
st_write_marker.user_creation_mode = UserCreationMode.NEVER_CREATE
st_write_marker.save()

enroll, e_created = Flow.objects.get_or_create(
    slug="meshsat-enrollment",
    defaults={"name": "MeshSat Hub beta access", "title": "Request beta access to MeshSat Hub",
              "designation": FlowDesignation.ENROLLMENT},
)
enroll.title = "Request beta access to MeshSat Hub"
enroll.designation = FlowDesignation.ENROLLMENT
enroll.save()
for order, st in ((10, st_account), (20, st_details), (30, st_write), (40, st_email), (50, st_marker), (60, st_write_marker)):
    b, _ = FlowStageBinding.objects.get_or_create(target=enroll, stage=st, defaults={"order": order})
    if b.order != order:
        b.order = order
        b.save()
FlowStageBinding.objects.filter(target=enroll).exclude(stage__in=[st_account, st_details, st_write, st_email, st_marker, st_write_marker]).delete()
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
email_transport = NotificationTransport.objects.filter(mode=TransportMode.EMAIL).first()

matcher, _ = EventMatcherPolicy.objects.get_or_create(
    name="meshsat-user-created",
    defaults={"action": EventAction.MODEL_CREATED, "model": "authentik_core.user"},
)
rule, r_created = NotificationRule.objects.get_or_create(
    name="meshsat-new-signup",
    defaults={"severity": NotificationSeverity.NOTICE, "destination_group": groups["meshsat-platform-admin"]},
)
rule.destination_group = groups["meshsat-platform-admin"]
rule.save()
rule.transports.set([t for t in (transport, email_transport) if t])
PolicyBinding.objects.get_or_create(policy=matcher, target=rule, defaults={"order": 0, "enabled": True})
note(f"notification rule meshsat-new-signup {'created' if r_created else 'ok'}")

print("---MESHSAT_BOOTSTRAP_LOG---")
for line in log:
    print(line)
print("---MESHSAT_OIDC_CONFIG---")
print(f"HUB_OIDC_CLIENT_ID={provider.client_id}")
print(f"HUB_OIDC_CLIENT_SECRET={provider.client_secret}")
print(f"ISSUER=https://auth.meshsat.net/application/o/{app.slug}/")
print(f"ENROLLMENT=https://auth.meshsat.net/if/flow/{enroll.slug}/")
print("---END---")
