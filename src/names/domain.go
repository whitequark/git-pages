package names

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

var ErrInvalidDomainName = NameError{"invalid domain name"}

// Domain of a git-pages site.
type Domain struct {
	name string
}

// IsValid checks whether the domain name is valid. The zero value is the only invalid value.
func (d Domain) IsValid() bool {
	return d.name != ""
}

// MustValid ensures the domain name is valid by panicking if it is not.
func (d Domain) MustValid() Domain {
	if !d.IsValid() {
		panic("nil domain name")
	}
	return d
}

// String returns the normalized domain name.
func (d Domain) String() string {
	return d.MustValid().name
}

var idnaProfile = idna.New(idna.MapForLookup(), idna.BidiRule())

type ParseDomainMode int

const (
	// RFC-compliant parsing mode.
	ParseDomainStrict ParseDomainMode = iota
	// Parsing mode that accepts `_` symbols in the middle and at the end of a label, and behaves
	// like ParseDomainNormal otherwise.
	ParseDomainRelaxed
)

// ParseDomain parses and normalizes a domain name according to the requirements set out in
// Section 5 of RFC 5891, as well as the Bidi Rule.
//
// The mappings include normalization and mapping case, width and other compatibility mappings.
func ParseDomain(name string, mode ParseDomainMode) (Domain, error) {
	if name == "" {
		return Domain{}, fmt.Errorf("%w: empty host name", ErrInvalidDomainName)
	}
	// this also rejects invalid characters and labels
	name, err := idnaProfile.ToASCII(name)
	if err != nil {
		if mode == ParseDomainRelaxed {
			// unfortunately, the go IDNA library has some significant issues around its
			// Unicode TR46 implementation: https://github.com/golang/go/issues/76804
			// we would like to allow *just* the _ here, but adding `idna.StrictDomainName(false)`
			// would also accept domains like `*.foo.bar` which should clearly be disallowed.
			// as a workaround, accept a domain name if it is valid with all `_` characters
			// replaced with an alphanumeric character (we use `a`); this allows e.g. `foo_bar.xxx`
			// and `foo__bar.xxx`, as well as `_foo.xxx` and `foo_.xxx`. labels starting with
			// an underscore are explicitly rejected below.
			_, err = idnaProfile.ToASCII(strings.ReplaceAll(name, "_", "a"))
		}
		if err != nil {
			return Domain{}, fmt.Errorf("%w: malformed host name %q", ErrInvalidDomainName, name)
		}
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return Domain{}, fmt.Errorf("%w: reserved host name %q", ErrInvalidDomainName, name)
	}

	return Domain{strings.TrimSuffix(name, ".")}, nil
}

func (d Domain) EffectiveTLDPlusOne(extraSuffixes []Domain) Domain {
	domainParts := strings.Split(d.name, ".")
	for _, suffix := range extraSuffixes {
		suffixParts := strings.Split(suffix.name, ".")
		if len(domainParts) < len(suffixParts)+1 {
			continue
		}
		if slices.Equal(domainParts[len(domainParts)-len(suffixParts):], suffixParts) {
			return Domain{strings.Join(domainParts[len(domainParts)-len(suffixParts)-1:], ".")}
		}
	}

	eTLDp1, err := publicsuffix.EffectiveTLDPlusOne(d.String())
	if err != nil {
		// This can happen if the domain itself is on the PSL, e.g. `codeberg.page`.
		return d
	}

	return Domain{eTLDp1}
}
