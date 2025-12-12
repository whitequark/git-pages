package git_pages

import (
	"net/http"
	"strings"
	"testing"
)

func checkHost(t *testing.T, host string, expectOk string, expectErr string) {
	host, err := GetHost(&http.Request{Host: host})
	if expectErr != "" {
		if err == nil || !strings.HasPrefix(err.Error(), expectErr) {
			t.Errorf("%s: expect err %s, got err %s", host, expectErr, err)
		}
	}
	if expectOk != "" {
		if err != nil {
			t.Errorf("%s: expect ok %s, got err %s", host, expectOk, err)
		} else if host != expectOk {
			t.Errorf("%s: expect ok %s, got ok %s", host, expectOk, host)
		}
	}
}

func TestHelloName(t *testing.T) {
	config = &Config{Features: []string{}}

	checkHost(t, "foo.bar", "foo.bar", "")
	checkHost(t, "foo-baz.bar", "foo-baz.bar", "")
	checkHost(t, "foo--baz.bar", "foo--baz.bar", "")
	checkHost(t, "foo.bar.", "foo.bar", "")
	checkHost(t, ".foo.bar", "", "reserved host name")
	checkHost(t, "..foo.bar", "", "reserved host name")

	checkHost(t, "ß.bar", "xn--zca.bar", "")
	checkHost(t, "xn--zca.bar", "xn--zca.bar", "")

	checkHost(t, "foo-.bar", "", "malformed host name")
	checkHost(t, "-foo.bar", "", "malformed host name")
	checkHost(t, "foo_.bar", "", "malformed host name")
	checkHost(t, "_foo.bar", "", "malformed host name")
	checkHost(t, "foo_baz.bar", "", "malformed host name")
	checkHost(t, "foo__baz.bar", "", "malformed host name")
	checkHost(t, "*.foo.bar", "", "malformed host name")

	config = &Config{Features: []string{"relaxed-idna"}}

	checkHost(t, "foo-.bar", "", "malformed host name")
	checkHost(t, "-foo.bar", "", "malformed host name")
	checkHost(t, "foo_.bar", "foo_.bar", "")
	checkHost(t, "_foo.bar", "", "reserved host name")
	checkHost(t, "foo_baz.bar", "foo_baz.bar", "")
	checkHost(t, "foo__baz.bar", "foo__baz.bar", "")
	checkHost(t, "*.foo.bar", "", "malformed host name")
}
