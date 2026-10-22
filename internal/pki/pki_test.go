package pki

import (
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"testing"
)

func TestCAPoolSecretData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyType KeyType
	}{
		{"ed25519", KeyTypeED25519},
		{"ecdsa", KeyTypeECDSAP256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := NewCAPoolSecretData(tc.keyType)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := UnmarshalCAPool(data["pool"])
			if err != nil {
				t.Fatal(err)
			}
			if len(pool.CAs) != 1 || pool.ActiveForSigning != "1" || pool.CAs[0].ID != "1" {
				t.Fatalf("pool = %+v, want one CA with ID 1 active", pool)
			}
			if !pool.CAs[0].RootCertificate.IsCA {
				t.Error("root certificate is not a CA")
			}
			// tls.crt and tls.key must be a usable pair for the TLS consumers.
			if _, err := tls.X509KeyPair(data["tls.crt"], data["tls.key"]); err != nil {
				t.Errorf("tls.crt and tls.key are not a key pair: %v", err)
			}
			roots, err := CAPoolRootsPEM(data["pool"])
			if err != nil {
				t.Fatal(err)
			}
			if string(roots) != string(data["tls.crt"]) {
				t.Error("CAPoolRootsPEM differs from tls.crt")
			}
		})
	}
}

func TestCAPoolWireFormat(t *testing.T) {
	data, err := NewCAPoolSecretData(KeyTypeED25519)
	if err != nil {
		t.Fatal(err)
	}
	// Upstream reads these exact field names.
	var wire struct {
		CAs []struct {
			ID                 string
			SigningKeyPKCS8    []byte
			RootCertificateDER []byte
		}
		ActiveForSigning string
	}
	if err := json.Unmarshal(data["pool"], &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.CAs) != 1 || len(wire.CAs[0].SigningKeyPKCS8) == 0 || len(wire.CAs[0].RootCertificateDER) == 0 {
		t.Fatalf("wire = %+v", wire)
	}
	pool, err := UnmarshalCAPool(data["pool"])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.CAs[0].SigningKey.(ed25519.PrivateKey); !ok {
		t.Errorf("signing key is %T, want ed25519", pool.CAs[0].SigningKey)
	}
}

func TestJWTPoolSecretData(t *testing.T) {
	for _, alg := range []string{"ES256", "RS256"} {
		t.Run(alg, func(t *testing.T) {
			data, err := NewJWTPoolSecretData(alg)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Authorities []struct {
					ID              string
					Algorithm       string
					SigningKeyPKCS8 []byte
				}
				ActiveForSigning string
			}
			if err := json.Unmarshal(data["pool"], &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire.Authorities) != 1 {
				t.Fatalf("got %d authorities, want 1", len(wire.Authorities))
			}
			a := wire.Authorities[0]
			if a.Algorithm != alg || a.ID == "" || a.ID != wire.ActiveForSigning || len(a.SigningKeyPKCS8) == 0 {
				t.Errorf("authority = %+v, active %q", a, wire.ActiveForSigning)
			}
		})
	}
	if _, err := NewJWTPoolSecretData("HS256"); err == nil {
		t.Error("HS256 accepted")
	}
}
