// Package oci implements the minimal subset of the OCI REST API needed for
// usage reporting: request signing (Signature Version 1) and the Usage API.
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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Profile mirrors the fields of an ~/.oci/config section (official format).
type Profile struct {
	User        string
	Fingerprint string
	Tenancy     string
	Region      string
	PrivateKey  *rsa.PrivateKey
}

// LineItem is one aggregated usage/cost row returned by the Usage API.
type LineItem struct {
	Service    string   `json:"service"`
	SKU        string   `json:"sku"`
	Unit       string   `json:"unit"`
	Quantity   float64  `json:"quantity"`
	Cost       *float64 `json:"cost"` // nil = no cost data
	Currency   string   `json:"currency"`
	IsFreeTier bool     `json:"is_free_tier"`
}

// UsageResult is the aggregated result for one profile and time range.
type UsageResult struct {
	Region     string
	LineItems  []LineItem
	OutboundGB float64
	MaxCost    float64 // largest single-currency total; free-tier users expect 0
	CostFailed bool    // true if the Cost API failed (never silently report $0)
}

// usageHostOf may be overridden in tests via OCI_USAGE_HOST.
func usageHostOf(region string) string {
	if h := os.Getenv("OCI_USAGE_HOST"); h != "" {
		return h
	}

	return fmt.Sprintf("usageapi.%s.oci.oraclecloud.com", region)
}

// usageURL builds the request URL; OCI_USAGE_HOST may carry a scheme for tests.
func usageURL(region, path string) string {
	host := usageHostOf(region)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	return host + path
}

// usageHostHeader strips any scheme for the Host header and signing string.
func usageHostHeader(region string) string {
	host := usageHostOf(region)
	return strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
}

