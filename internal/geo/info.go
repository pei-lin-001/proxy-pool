package geo

import "strings"

// Info describes the GeoIP metadata for a node's server IP.
// It is runtime-only and never persisted to config files.
type Info struct {
	IP          string `json:"ip,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	ASN         string `json:"asn,omitempty"`
	ISP         string `json:"isp,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (g *Info) Text() string {
	if g == nil {
		return ""
	}
	parts := make([]string, 0, 6)
	if g.CountryCode != "" {
		parts = append(parts, g.CountryCode)
	}
	if g.Country != "" {
		parts = append(parts, g.Country)
	}
	if g.Region != "" {
		parts = append(parts, g.Region)
	}
	if g.City != "" {
		parts = append(parts, g.City)
	}
	if g.ASN != "" {
		parts = append(parts, g.ASN)
	}
	if g.ISP != "" {
		parts = append(parts, g.ISP)
	}
	return strings.Join(parts, " ")
}
