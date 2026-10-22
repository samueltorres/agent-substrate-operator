// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pki generates the CA and JWT pool Secrets Agent Substrate reads.
//
// The pool types, their JSON wire format, and the key generation are copied
// from upstream internal/localca and internal/localjwtauthority at the
// embedded release, keeping only what generating a pool needs. The operator
// is a separate module, so it cannot import them.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// CAPool is upstream's localca.ConcretePool.
type CAPool struct {
	CAs              []*CA
	ActiveForSigning string
}

// CA is a certificate authority signing from a single root certificate.
type CA struct {
	ID              string
	SigningKey      crypto.PrivateKey
	RootCertificate *x509.Certificate
}

// TLSCertificateChainPEM returns the CA certificate in the PEM encoding used
// by TLS servers.
func (ca *CA) TLSCertificateChainPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.RootCertificate.Raw})
}

// TLSPrivateKeyPEM returns the CA signing key in the PKCS#8 PEM encoding used
// by TLS servers.
func (ca *CA) TLSPrivateKeyPEM() ([]byte, error) {
	key, err := x509.MarshalPKCS8PrivateKey(ca.SigningKey)
	if err != nil {
		return nil, fmt.Errorf("ca key: serializing PKCS#8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), nil
}

type serializedCAPool struct {
	CAs              []*serializedCA
	ActiveForSigning string
}

type serializedCA struct {
	ID                 string
	SigningKeyPKCS8    []byte
	RootCertificateDER []byte
}

// MarshalCAPool serializes a pool to upstream's JSON wire format.
func MarshalCAPool(pool *CAPool) ([]byte, error) {
	wire := &serializedCAPool{ActiveForSigning: pool.ActiveForSigning}
	for _, ca := range pool.CAs {
		key, err := x509.MarshalPKCS8PrivateKey(ca.SigningKey)
		if err != nil {
			return nil, fmt.Errorf("while serializing signing key for CA %q to PKCS#8: %w", ca.ID, err)
		}
		wire.CAs = append(wire.CAs, &serializedCA{
			ID:                 ca.ID,
			SigningKeyPKCS8:    key,
			RootCertificateDER: ca.RootCertificate.Raw,
		})
	}
	return json.Marshal(wire)
}

// UnmarshalCAPool loads a pool from upstream's JSON wire format.
func UnmarshalCAPool(wireBytes []byte) (*CAPool, error) {
	wire := &serializedCAPool{}
	if err := json.Unmarshal(wireBytes, wire); err != nil {
		return nil, fmt.Errorf("while unmarshaling JSON: %w", err)
	}
	pool := &CAPool{ActiveForSigning: wire.ActiveForSigning}
	for _, wireCA := range wire.CAs {
		key, err := x509.ParsePKCS8PrivateKey(wireCA.SigningKeyPKCS8)
		if err != nil {
			return nil, fmt.Errorf("while parsing signing key: %w", err)
		}
		cert, err := x509.ParseCertificate(wireCA.RootCertificateDER)
		if err != nil {
			return nil, fmt.Errorf("while parsing root certificate: %w", err)
		}
		pool.CAs = append(pool.CAs, &CA{ID: wireCA.ID, SigningKey: key, RootCertificate: cert})
	}
	return pool, nil
}

// KeyType is the kind of key a CA signs with.
type KeyType int

const (
	KeyTypeED25519 KeyType = iota
	KeyTypeECDSAP256
)

// GenerateCA creates a self-signed CA with its own freshly generated key.
func GenerateCA(id string, keyType KeyType, validity time.Duration) (*CA, error) {
	var rootPrivKey crypto.PrivateKey
	var rootPubKey crypto.PublicKey
	switch keyType {
	case KeyTypeED25519:
		pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("while generating root key: %w", err)
		}
		rootPrivKey = privKey
		rootPubKey = pubKey
	case KeyTypeECDSAP256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("while generating root key: %w", err)
		}
		rootPrivKey = key
		rootPubKey = &key.PublicKey
	default:
		return nil, fmt.Errorf("unsupported key type")
	}

	notBefore := time.Now()
	rootTemplate := &x509.Certificate{
		// Random subject content keeps Go's x509 code from mistaking the
		// template for its own parent.
		Subject:               pkix.Name{CommonName: rand.Text()},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootPubKey, rootPrivKey)
	if err != nil {
		return nil, fmt.Errorf("while generating root certificate: %w", err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, fmt.Errorf("while parsing root certificate: %w", err)
	}
	return &CA{ID: id, SigningKey: rootPrivKey, RootCertificate: rootCert}, nil
}
