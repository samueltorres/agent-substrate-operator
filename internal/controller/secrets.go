package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samueltorres/agent-substrate-operator/internal/pki"
	"github.com/samueltorres/agent-substrate-operator/internal/render"
)

// Pool Secrets the control plane reads, as upstream's installer names them.
const (
	secretActorIDJWTPool   = "actor-id-jwt-pool"
	secretActorIDCAPool    = "actor-id-ca-pool"
	secretActorIDCACerts   = "actor-id-ca-certs"
	secretEgressMITMCAPool = "egress-mitm-ca-pool"
	secretServiceDNSCA     = "service-dns-ca-pool"
	secretPodIdentityCA    = "pod-identity-ca-pool"
	secretPostgresCA       = "postgres-ca-pool"
)

// ensureControlPlaneSecrets creates the pools ate-api-server and the egress
// gateway sign with, and the root-only bundle derived from the actor identity
// CA. Existing pools are kept: regenerating one would rotate its root out from
// under everything already issued from it.
func (r *Reconciler) ensureControlPlaneSecrets(ctx context.Context, jwtAlgorithm string) error {
	if err := r.ensurePool(ctx, render.Namespace, secretActorIDJWTPool, corev1.SecretTypeOpaque, func() (map[string][]byte, error) {
		return pki.NewJWTPoolSecretData(jwtAlgorithm)
	}); err != nil {
		return err
	}
	if err := r.ensureCAPool(ctx, render.Namespace, secretActorIDCAPool, pki.KeyTypeED25519); err != nil {
		return err
	}
	// The egress gateway verifies actor client certificates, so it needs the
	// actor identity root, but not the signing key the pool also holds.
	if err := r.ensurePool(ctx, render.Namespace, secretActorIDCACerts, corev1.SecretTypeOpaque, func() (map[string][]byte, error) {
		pool := &corev1.Secret{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: render.Namespace, Name: secretActorIDCAPool}, pool); err != nil {
			return nil, err
		}
		roots, err := pki.CAPoolRootsPEM(pool.Data["pool"])
		if err != nil {
			return nil, fmt.Errorf("while reading %s: %w", secretActorIDCAPool, err)
		}
		return map[string][]byte{"ca.crt": roots}, nil
	}); err != nil {
		return err
	}
	// ECDSA, unlike the other pools: Envoy and agentgateway serve it as a TLS
	// key.
	return r.ensureCAPool(ctx, render.Namespace, secretEgressMITMCAPool, pki.KeyTypeECDSAP256)
}

// ensurePodCertificateCAs creates the signer pools the podcertificate
// controller issues from. Its pod mounts every one of them.
func (r *Reconciler) ensurePodCertificateCAs(ctx context.Context) error {
	for _, name := range []string{secretServiceDNSCA, secretPodIdentityCA, secretPostgresCA} {
		if err := r.ensureCAPool(ctx, render.PodCertNamespace, name, pki.KeyTypeED25519); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureCAPool(ctx context.Context, namespace, name string, keyType pki.KeyType) error {
	return r.ensurePool(ctx, namespace, name, corev1.SecretTypeTLS, func() (map[string][]byte, error) {
		return pki.NewCAPoolSecretData(keyType)
	})
}

// ensurePool creates a pool Secret when it is missing. It creates rather than
// applies, so a pool another writer just generated is never overwritten. The
// Secret carries no install label: pruning must never delete key material.
func (r *Reconciler) ensurePool(ctx context.Context, namespace, name string, secretType corev1.SecretType, generate func() (map[string][]byte, error)) error {
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	data, err := generate()
	if err != nil {
		return fmt.Errorf("while generating %s/%s: %w", namespace, name, err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{render.LabelManagedBy: render.ManagedBy},
		},
		Type: secretType,
		Data: data,
	}
	if err := r.Client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("while creating %s/%s: %w", namespace, name, err)
	}
	return nil
}
