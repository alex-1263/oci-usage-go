package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testProfile(t *testing.T) *Profile {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Profile{User: "ocid1.user.x", Fingerprint: "aa:bb", Tenancy: "ocid1.tenancy.x", Region: "us-ashburn-1", PrivateKey: key}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func pemEncode(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

// TestSign verifies the signing-string layout per OCI spec and that the
// signature validates over the exact expected signing string.
func TestSign(t *testing.T) {
	p := testProfile(t)
	date := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	body := []byte(`{"a":1}`)
	host := "usageapi.us-ashburn-1.oci.oraclecloud.com"

	h, auth, err := sign(p, "POST", host, "/20200107/usage", date, body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	wantB64 := base64.StdEncoding.EncodeToString(sum[:])
	if h.Get("x-content-sha256") != wantB64 {
		t.Fatal("body hash mismatch")
	}
	if h.Get("content-length") != "7" || h.Get("content-type") != "application/json" {
		t.Fatalf("body headers wrong: %v", h)
	}
	wantHeaders := "(request-target) date host x-content-sha256 content-type content-length"
	if !containsAll(auth, []string{
		`version="1"`,
		`keyId="ocid1.tenancy.x/ocid1.user.x/aa:bb"`,
		`algorithm="rsa-sha256"`,
		`headers="` + wantHeaders + `"`,
	}) {
		t.Fatalf("authorization malformed:\n%s", auth)
	}
	sigPart := auth[strings.Index(auth, `signature="`)+len(`signature="`):]
	sigPart = sigPart[:len(sigPart)-1]
	sig, err := base64.StdEncoding.DecodeString(sigPart)
	if err != nil {
		t.Fatal(err)
	}
	ss := "(request-target): post /20200107/usage\ndate: " + date.UTC().Format(http.TimeFormat) +
		"\nhost: " + host +
		"\nx-content-sha256: " + wantB64 +
		"\ncontent-type: application/json\ncontent-length: 7"
	digest := sha256.Sum256([]byte(ss))
	if err := rsa.VerifyPKCS1v15(&p.PrivateKey.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

// TestQueryUsageAggregation covers quantity summing, USD preference, free-tier
// marking, outbound transfer detection.
func TestQueryUsageAggregation(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req["queryType"] == "USAGE" {
			w.Write([]byte(`{"items":[
				{"service":"Compute","skuName":"Standard - A1","unit":"OCPU Per Hour","computedQuantity":100},
				{"service":"Virtual Cloud Network","skuName":"Outbound Data Transfer Zone 2","unit":"GB Months","computedQuantity":12.5}
			]}`))
			return
		}
		w.Write([]byte(`{"items":[
			{"service":"Compute","skuName":"Standard - A1","computedAmount":0,"currency":"SGD"},
			{"service":"Compute","skuName":"Standard - A1","computedAmount":0,"currency":"USD"},
			{"service":"Load Balancer","skuName":"Flexible LB Bandwidth","computedAmount":4.2,"currency":"USD"}
		]}`))
	}))
	defer srv.Close()
	t.Setenv("OCI_USAGE_HOST", "http://"+srv.Listener.Addr().String())

	res, err := QueryUsage(testProfile(t),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected USAGE+COST calls, got %d", calls)
	}
	if res.CostFailed {
		t.Fatal("unexpected cost failure")
	}
	bySKU := map[string]LineItem{}
	for _, li := range res.LineItems {
		bySKU[li.SKU] = li
	}
	a1 := bySKU["Standard - A1"]
	if a1.Quantity != 100 {
		t.Fatalf("A1 quantity = %v, want 100", a1.Quantity)
	}
	if a1.Cost == nil || *a1.Cost != 0 || a1.Currency != "USD" {
		t.Fatalf("A1 cost = %v %s, want 0 USD (SGD dropped)", a1.Cost, a1.Currency)
	}
	if !a1.IsFreeTier {
		t.Fatal("A1 should be free tier")
	}
	lb := bySKU["Flexible LB Bandwidth"]
	if lb.Cost == nil || *lb.Cost != 4.2 || lb.IsFreeTier {
		t.Fatalf("LB cost = %v, want 4.2 USD non-free", lb.Cost)
	}
	if res.OutboundGB != 12.5 {
		t.Fatalf("outbound = %v, want 12.5", res.OutboundGB)
	}
	if res.MaxCost != 4.2 {
		t.Fatalf("max cost = %v, want 4.2", res.MaxCost)
	}
}

// TestQueryUsageCostFailure ensures Cost API errors are surfaced, never
// silently reported as $0.
func TestQueryUsageCostFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req["queryType"] == "USAGE" {
			w.Write([]byte(`{"items":[{"service":"Compute","skuName":"Standard - A1","computedQuantity":1}]}`))
			return
		}
		w.WriteHeader(500)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("OCI_USAGE_HOST", "http://"+srv.Listener.Addr().String())

	res, err := QueryUsage(testProfile(t), time.Now().UTC().Add(-time.Hour), time.Now().UTC(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CostFailed {
		t.Fatal("CostFailed should be true when COST API returns 500")
	}
}

// TestParseConfig checks ~/.oci/config parsing with relative key_file.
func TestParseConfig(t *testing.T) {
	dir := t.TempDir()
	ociDir := filepath.Join(dir, ".oci")
	if err := os.MkdirAll(ociDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := testProfile(t).PrivateKey
	if err := os.WriteFile(filepath.Join(ociDir, "key.pem"), pemEncode(x509.MarshalPKCS1PrivateKey(key)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := "[DEFAULT]\nuser=u1\nfingerprint=fp1\ntenancy=t1\nregion=ap-seoul-1\nkey_file=.oci/key.pem\n"
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := ParseConfig(cfgPath, "DEFAULT")
	if err != nil {
		t.Fatal(err)
	}
	if p.User != "u1" || p.Region != "ap-seoul-1" || p.Tenancy != "t1" || p.Fingerprint != "fp1" {
		t.Fatalf("profile fields wrong: %+v", p)
	}
	if p.PrivateKey == nil || p.PrivateKey.N.BitLen() != 2048 {
		t.Fatal("private key not parsed")
	}
	if _, err := ParseConfig(cfgPath, "NOPE"); err == nil {
		t.Fatal("expected error for missing section")
	}
}
