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

package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// JWTPool is upstream's localjwtauthority.ConcretePool.
type JWTPool struct {
	Authorities      []*JWTAuthority
	ActiveForSigning string
}

// JWTAuthority is one JWT signing key.
type JWTAuthority struct {
	ID         string
	Algorithm  string
	SigningKey crypto.Signer
}

type serializedJWTPool struct {
	Authorities      []*serializedJWTAuthority
	ActiveForSigning string
}

type serializedJWTAuthority struct {
	ID              string
	Algorithm       string
	SigningKeyPKCS8 []byte
}

// MarshalJWTPool serializes a pool to upstream's JSON wire format.
func MarshalJWTPool(pool *JWTPool) ([]byte, error) {
	wire := &serializedJWTPool{ActiveForSigning: pool.ActiveForSigning}
	for _, authority := range pool.Authorities {
		key, err := x509.MarshalPKCS8PrivateKey(authority.SigningKey)
		if err != nil {
			return nil, fmt.Errorf("while serializing signing key to PKCS#8: %w", err)
		}
		wire.Authorities = append(wire.Authorities, &serializedJWTAuthority{
			ID:              authority.ID,
			Algorithm:       authority.Algorithm,
			SigningKeyPKCS8: key,
		})
	}
	return json.Marshal(wire)
}

// rsaKeyBits is the size of the RSA keys GenerateJWTAuthority makes.
const rsaKeyBits = 4096

// GenerateJWTAuthority generates a JWT signing key for algorithm, RS256 or
// ES256, identified by the thumbprint of its public key.
func GenerateJWTAuthority(algorithm string) (*JWTAuthority, error) {
	var key crypto.Signer
	var err error
	switch algorithm {
	case "RS256":
		key, err = rsa.GenerateKey(rand.Reader, rsaKeyBits)
	case "ES256":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return nil, fmt.Errorf("unsupported algorithm %q, want RS256 or ES256", algorithm)
	}
	if err != nil {
		return nil, fmt.Errorf("while generating key: %w", err)
	}
	id, err := Thumbprint(key.Public())
	if err != nil {
		return nil, fmt.Errorf("while computing key thumbprint: %w", err)
	}
	return &JWTAuthority{ID: id, Algorithm: algorithm, SigningKey: key}, nil
}

// GenerateJWTPool generates a serialized pool holding one authority, active
// for signing.
func GenerateJWTPool(algorithm string) ([]byte, error) {
	authority, err := GenerateJWTAuthority(algorithm)
	if err != nil {
		return nil, err
	}
	return MarshalJWTPool(&JWTPool{
		Authorities:      []*JWTAuthority{authority},
		ActiveForSigning: authority.ID,
	})
}

// Thumbprint returns the SHA-256 of an RSA or P-256 EC public key's PKIX
// (SubjectPublicKeyInfo) DER encoding, base64url-encoded without padding.
func Thumbprint(pub crypto.PublicKey) (string, error) {
	switch k := pub.(type) {
	case *rsa.PublicKey:
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return "", fmt.Errorf("unsupported EC curve %s", k.Curve.Params().Name)
		}
	default:
		return "", fmt.Errorf("unsupported public key type %T", pub)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("while encoding the public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
