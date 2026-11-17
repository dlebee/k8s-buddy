package monitor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlebee/k8s-buddy/internal/config"
)

// Node list prices for cost estimates. Only public, login-free price lists are used (Azure Retail
// Prices API, Linode's public types API), looked up by VM size and region — no cluster data is
// sent — and cached on disk for a week. Prices are pay-as-you-go Linux list prices: reservations,
// savings plans, spot and negotiated discounts aren't known, so every figure is an estimate.

type NodeSKU struct {
	Provider string `json:"provider"` // azure, aws, gcp, linode, digitalocean, or ""
	SKU      string `json:"sku"`
	Region   string `json:"region"`
}

func nodeSKU(n *corev1.Node) NodeSKU {
	s := NodeSKU{
		SKU:    firstNonEmpty(n.Labels["node.kubernetes.io/instance-type"], n.Labels["beta.kubernetes.io/instance-type"]),
		Region: firstNonEmpty(n.Labels["topology.kubernetes.io/region"], n.Labels["failure-domain.beta.kubernetes.io/region"]),
	}
	switch id := n.Spec.ProviderID; {
	case strings.HasPrefix(id, "azure://"):
		s.Provider = "azure"
	case strings.HasPrefix(id, "aws://"):
		s.Provider = "aws"
	case strings.HasPrefix(id, "gce://"):
		s.Provider = "gcp"
	case strings.HasPrefix(id, "linode://"):
		s.Provider = "linode"
	case strings.HasPrefix(id, "digitalocean://"):
		s.Provider = "digitalocean"
	}
	return s
}

type priceEntry struct {
	Hourly float64   `json:"hourly"` // USD; <0 means "no price found"
	At     time.Time `json:"at"`
}

const priceTTL = 7 * 24 * time.Hour

var (
	priceMu      sync.Mutex
	prices       map[string]priceEntry
	priceLoading = map[string]bool{}
	safeSKU      = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)
)

func pricePath() string {
	d, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "node-prices.json")
}

// NodePrice returns the hourly USD list price for a node SKU, or ok=false while unknown. A miss
// starts a background lookup, so the next report has it.
func NodePrice(s NodeSKU) (hourly float64, source string, ok bool) {
	if !safeSKU.MatchString(s.SKU) || !safeSKU.MatchString(s.Region) {
		return 0, "", false
	}
	var fetch func(NodeSKU) (float64, error)
	switch s.Provider {
	case "azure":
		fetch, source = azurePrice, "Azure Retail Prices API"
	case "linode":
		fetch, source = linodePrice, "Linode public API"
	default:
		return 0, "", false
	}
	key := s.Provider + "|" + s.Region + "|" + s.SKU
	priceMu.Lock()
	if prices == nil {
		prices = map[string]priceEntry{}
		if b, err := os.ReadFile(pricePath()); err == nil {
			_ = json.Unmarshal(b, &prices)
		}
	}
	e, have := prices[key]
	stale := !have || time.Since(e.At) > priceTTL
	if stale && !priceLoading[key] {
		priceLoading[key] = true
		go func() {
			h, err := fetch(s)
			priceMu.Lock()
			defer priceMu.Unlock()
			delete(priceLoading, key)
			if err != nil {
				return // try again on a later report
			}
			prices[key] = priceEntry{Hourly: h, At: time.Now()}
			if b, err := json.Marshal(prices); err == nil && pricePath() != "" {
				_ = os.WriteFile(pricePath(), b, 0o600)
			}
		}()
	}
	priceMu.Unlock()
	if !have || e.Hourly < 0 {
		return 0, source, false
	}
	return e.Hourly, source, true
}

var priceClient = &http.Client{Timeout: 20 * time.Second}

func getJSON(u string, v any) error {
	resp, err := priceClient.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// azurePrice is the pay-as-you-go Linux price (no Spot / Low Priority / Windows / Cloud Services).
func azurePrice(s NodeSKU) (float64, error) {
	filter := fmt.Sprintf("serviceName eq 'Virtual Machines' and armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'", s.Region, s.SKU)
	var r struct {
		Items []struct {
			SKUName     string  `json:"skuName"`
			ProductName string  `json:"productName"`
			UnitPrice   float64 `json:"unitPrice"`
			Unit        string  `json:"unitOfMeasure"`
		} `json:"Items"`
	}
	if err := getJSON("https://prices.azure.com/api/retail/prices?currencyCode=USD&$filter="+url.QueryEscape(filter), &r); err != nil {
		return 0, err
	}
	best := -1.0
	for _, it := range r.Items {
		if it.Unit != "1 Hour" || strings.Contains(it.ProductName, "Windows") || strings.Contains(it.ProductName, "Cloud Services") ||
			strings.Contains(it.SKUName, "Spot") || strings.Contains(it.SKUName, "Low Priority") {
			continue
		}
		if best < 0 || it.UnitPrice < best {
			best = it.UnitPrice
		}
	}
	return best, nil
}

// linodePrice uses the region-specific price when Linode publishes one, else the base price.
func linodePrice(s NodeSKU) (float64, error) {
	var r struct {
		Data []struct {
			ID    string `json:"id"`
			Price struct {
				Hourly float64 `json:"hourly"`
			} `json:"price"`
			RegionPrices []struct {
				ID     string  `json:"id"`
				Hourly float64 `json:"hourly"`
			} `json:"region_prices"`
		} `json:"data"`
	}
	if err := getJSON("https://api.linode.com/v4/linode/types?page_size=500", &r); err != nil {
		return 0, err
	}
	for _, t := range r.Data {
		if t.ID != s.SKU {
			continue
		}
		for _, rp := range t.RegionPrices {
			if rp.ID == s.Region {
				return rp.Hourly, nil
			}
		}
		return t.Price.Hourly, nil
	}
	return -1, nil
}
