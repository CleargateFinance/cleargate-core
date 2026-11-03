package counterparty

import (
	"net"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// Kind says what a counterparty's identity is made of.
type Kind string

const (
	// KindEndpoint is identified by the endpoint the agent reported paying
	// for: host and path.
	KindEndpoint Kind = "endpoint"
	// KindAddress is identified by the address the money went to, used when
	// the agent reported no usable URL.
	KindAddress Kind = "address"
)

// Bounds the counterparty table holds too. An endpoint beyond them is not
// refused, only not used as an identity, and the address is used instead.
const (
	maxHostLength    = 259 // a 253-character name, a colon and a port
	maxPathLength    = 2048
	maxAddressLength = 256
)

// Observation is what a payment says about its payee.
type Observation struct {
	// ResourceURL is the endpoint the agent reported paying for. It is the
	// agent's own account, so it identifies a counterparty without proving
	// anything about it.
	ResourceURL string

	// Network and PayTo are where the money goes: a CAIP-2 network and the
	// address on it.
	Network string
	PayTo   string
}

// Identity is a counterparty's identity, in the normal form it is stored in.
type Identity struct {
	Kind Kind

	// An endpoint's registrable domain, host and path. Domain and host are
	// the levels distrust is scored at (data-architecture.md §4.4), so they
	// are kept even though Key does not use the domain.
	Domain string
	Host   string
	Path   string

	// An address's network and the address exactly as given.
	Network string
	Address string
}

// Key is the identity as one string, the same one the database derives into
// counterparty.canonical_key.
func (i Identity) Key() string {
	if i.Kind == KindEndpoint {
		return "endpoint:" + i.Host + i.Path
	}
	return "address:" + i.Network + ":" + i.Address
}

// Identify returns the identity an observation resolves to: the endpoint when
// the URL is usable, otherwise the address, and false when there is neither.
//
// This is the owner's rule of 3 November 2026. Reputation should follow the
// endpoint, which a merchant keeps, rather than the address, which it can
// rotate for nothing. The address stays on every payment, so an address
// change behind a known endpoint remains visible.
func Identify(o Observation) (Identity, bool) {
	if id, ok := endpoint(o.ResourceURL); ok {
		return id, true
	}
	if o.Network != "" && o.PayTo != "" && len(o.PayTo) <= maxAddressLength {
		return Identity{Kind: KindAddress, Network: o.Network, Address: o.PayTo}, true
	}
	return Identity{}, false
}

// endpoint normalises a URL to host and path, so the ways of writing one
// endpoint are one counterparty: the scheme, user info, query and fragment
// are dropped, the host is lower-cased ASCII without a trailing dot or a
// default port, and the path is cleaned of dot segments, repeated slashes and
// a trailing slash.
//
// The query never takes part. It varies per request, and it is where a
// careless client puts an API key.
func endpoint(raw string) (Identity, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Identity{}, false
	}

	name, ok := hostName(u.Hostname())
	if !ok {
		return Identity{}, false
	}

	host := name
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		host = net.JoinHostPort(name, port)
	} else if strings.Contains(name, ":") {
		host = "[" + name + "]" // an IPv6 literal keeps its brackets
	}

	p := path.Clean("/" + u.EscapedPath())
	if len(host) > maxHostLength || len(p) > maxPathLength {
		return Identity{}, false
	}

	return Identity{Kind: KindEndpoint, Domain: registrableDomain(name), Host: host, Path: p}, true
}

// hostName is the host in one form: an IP address in its canonical text, any
// other name lower-cased, without a trailing dot, and in its ASCII (punycode)
// form, so bücher.de and xn--bcher-kva.de are one host.
func hostName(raw string) (string, bool) {
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String(), true
	}
	name, err := idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(raw), "."))
	return name, err == nil && name != ""
}

// registrableDomain is the name a registrant controls, weather.com for
// api.weather.com, from the public suffix list. A name with none, an IP
// address or localhost, is its own domain.
func registrableDomain(name string) string {
	if net.ParseIP(name) != nil {
		return name
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return name
	}
	return d
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}