// sign builds the OCI Signature v1 Authorization header.
// Signing string per https://docs.oracle.com/en-us/iaas/Content/API/Concepts/signingrequests.htm:
// (request-target)\ndate\nhost[,\nx-content-sha256,\ncontent-type,\ncontent-length for POST]
func sign(p *Profile, method, host, path string, date time.Time, body []byte) (http.Header, string, error) {
	var names []string
	var ss strings.Builder
	fmt.Fprintf(&ss, "(request-target): %s %s\ndate: %s\nhost: %s",
		strings.ToLower(method), path, date.UTC().Format(http.TimeFormat), host)
	names = append(names, "(request-target)", "date", "host")

	h := http.Header{}
	h.Set("date", date.UTC().Format(http.TimeFormat))
	h.Set("host", host)

	if body != nil {
		sum := sha256.Sum256(body)
		b64 := base64.StdEncoding.EncodeToString(sum[:])
		h.Set("x-content-sha256", b64)
		h.Set("content-type", "application/json")
		h.Set("content-length", fmt.Sprint(len(body)))
		fmt.Fprintf(&ss, "\nx-content-sha256: %s\ncontent-type: application/json\ncontent-length: %s",
			b64, h.Get("content-length"))
		names = append(names, "x-content-sha256", "content-type", "content-length")
	}

	digest := sha256.Sum256([]byte(ss.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return nil, "", err
	}
	keyID := fmt.Sprintf("%s/%s/%s", p.Tenancy, p.User, p.Fingerprint)
	auth := fmt.Sprintf(`Signature version="1",keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		keyID, strings.Join(names, " "), base64.StdEncoding.EncodeToString(sig))
	return h, auth, nil
}

type rawItem struct {
	Service        string  `json:"service"`
	SKU            string  `json:"skuName"`
	Unit           string  `json:"unit"`
	Quantity       float64 `json:"computedQuantity"`
	ComputedAmount float64 `json:"computedAmount"`
	Currency       string  `json:"currency"`
}

// QueryUsage calls the Usage API for [timeStart, timeEnd) and returns
// aggregated line items. It queries both USAGE (quantities) and COST (money),
// mirroring what the OCI console "Cost analysis" shows.
func QueryUsage(p *Profile, timeStart, timeEnd time.Time, timeout time.Duration) (*UsageResult, error) {
	res := &UsageResult{Region: p.Region}
	client := &http.Client{Timeout: timeout}

	fetch := func(queryType string) ([]rawItem, error) {
		payload, _ := json.Marshal(map[string]any{
			"tenantId":         p.Tenancy,
			"timeUsageStarted": timeStart.UTC().Format("2006-01-02T15:04:05.000Z"),
			"timeUsageEnded":   timeEnd.UTC().Format("2006-01-02T15:04:05.000Z"),
			"granularity":      "TOTAL",
			"queryType":        queryType,
		})
		path := "/20200107/usage"
		hh := usageHostHeader(p.Region)
		h, auth, err := sign(p, "POST", hh, path, time.Now(), payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("POST", usageURL(p.Region, path), strings.NewReader(string(payload)))
		if err != nil {
			return nil, err
		}
		for k, v := range h {
			req.Header[k] = v
		}
		req.Header.Set("authorization", auth)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s api status %d", queryType, resp.StatusCode)
		}
		var items []rawItem
		return items, json.NewDecoder(resp.Body).Decode(&items)
	}

	usageItems, err := fetch("USAGE")
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	costItems, costErr := fetch("COST")
	res.CostFailed = costErr != nil // never silently report $0

	// Quantities from USAGE; costs from COST (USD preferred over other currencies).
	type acc struct {
		li       LineItem
		perCur   map[string]float64
		usdFound bool
	}
	order := []*acc{}
	byKey := map[string]*acc{}
	add := func(service, sku, unit string) *acc {
		key := service + "|" + sku
		if a, ok := byKey[key]; ok {
			return a
		}
		a := &acc{li: LineItem{Service: service, SKU: sku, Unit: unit}, perCur: map[string]float64{}}
		byKey[key] = a
		order = append(order, a)
		return a
	}
	for _, u := range usageItems {
		add(u.Service, u.SKU, u.Unit).li.Quantity += u.Quantity
	}
	if !res.CostFailed {
		for _, c := range costItems {
			a := add(c.Service, c.SKU, c.Unit)
			a.perCur[c.Currency] += c.ComputedAmount
		}
		maxCost := 0.0
		for _, a := range order {
			if len(a.perCur) == 0 {
				continue
			}
			if v, ok := a.perCur["USD"]; ok {
				a.li.Cost, a.li.Currency = &v, "USD"
			} else {
				// No USD entry: take the single currency if only one exists,
				// otherwise list the first alphabetically (never mix currencies).
				curs := make([]string, 0, len(a.perCur))
				for c := range a.perCur {
					curs = append(curs, c)
				}
				c := curs[0]
				for _, x := range curs {
					if x < c {
						c = x
					}
				}
				v := a.perCur[c]
				a.li.Cost, a.li.Currency = &v, c
			}
			if a.li.Cost != nil && *a.li.Cost > maxCost {
				maxCost = *a.li.Cost
			}
		}
		res.MaxCost = maxCost
	}
	for _, a := range order {
		a.li.IsFreeTier = a.li.Cost == nil || *a.li.Cost == 0
		res.LineItems = append(res.LineItems, a.li)
		if strings.Contains(strings.ToLower(a.li.SKU), "outbound data transfer") {
			res.OutboundGB += a.li.Quantity
		}
	}
	return res, nil
}

// parseRSA parses a PEM-encoded PKCS#1 or PKCS#8 RSA private key.
func ParseRSA(pemBytes []byte) *rsa.PrivateKey {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk
		}
	}
	return nil
}

// ParseConfig reads one profile section from an ~/.oci/config file
// (official INI layout) and resolves key_file relative to the config dir.
func ParseConfig(path, section string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cur := ""
	want := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = line[1 : len(line)-1]
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		want[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	keyPath := want["key_file"]
	if keyPath != "" && !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(filepath.Dir(path), keyPath)
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	return &Profile{
		User:        want["user"],
		Fingerprint: want["fingerprint"],
		Tenancy:     want["tenancy"],
		Region:      want["region"],
		PrivateKey:  ParseRSA(pemBytes),
	}, nil
}
