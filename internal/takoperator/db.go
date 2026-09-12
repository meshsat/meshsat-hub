package takoperator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Each tenant gets one database and one role in the shared TAK cluster, as
// CloudNativePG objects rather than as SQL.
//
// CNPG 1.30 serves `databases` and `databaseroles`, verified on the cluster
// rather than assumed, so the operator never patches the Cluster object and Argo
// CD's selfHeal has nothing to fight over. It also means the two things that
// matter are declarations instead of statements: the connection limit, and
// whether a teardown keeps the data.
//
// THE ROLE AND THE DATABASE SHARE A NAME on purpose. The cluster's pg_hba is
// `host sameuser all all scram-sha-256` followed by `host all all all reject`
// (validated in spike S3), so a role may reach only the database whose name
// matches its own. Equal names are what turns that line into an isolation
// boundary; if they ever diverge, tenant isolation stops working silently.

// DatabaseObject is the Database the operator applies for one instance.
//
// hibernated maps to allowConnections=false, which is how an idle free tenant is
// parked: nothing is deleted, the database simply refuses new sessions.
// purge is the only thing that sets a reclaim policy of delete.
func DatabaseObject(label, dbCluster string, hibernated, purge bool) CNPGDatabase {
	allow := !hibernated
	limit := RoleConnectionLimit
	reclaim := ReclaimRetain
	if purge {
		reclaim = ReclaimDelete
	}
	return CNPGDatabase{
		TypeMeta: metav1.TypeMeta{APIVersion: CNPGGV, Kind: "Database"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DatabaseName(label),
			Namespace: DefaultDBNamespace,
			Labels:    objectLabels(label),
		},
		Spec: CNPGDatabaseSpec{
			Cluster:          ClusterRef{Name: dbCluster},
			Name:             DatabaseName(label),
			Owner:            DatabaseName(label),
			Ensure:           "present",
			AllowConnections: &allow,
			ConnectionLimit:  &limit,
			ReclaimPolicy:    reclaim,
		},
	}
}

// RoleObject is the DatabaseRole the operator applies for one instance.
//
// Note what is NOT set: superuser, createdb, createrole, replication and
// bypassrls are all absent, so the role can log in and use its own database and
// nothing else. OpenTAKServer runs its own migrations on every start, which is
// why it needs ownership of that database — and exactly why it must own nothing
// beyond it.
func RoleObject(label, dbCluster string, purge bool) CNPGDatabaseRole {
	login := true
	limit := RoleConnectionLimit
	reclaim := ReclaimRetain
	if purge {
		reclaim = ReclaimDelete
	}
	return CNPGDatabaseRole{
		TypeMeta: metav1.TypeMeta{APIVersion: CNPGGV, Kind: "DatabaseRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DatabaseName(label),
			Namespace: DefaultDBNamespace,
			Labels:    objectLabels(label),
		},
		Spec: CNPGDatabaseRoleSpec{
			Cluster:         ClusterRef{Name: dbCluster},
			Name:            DatabaseName(label),
			Ensure:          "present",
			Login:           &login,
			ConnectionLimit: &limit,
			PasswordSecret:  &SecretRef{Name: DBSecretName(label)},
			// A comment in the database itself, so somebody reading \du in psql
			// can tell what a role is for without a lookup table.
			Comment:       "MeshSat hosted TAK instance " + label,
			ReclaimPolicy: reclaim,
		},
	}
}

// DBPasswordSecret is the basic-auth Secret CNPG reads the role's password from.
// CNPG requires username and password keys, the same shape the Hub's own cluster
// credentials use.
//
// The password is generated once, by the operator, and never leaves the cluster:
// the Hub does not connect to a tenant's database, only the tenant's
// OpenTAKServer does.
func DBPasswordSecret(label, password string) corev1.Secret {
	return corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DBSecretName(label),
			Namespace: DefaultDBNamespace,
			Labels:    objectLabels(label),
		},
		Type: corev1.SecretTypeBasicAuth,
		StringData: map[string]string{
			corev1.BasicAuthUsernameKey: DatabaseName(label),
			corev1.BasicAuthPasswordKey: password,
		},
	}
}

