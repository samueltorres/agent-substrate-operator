package pki

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// caValidity is how long a generated root is good for, the year upstream's
// installer asks for.
const caValidity = 365 * 24 * time.Hour

// poolKeyID names the first CA in a new pool, as upstream's installer does.
const poolKeyID = "1"

// NewCAPoolSecretData generates a pool holding one CA, active for signing,
// and returns the Secret contents upstream's installer writes: the pool under
// "pool", plus the root and its key under the standard TLS keys for consumers
// that speak TLS rather than the pool format.
func NewCAPoolSecretData(keyType KeyType) (map[string][]byte, error) {
	ca, err := GenerateCA(poolKeyID, keyType, caValidity)
	if err != nil {
		return nil, fmt.Errorf("while generating CA %q: %w", poolKeyID, err)
	}
	pool, err := MarshalCAPool(&CAPool{CAs: []*CA{ca}, ActiveForSigning: poolKeyID})
	if err != nil {
		return nil, fmt.Errorf("while marshaling the pool for CA %q: %w", poolKeyID, err)
	}
	key, err := ca.TLSPrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"pool":                  pool,
		corev1.TLSCertKey:       ca.TLSCertificateChainPEM(),
		corev1.TLSPrivateKeyKey: key,
	}, nil
}

// NewJWTPoolSecretData generates the actor identity JWT signing pool.
func NewJWTPoolSecretData(algorithm string) (map[string][]byte, error) {
	pool, err := GenerateJWTPool(algorithm)
	if err != nil {
		return nil, fmt.Errorf("while generating the JWT pool: %w", err)
	}
	return map[string][]byte{"pool": pool}, nil
}

// CAPoolRootsPEM returns the root certificates of a serialized CA pool, for
// verifiers that must not see its signing keys.
func CAPoolRootsPEM(poolBytes []byte) ([]byte, error) {
	pool, err := UnmarshalCAPool(poolBytes)
	if err != nil {
		return nil, err
	}
	if len(pool.CAs) == 0 {
		return nil, fmt.Errorf("the CA pool contains no CAs")
	}
	var buf bytes.Buffer
	for _, ca := range pool.CAs {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: ca.RootCertificate.Raw}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
