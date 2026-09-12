package takoperator

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// rfc1123 is the name Kubernetes accepts for an object: lowercase alphanumeric,
// '-' and '.', starting and ending alphanumeric. Notably NO underscore.
var rfc1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// This test exists because the phase-2 gate's very first TakInstance failed on
// it. The Database and DatabaseRole objects were being created with
// metadata.name = "tak_gatetest01", and the API server answered 422:
//
//	metadata.name: Invalid value: "tak_gatetest01": a lowercase RFC 1123
//	subdomain must consist of lower case alphanumeric characters, '-' or '.'
//
// The underscored name is right for Postgres and wrong for Kubernetes, and
// nothing asserted the difference — the existing test compared the two POSTGRES
// names to each other, which stayed true while the object names were invalid.
func TestEveryKubernetesObjectNameIsDNSSafe(t *testing.T) {
	const label = "abcdefghij"
	for name, got := range map[string]string{
		"InstanceName":     InstanceName(label),
		"DBObjectName":     DBObjectName(label),
		"CASecretName":     CASecretName(label),
		"TLSSecretName":    TLSSecretName(label),
		"ConfigSecretName": ConfigSecretName(label),
		"DBSecretName":     DBSecretName(label),
	} {
		t.Run(name, func(t *testing.T) {
			if !rfc1123.MatchString(got) {
				t.Errorf("%s = %q, which Kubernetes will refuse with a 422", name, got)
			}
			if strings.Contains(got, "_") {
				t.Errorf("%s = %q contains an underscore; that is a Postgres identifier, not an object name", name, got)
			}
			if len(got) > 253 {
				t.Errorf("%s = %q is longer than 253 characters", name, got)
			}
		})
	}
}

// And the rendered objects, not just the helpers: the bug was in the call site,
// so assert what actually goes to the API server.
func TestTheRenderedDatabaseObjectsCarryBothKindsOfNameCorrectly(t *testing.T) {
	const label = "abcdefghij"
	db := DatabaseObject(label, "meshsat-tak-main", false, false)
	role := RoleObject(label, "meshsat-tak-main", false)
	secret := DBPasswordSecret(label, DefaultDBNamespace, "irrelevant")

	for name, objName := range map[string]string{
		"Database.metadata.name":     db.Name,
		"DatabaseRole.metadata.name": role.Name,
		"Secret.metadata.name":       secret.Name,
	} {
		if !rfc1123.MatchString(objName) {
			t.Errorf("%s = %q is not a valid Kubernetes name", name, objName)
		}
	}

	// The Postgres side keeps the underscore, and the two must match each other:
	// the cluster's pg_hba is `host sameuser all all scram-sha-256` followed by
	// `host all all all reject`, so a role reaches only the database whose name
	// equals its own. If these diverge, tenant isolation stops working silently.
	if db.Spec.Name != role.Spec.Name {
		t.Errorf("database is %q but role is %q; `host sameuser` needs them equal", db.Spec.Name, role.Spec.Name)
	}
	if db.Spec.Owner != role.Spec.Name {
		t.Errorf("database owner %q is not the role %q", db.Spec.Owner, role.Spec.Name)
	}
	if want := DatabaseName(label); db.Spec.Name != want {
		t.Errorf("database spec.name = %q, want the Postgres identifier %q", db.Spec.Name, want)
	}
	if !strings.Contains(db.Spec.Name, "_") {
		t.Errorf("database spec.name = %q lost its underscore; it is a Postgres identifier", db.Spec.Name)
	}
	// The role password Secret is referenced by name from the role, so a
	// mismatch there means CNPG cannot find the password.
	if role.Spec.PasswordSecret == nil || role.Spec.PasswordSecret.Name != secret.Name {
		t.Errorf("role points at password secret %+v, but the secret is %q", role.Spec.PasswordSecret, secret.Name)
	}
}

// Every Secret the rendered pod references must exist in the POD's namespace.
//
// This is the third failure of the same family, and the most expensive: the pod
// read its database credentials from `tak-<label>-db` in meshsat-tak while the
// operator created that Secret only in meshsat-tak-db, where CNPG needs it. A
// secretKeyRef is namespace-local, so the init container sat in
// CreateContainerConfigError with `secret "tak-gatetest01-db" not found` until it
// was looked at directly.
//
// The earlier tests could not catch it: they asserted names were well formed and
// that the Postgres identifiers matched, never that a referenced Secret is one
// somebody creates in the namespace that reads it.
//
// Mutation-checked: writing the password to only the database namespace makes
// this fail with "the pod reads Secret ... but nothing creates it in the pod's
// namespace".
func TestEverySecretThePodReadsIsCreatedInThePodsNamespace(t *testing.T) {
	const label = "abcdefghij"
	dep := InstanceDeployment(label, "ots@sha256:x", "rabbit:1", "nginx@sha256:y", 1)
	spec := dep.Spec.Template.Spec

	referenced := map[string]bool{}
	for _, v := range spec.Volumes {
		if v.Secret != nil {
			referenced[v.Secret.SecretName] = true
		}
	}
	collect := func(envs []corev1.EnvVar) {
		for _, e := range envs {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				referenced[e.ValueFrom.SecretKeyRef.Name] = true
			}
		}
	}
	for _, c := range spec.InitContainers {
		collect(c.Env)
	}
	for _, c := range spec.Containers {
		collect(c.Env)
	}

	// What the operator creates in the instance namespace.
	created := map[string]bool{
		CASecretName(label):     true,
		TLSSecretName(label):    true,
		ConfigSecretName(label): true,
		DBSecretName(label):     true, // written in BOTH namespaces, on purpose
	}

	if len(referenced) == 0 {
		t.Fatal("the rendered pod references no Secrets at all, which cannot be right")
	}
	for name := range referenced {
		if !created[name] {
			t.Errorf("the pod reads Secret %q, but nothing creates it in the pod's namespace; "+
				"the kubelet will report CreateContainerConfigError", name)
		}
	}
}

