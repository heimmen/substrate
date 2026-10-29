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

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIssuerScopedURL(t *testing.T) {
	for _, tt := range []struct {
		name   string
		url    string
		issuer string
		want   bool
	}{
		{
			name:   "same host root issuer discovery",
			url:    "https://kubernetes.default.svc/.well-known/openid-configuration",
			issuer: "https://kubernetes.default.svc",
			want:   true,
		},
		{
			name:   "same host path issuer jwks",
			url:    "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c/jwks",
			issuer: "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c",
			want:   true,
		},
		{
			name:   "same host sibling path",
			url:    "https://container.googleapis.com/v1/projects/p/locations/l/clusters/other/jwks",
			issuer: "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c",
			want:   false,
		},
		{
			name:   "prefix lookalike",
			url:    "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c-attacker/jwks",
			issuer: "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c",
			want:   false,
		},
		{
			name:   "different host",
			url:    "https://attacker.example/.well-known/openid-configuration",
			issuer: "https://kubernetes.default.svc",
			want:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuerScopedURL(tt.url, tt.issuer); got != tt.want {
				t.Fatalf("issuerScopedURL(%q, %q) = %v, want %v", tt.url, tt.issuer, got, tt.want)
			}
		})
	}
}

func TestK8sServiceAccountIssuerDiscoveryTransport(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var gotAuth string
	transport := &k8sServiceAccountIssuerDiscoveryTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(nil),
				Header:     make(http.Header),
			}, nil
		}),
		tokenFile: tokenFile,
		issuer:    "https://kubernetes.default.svc",
	}

	req, err := http.NewRequest(http.MethodGet, "https://kubernetes.default.svc/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer test-token", gotAuth)
	}
}

func TestK8sServiceAccountIssuerDiscoveryTransportSendsTokenToKubernetesJWKSURL(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var gotAuth string
	transport := &k8sServiceAccountIssuerDiscoveryTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(nil),
				Header:     make(http.Header),
			}, nil
		}),
		tokenFile: tokenFile,
		issuer:    "https://kubernetes.default.svc",
	}

	req, err := http.NewRequest(http.MethodGet, "https://172.18.0.2:6443/openid/v1/jwks", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer test-token", gotAuth)
	}
}

func TestK8sServiceAccountIssuerDiscoveryTransportDoesNotSendTokenToArbitraryURL(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var gotAuth string
	transport := &k8sServiceAccountIssuerDiscoveryTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(nil),
				Header:     make(http.Header),
			}, nil
		}),
		tokenFile: tokenFile,
		issuer:    "https://kubernetes.default.svc",
	}

	req, err := http.NewRequest(http.MethodGet, "https://attacker.example/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestBuildK8sServiceAccountIssuerDiscoveryClientUsesDefaultTransportForExternalIssuer(t *testing.T) {
	client := buildK8sServiceAccountIssuerDiscoveryClient(t.Context(), "/does/not/matter", "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c")
	if client == nil {
		t.Fatalf("buildK8sServiceAccountIssuerDiscoveryClient() = nil, want client")
	}
	if client.Timeout == 0 {
		t.Fatalf("client timeout = 0, want nonzero timeout")
	}
	if _, ok := client.Transport.(*k8sServiceAccountIssuerDiscoveryTransport); ok {
		t.Fatalf("external issuer should not use ServiceAccount token transport")
	}
}

func TestIsInClusterKubernetesIssuer(t *testing.T) {
	for _, tt := range []struct {
		issuer string
		want   bool
	}{
		{issuer: "https://kubernetes.default.svc", want: true},
		{issuer: "https://kubernetes.default.svc.cluster.local", want: true},
		{issuer: "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c", want: false},
		{issuer: "https://attacker.example", want: false},
	} {
		t.Run(tt.issuer, func(t *testing.T) {
			if got := isInClusterKubernetesIssuer(tt.issuer); got != tt.want {
				t.Fatalf("isInClusterKubernetesIssuer(%q) = %v, want %v", tt.issuer, got, tt.want)
			}
		})
	}
}

// TestBuildRedisTLSConfigReloadsClientCertificate guards against pinning the
// pod-identity credential bundle at startup. Kubelet rotates the bundle in
// place; the Redis TLS config must re-read it on each handshake, otherwise a
// long-lived ate-api process keeps presenting an expired client certificate
// and Valkey rejects it with "remote error: tls: expired certificate".
func TestBuildRedisTLSConfigReloadsClientCertificate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential-bundle.pem")

	writeBundle := func(serial int64) *x509.Certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(time.Hour),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("create certificate: %v", err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal private key: %v", err)
		}
		var buf bytes.Buffer
		if err := pem.Encode(&buf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
			t.Fatalf("encode private key: %v", err)
		}
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			t.Fatalf("encode certificate: %v", err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse certificate: %v", err)
		}
		return cert
	}

	oldCA, oldServerName, oldClientCert := *redisCACerts, *redisTLSServerName, *redisClientCert
	t.Cleanup(func() {
		*redisCACerts = oldCA
		*redisTLSServerName = oldServerName
		*redisClientCert = oldClientCert
	})
	*redisCACerts = ""
	*redisTLSServerName = ""
	*redisClientCert = path

	first := writeBundle(111)
	cfg, err := buildRedisTLSConfig(t.Context())
	if err != nil {
		t.Fatalf("buildRedisTLSConfig: %v", err)
	}
	if cfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate is nil; client certificate was pinned at startup instead of reloaded")
	}
	if len(cfg.Certificates) != 0 {
		t.Fatalf("tls.Config.Certificates = %d entries, want 0 (must reload from disk)", len(cfg.Certificates))
	}

	got, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate (first): %v", err)
	}
	if got.Leaf == nil {
		leaf, err := x509.ParseCertificate(got.Certificate[0])
		if err != nil {
			t.Fatalf("parse returned certificate: %v", err)
		}
		got.Leaf = leaf
	}
	if got.Leaf.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("first serial = %v, want %v", got.Leaf.SerialNumber, first.SerialNumber)
	}

	// Simulate kubelet rotating the projected volume in place.
	second := writeBundle(222)
	got, err = cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate (after rotation): %v", err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatalf("parse rotated certificate: %v", err)
	}
	if leaf.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatalf("rotated serial = %v, want %v (bundle was not re-read)", leaf.SerialNumber, second.SerialNumber)
	}
}

func TestBuildRedisTLSConfigRejectsMissingClientBundle(t *testing.T) {
	oldClientCert := *redisClientCert
	t.Cleanup(func() { *redisClientCert = oldClientCert })
	*redisClientCert = filepath.Join(t.TempDir(), "missing.pem")

	if _, err := buildRedisTLSConfig(t.Context()); err == nil {
		t.Fatal("buildRedisTLSConfig() = nil error for a missing client bundle, want error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
