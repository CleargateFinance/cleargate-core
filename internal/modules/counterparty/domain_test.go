package counterparty

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const devnet = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"

// TestIdentify_OneEndpointHowEverItIsWritten: the spellings of one endpoint
// are one counterparty, so its record is not split across them.
func TestIdentify_OneEndpointHowEverItIsWritten(t *testing.T) {
	for _, raw := range []string{
		"https://api.weather.com/v1/forecast",
		"HTTPS://API.Weather.COM/v1/forecast",
		"https://api.weather.com./v1/forecast",
		"https://api.weather.com:443/v1/forecast",
		"https://api.weather.com/v1/forecast/",
		"https://api.weather.com//v1/./forecast",
		"https://api.weather.com/v1/forecast?city=paris&key=secret#today",
		"https://user:password@api.weather.com/v1/forecast",
		"http://api.weather.com/v1/forecast",
		"  https://api.weather.com/v1/forecast  ",
	} {
		id, ok := Identify(Observation{ResourceURL: raw})
		if assert.True(t, ok, raw) {
			assert.Equal(t, "endpoint:api.weather.com/v1/forecast", id.Key(), raw)
			assert.Equal(t, "weather.com", id.Domain, raw)
		}
	}
}

func TestIdentify_DifferentEndpointsStayApart(t *testing.T) {
	keys := map[string]bool{}
	for _, raw := range []string{
		"https://api.weather.com/v1/forecast",
		"https://api.weather.com/v2/forecast",
		"https://api.weather.com/V1/forecast", // paths are case-sensitive
		"https://maps.weather.com/v1/forecast",
		"https://api.weather.com:8443/v1/forecast",
	} {
		id, ok := Identify(Observation{ResourceURL: raw})
		assert.True(t, ok, raw)
		keys[id.Key()] = true
	}
	assert.Len(t, keys, 5)
}

func TestIdentify_RecordsTheRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"https://a.b.example.co.uk/x":   "example.co.uk",
		"https://weather.com":           "weather.com",
		"https://127.0.0.1:8080/x":      "127.0.0.1",
		"http://localhost:3000/x":       "localhost",
		"https://[2001:db8::1]/x":       "2001:db8::1",
		"https://bücher.example.de/x":   "example.de",
		"https://api.xn--bcher-kva.de/": "xn--bcher-kva.de",
	}
	for raw, domain := range cases {
		id, ok := Identify(Observation{ResourceURL: raw})
		if assert.True(t, ok, raw) {
			assert.Equal(t, domain, id.Domain, raw)
		}
	}
}

func TestIdentify_HostForms(t *testing.T) {
	cases := map[string]string{
		"https://[2001:DB8::1]/x":      "endpoint:[2001:db8::1]/x",
		"https://[2001:db8::1]:8443/x": "endpoint:[2001:db8::1]:8443/x",
		"https://bücher.example.de/x":  "endpoint:xn--bcher-kva.example.de/x",
		"https://weather.com":          "endpoint:weather.com/",
	}
	for raw, key := range cases {
		id, ok := Identify(Observation{ResourceURL: raw})
		if assert.True(t, ok, raw) {
			assert.Equal(t, key, id.Key(), raw)
		}
	}
}

// TestIdentify_FallsBackToTheAddress: with no usable URL the payee is where
// the money went.
func TestIdentify_FallsBackToTheAddress(t *testing.T) {
	for _, raw := range []string{
		"",
		"not a url",
		"ftp://files.example.com/x",
		"https:///no-host",
		"https://" + strings.Repeat("a", 300) + ".com/x",
		"https://example.com/" + strings.Repeat("p", 2100),
	} {
		id, ok := Identify(Observation{ResourceURL: raw, Network: devnet, PayTo: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"})
		if assert.True(t, ok, raw) {
			assert.Equal(t, KindAddress, id.Kind, raw)
			assert.Equal(t, "address:"+devnet+":7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", id.Key())
		}
	}
}

func TestIdentify_NothingToIdentify(t *testing.T) {
	for _, o := range []Observation{
		{},
		{Network: devnet},
		{PayTo: "7xKX"},
		{ResourceURL: "nope", Network: devnet},
		{Network: devnet, PayTo: strings.Repeat("a", 257)},
	} {
		_, ok := Identify(o)
		assert.False(t, ok, "%+v", o)
	}
}
