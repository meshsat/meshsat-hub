# MeshSat Hub email templates

authentik renders these; they are the source of truth and a copy is mounted into
the authentik worker.

## Why they exist

authentik's stock `email/account_confirmation.html` and `email/password_reset.html`
carry the authentik logo, an authentik-blue button and a "Powered by authentik"
footer. The confirmation is the **first message a MeshSat Hub customer ever
receives**, and nothing on it said MeshSat except the subject line.

## How they reach authentik

The authentik deployment lives in the omoikane repo, because that repo owns the
shared instance. `k8s/auth/kustomization.yaml` there has a `configMapGenerator`
entry that packages these two files, and `deployment-worker.yaml` mounts it at
`/templates/email` on the worker -- the component that renders email.

The mount is **additive**: `/templates` keeps its emptyDir and its drop-in
behaviour, and the ConfigMap only fills the `email/` subdirectory that the
EmailStage `template` paths are addressed by.

Copy edits here, then copy the file to `k8s/auth/templates/email/` in the
omoikane repo. Two copies is deliberate: this one is version-controlled beside
the bootstrap that references it, so a MeshSat rebuild has everything it needs.

## Constraints

- **Standalone, not `{% extends %}`.** Extending authentik's base pulls the stock
  chrome back in.
- **No remote resources.** The logo is a base64 data URI, so there is no tracking
  surface and the mail renders the same in a client that blocks images.
- Keep them free of the string "authentik" -- a rebuild should fail loudly rather
  than quietly reintroduce the other brand.
