package names

import (
	"errors"
	"fmt"
	"testing"
)

func checkDomainParseOk(t *testing.T, mode ParseDomainMode, value string, expect string) {
	t.Helper()
	if actual, err := ParseDomain(value, mode); err != nil {
		t.Error(err)
	} else {
		if actual.name != expect {
			t.Errorf("unexpected value: %v != %v", actual, expect)
		}
	}
}

func checkDomainParseErr(t *testing.T, mode ParseDomainMode, value string, expect error) {
	t.Helper()
	_, err := ParseDomain(value, mode)
	if !errors.Is(err, expect) {
		t.Errorf("unexpected error: %v, expected %v", err, expect)
	}
}

func TestDomainParse(t *testing.T) {
	for _, mode := range []ParseDomainMode{ParseDomainStrict, ParseDomainRelaxed} {
		checkDomainParseOk(t, mode, "foo.bar", "foo.bar")
		checkDomainParseOk(t, mode, "foo.bar.", "foo.bar")
		checkDomainParseOk(t, mode, "foo-baz.bar", "foo-baz.bar")
		checkDomainParseOk(t, mode, "foo--baz.bar", "foo--baz.bar")
		checkDomainParseOk(t, mode, "ß.bar", "xn--zca.bar")
		checkDomainParseOk(t, mode, "xn--zca.bar", "xn--zca.bar")

		checkDomainParseErr(t, mode, "", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, ".foo.bar", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, "..foo.bar", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, "foo-.bar", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, "-foo.bar", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, "_foo.bar", ErrInvalidDomainName)
		checkDomainParseErr(t, mode, "*.foo.bar", ErrInvalidDomainName)
	}

	for _, label := range []string{"foo_", "foo_baz", "foo__baz"} {
		domain := fmt.Sprintf("%s.bar", label)
		checkDomainParseErr(t, ParseDomainStrict, domain, ErrInvalidDomainName)
		checkDomainParseOk(t, ParseDomainRelaxed, domain, domain)
	}
}

func TestDomainProperties(t *testing.T) {
	checkEq(t, Domain{}.IsValid(), false)
	checkEq(t, Domain{"example.org"}.IsValid(), true)

	checkPanic(t, func() Domain { return Domain{}.MustValid() })
	checkEq(t, Domain{"example.org"}.MustValid(), Domain{"example.org"})

	checkPanic(t, func() string { return Domain{}.String() })
	checkEq(t, Domain{"example.org"}.String(), "example.org")

	checkEq(t, Domain{"example.org"}.EffectiveTLDPlusOne([]Domain{}).String(),
		"example.org")
	checkEq(t, Domain{"foo.example.org"}.EffectiveTLDPlusOne([]Domain{}).String(),
		"example.org")
	checkEq(t, Domain{"example.co.uk"}.EffectiveTLDPlusOne([]Domain{}).String(),
		"example.co.uk")
	checkEq(t, Domain{"foo.example.co.uk"}.EffectiveTLDPlusOne([]Domain{}).String(),
		"example.co.uk")
	// domain below must be on PSL
	checkEq(t, Domain{"codeberg.page"}.EffectiveTLDPlusOne([]Domain{}).String(),
		"codeberg.page")
}