// The two copies of the database password must be byte-identical, or CNPG sets
// one password on the role while OpenTAKServer connects with another.
func TestBothCopiesOfTheDatabasePasswordAgree(t *testing.T) {
	const label = "abcdefghij"
	const pw = "a-password-that-must-not-change"
	inDB := DBPasswordSecret(label, DefaultDBNamespace, pw)
	inApp := DBPasswordSecret(label, DefaultNamespace, pw)

	if inDB.Name != inApp.Name {
		t.Errorf("names differ: %q and %q", inDB.Name, inApp.Name)
	}
	if inDB.Namespace == inApp.Namespace {
		t.Fatalf("both copies claim namespace %q; they must be in different ones", inDB.Namespace)
	}
	if inDB.StringData[corev1.BasicAuthPasswordKey] != inApp.StringData[corev1.BasicAuthPasswordKey] {
		t.Error("the two copies carry different passwords")
	}
	if inDB.StringData[corev1.BasicAuthUsernameKey] != DatabaseName(label) {
		t.Errorf("username is %q, want the Postgres role name %q",
			inDB.StringData[corev1.BasicAuthUsernameKey], DatabaseName(label))
	}
	if inDB.Type != corev1.SecretTypeBasicAuth {
		t.Errorf("type is %q; CNPG requires basic-auth", inDB.Type)
	}
}

// Teardown must keep data unless a purge is explicitly asked for: stopping a
// tenant and destroying its history are different acts.
func TestReclaimPoliciesDefaultToRetain(t *testing.T) {
	const label = "abcdefghij"
	db := DatabaseObject(label, "meshsat-tak-main", false, false)
	role := RoleObject(label, "meshsat-tak-main", false)
	if db.Spec.ReclaimPolicy != ReclaimRetain || role.Spec.ReclaimPolicy != ReclaimRetain {
		t.Errorf("policies are %q/%q, want both %q", db.Spec.ReclaimPolicy, role.Spec.ReclaimPolicy, ReclaimRetain)
	}
	purgedDB := DatabaseObject(label, "meshsat-tak-main", false, true)
	purgedRole := RoleObject(label, "meshsat-tak-main", true)
	if purgedDB.Spec.ReclaimPolicy != ReclaimDelete || purgedRole.Spec.ReclaimPolicy != ReclaimDelete {
		t.Errorf("a purge gave %q/%q, want both %q",
			purgedDB.Spec.ReclaimPolicy, purgedRole.Spec.ReclaimPolicy, ReclaimDelete)
	}
}

// A hibernated tenant is parked, not deleted: the database refuses new
// connections and nothing is dropped.
func TestHibernationOnlyClosesTheDoor(t *testing.T) {
	const label = "abcdefghij"
	running := DatabaseObject(label, "meshsat-tak-main", false, false)
	parked := DatabaseObject(label, "meshsat-tak-main", true, false)
	if running.Spec.AllowConnections == nil || !*running.Spec.AllowConnections {
		t.Error("a running instance should allow connections")
	}
	if parked.Spec.AllowConnections == nil || *parked.Spec.AllowConnections {
		t.Error("a hibernated instance should refuse new connections")
	}
	if parked.Spec.ReclaimPolicy != ReclaimRetain {
		t.Errorf("hibernation set reclaim %q; it must never imply deletion", parked.Spec.ReclaimPolicy)
	}
}

// The tenant role is allowed to log in and nothing else. OpenTAKServer runs its
// own migrations, so it owns its database — and must own nothing beyond it.
func TestTheTenantRoleHasNoPrivilegesBeyondItsOwnDatabase(t *testing.T) {
	role := RoleObject("abcdefghij", "meshsat-tak-main", false)
	if role.Spec.Login == nil || !*role.Spec.Login {
		t.Error("the role must be able to log in")
	}
	if role.Spec.ConnectionLimit == nil || *role.Spec.ConnectionLimit != RoleConnectionLimit {
		t.Errorf("connection limit = %v, want %d", role.Spec.ConnectionLimit, RoleConnectionLimit)
	}
	// Rendered JSON must not carry any of the privilege flags at all: absent is
	// what keeps them false, and a future edit that sets one should fail here.
	rendered := strings.ToLower(mustJSON(t, role.Spec))
	for _, forbidden := range []string{"superuser", "createdb", "createrole", "bypassrls", "replication"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the role spec mentions %q; a tenant role must have none of these", forbidden)
		}
	}
}

// mustJSON renders a spec the way the API client will send it, so assertions are
// about the bytes that reach the API server rather than about the Go struct.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