// NewPassword returns a password with 256 bits of entropy, encoded so it can
// travel through a connection string without quoting surprises: base64url minus
// padding, which yields only [A-Za-z0-9_-].
func NewPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("takoperator: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ApplyDatabase creates or updates a tenant's database, role and role password.
//
// The password is written only when the Secret does not already exist. Rotating
// it on every reconcile would change the password under a running
// OpenTAKServer, which keeps a connection pool and would fail its next
// migration rather than reconnect.
func (r *Reconciler) ApplyDatabase(ctx context.Context, label string, hibernated bool) error {
	var existing corev1.Secret
	err := r.Client.Get(ctx, "v1", DefaultDBNamespace, "secrets", DBSecretName(label), &existing)
	switch {
	case err == nil:
		// Keep the password that is already in use.
	case isNotFound(err):
		pw, perr := NewPassword()
		if perr != nil {
			return perr
		}
		sec := DBPasswordSecret(label, pw)
		if aerr := r.Client.Apply(ctx, "v1", DefaultDBNamespace, "secrets", sec.Name, sec); aerr != nil {
			return fmt.Errorf("takoperator: apply db password secret: %w", aerr)
		}
	default:
		return fmt.Errorf("takoperator: read db password secret: %w", err)
	}

	role := RoleObject(label, r.DBCluster, false)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, RoleResource, role.Name, role); err != nil {
		return fmt.Errorf("takoperator: apply database role: %w", err)
	}
	// The role first, then the database: the database declares it as its owner,
	// and CNPG will not create a database owned by a role that does not exist.
	db := DatabaseObject(label, r.DBCluster, hibernated, false)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, DatabaseResource, db.Name, db); err != nil {
		return fmt.Errorf("takoperator: apply database: %w", err)
	}
	return nil
}

// PurgeDatabase flips both objects to a reclaim policy of delete and then
// removes them, which is what actually drops the tenant's data.
//
// It is reached only from a teardown that carries the purge annotation. The
// two-step matters: deleting an object whose policy still says retain would
// leave the database on disk with nothing in Kubernetes pointing at it, which is
// the worst of both outcomes — the data survives an erasure request and nobody
// can see that it did.
func (r *Reconciler) PurgeDatabase(ctx context.Context, label string) error {
	db := DatabaseObject(label, r.DBCluster, false, true)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, DatabaseResource, db.Name, db); err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: mark database for deletion: %w", err)
	}
	role := RoleObject(label, r.DBCluster, true)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, RoleResource, role.Name, role); err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: mark role for deletion: %w", err)
	}
	// Database before role: the role owns the database, and a role cannot be
	// dropped while it owns objects.
	if err := r.Client.Delete(ctx, CNPGGV, DefaultDBNamespace, DatabaseResource, db.Name); err != nil {
		return fmt.Errorf("takoperator: delete database: %w", err)
	}
	if err := r.Client.Delete(ctx, CNPGGV, DefaultDBNamespace, RoleResource, role.Name); err != nil {
		return fmt.Errorf("takoperator: delete role: %w", err)
	}
	if err := r.Client.Delete(ctx, "v1", DefaultDBNamespace, "secrets", DBSecretName(label)); err != nil {
		return fmt.Errorf("takoperator: delete db password secret: %w", err)
	}
	return nil
}

// RetainDatabase is the ordinary teardown: stop serving, keep the data. It
// rewrites both objects with a retain policy and then deletes the Kubernetes
// objects, leaving the database and role in Postgres.
func (r *Reconciler) RetainDatabase(ctx context.Context, label string) error {
	db := DatabaseObject(label, r.DBCluster, false, false)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, DatabaseResource, db.Name, db); err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: confirm database retention: %w", err)
	}
	role := RoleObject(label, r.DBCluster, false)
	if err := r.Client.Apply(ctx, CNPGGV, DefaultDBNamespace, RoleResource, role.Name, role); err != nil && !isNotFound(err) {
		return fmt.Errorf("takoperator: confirm role retention: %w", err)
	}
	if err := r.Client.Delete(ctx, CNPGGV, DefaultDBNamespace, DatabaseResource, db.Name); err != nil {
		return err
	}
	return r.Client.Delete(ctx, CNPGGV, DefaultDBNamespace, RoleResource, role.Name)
}
