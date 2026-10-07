package savings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NodePricesJSON is the Thoras product's node price table, imported from its
// SQL migrations by scripts/import_node_prices.py. The report page inlines
// the same file, so the page and the summary price nodes alike.
//
//go:embed node_prices.json
var NodePricesJSON []byte

var (
	cloudOrder = []string{"aws", "gcp", "azure"}
	cloudNames = map[string]string{"aws": "AWS", "gcp": "Google Cloud", "azure": "Azure"}
	priceTable = mustPriceTable(NodePricesJSON)
)

// cloudPrices is one cloud's table. Types holds [vCPUs, MiB, on-demand USD
// per hour, spot USD per hour or null]. Custom holds the per-unit prices of
// machine families whose size is in the type name.
type cloudPrices struct {
	Region string
	Types  map[string][4]*float64
	Custom []customPrice
}

type customPrice struct {
	prefix                       string
	vcpu, gib, spotVCPU, spotGiB *float64
}

func (c *customPrice) UnmarshalJSON(b []byte) error {
	var row []json.RawMessage
	if err := json.Unmarshal(b, &row); err != nil {
		return err
	}
	if len(row) != 5 {
		return fmt.Errorf("custom price row has %d fields, want 5", len(row))
	}
	if err := json.Unmarshal(row[0], &c.prefix); err != nil {
		return err
	}
	for i, p := range []**float64{&c.vcpu, &c.gib, &c.spotVCPU, &c.spotGiB} {
		if err := json.Unmarshal(row[i+1], p); err != nil {
			return err
		}
	}
	return nil
}

func mustPriceTable(b []byte) map[string]cloudPrices {
	var t map[string]json.RawMessage
	if err := json.Unmarshal(b, &t); err != nil {
		panic(err)
	}
	out := map[string]cloudPrices{}
	for _, c := range cloudOrder {
		var cp cloudPrices
		if err := json.Unmarshal(t[c], &cp); err != nil {
			panic(fmt.Sprintf("node_prices.json %s: %v", c, err))
		}
		out[c] = cp
	}
	return out
}

// nodePrice is how one node is priced. reason is empty when it is priced, or
// says why it is left out of the rates: "type", "cloud", "size" or "spot".
type nodePrice struct {
	cloud        string
	cores, gib   float64
	hourly       float64
	spot, custom bool
	// inferred is set when the file does not name the node's cloud and the
	// instance type is found in exactly one cloud's table.
	inferred, azureSpot, otherRegion bool
	reason                           string
}

// customSize reads vCPUs and MiB from a custom machine type such as
// n2d-custom-16-32768 or n2-custom-4-8192-ext.
var customSize = regexp.MustCompile(`-custom-(\d+)-(\d+)(-ext)?$`)

// priceNode prices a node the way the product's average node query does:
// the cloud's table by instance type, else a custom family's per-unit
// prices, and the spot price for a spot node. The product detects no Azure
// spot nodes, so they are priced at on-demand.
func priceNode(n node) nodePrice {
	p := nodePrice{cloud: n.provider}
	if n.hostType == "" || n.hostType == "unknown" {
		p.reason = "type"
		return p
	}
	if _, ok := priceTable[p.cloud]; !ok {
		if n.provider != "" && n.provider != "unknown" {
			p.reason = "cloud"
			return p
		}
		p.cloud, p.inferred = "", true
		for _, c := range cloudOrder {
			if _, ok := priceTable[c].Types[n.hostType]; ok || customFor(priceTable[c], n.hostType) != nil {
				p.cloud = c
				break
			}
		}
		if p.cloud == "" {
			p.reason = "type"
			return p
		}
	}
	t := priceTable[p.cloud]
	p.spot = n.capacity == "spot"
	if p.spot && p.cloud == "azure" {
		p.spot, p.azureSpot = false, true
	}
	p.otherRegion = n.region != "" && n.region != "unknown" && n.region != t.Region
	if row, ok := t.Types[n.hostType]; ok {
		p.cores, p.gib = *row[0], *row[1]/1024
		price := row[2]
		if p.spot {
			price = row[3]
		}
		if price == nil {
			p.reason = "spot"
			return p
		}
		p.hourly = *price
		return p
	}
	c := customFor(t, n.hostType)
	if c == nil {
		p.reason = "type"
		return p
	}
	p.custom = true
	p.cores, p.gib = n.cores, n.gib
	if m := customSize.FindStringSubmatch(n.hostType); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		mib, _ := strconv.ParseFloat(m[2], 64)
		p.cores, p.gib = v, mib/1024
	}
	if !(p.cores > 0) || !(p.gib > 0) {
		p.reason = "size"
		return p
	}
	vcpu, gib := c.vcpu, c.gib
	if p.spot {
		vcpu, gib = c.spotVCPU, c.spotGiB
	}
	if vcpu == nil || gib == nil {
		p.reason = "spot"
		return p
	}
	p.hourly = p.cores**vcpu + p.gib**gib
	return p
}

func customFor(t cloudPrices, typ string) *customPrice {
	for i := range t.Custom {
		if strings.HasPrefix(typ, t.Custom[i].prefix) {
			return &t.Custom[i]
		}
	}
	return nil
}
