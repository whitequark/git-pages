package git_pages

import "testing"

func TestHttpContentEncodingNegotiation(t *testing.T) {
	check := func(available []string, requested string, expected string) {
		parsed := ParseAcceptEncodingHeader(requested)
		negotiated := parsed.Negotiate(available...)
		if negotiated != expected {
			t.Errorf("accept %q, offer %q: expect %q, got %q",
				requested, available, expected, negotiated)
		}
	}

	// quality sorting
	check([]string{"zstd", "identity"}, "zstd;q=1.0, identity;q=0.5", "zstd")
	check([]string{"zstd", "identity"}, "zstd;q=1.0, identity;q=1.0", "zstd")
	check([]string{"zstd", "identity"}, "zstd;q=0.5, identity;q=1.0", "identity")

	// implicit identity
	check([]string{"zstd", "identity"}, "", "identity")
	check([]string{"zstd", "identity"}, "zstd;q=0", "identity")
	check([]string{"zstd", "identity"}, "compress;q=0", "identity")

	// wildcard fallback
	check([]string{"zstd", "identity"}, "*", "zstd")
	check([]string{"zstd", "identity"}, "identity;q=1, *;q=0", "identity")
	check([]string{"zstd", "identity"}, "*;q=0, identity;q=1", "identity")

	// negotiation failure
	check([]string{"zstd", "identity"}, "*;q=0", "")
	check([]string{"zstd", "identity"}, "identity;q=0", "")
	check([]string{"zstd", "identity"}, "gzip;q=1.0, *;q=0", "")

	// parser lenience
	check([]string{"zstd", "identity"}, "zstd;q= 1.0", "identity")
	check([]string{"zstd", "identity"}, "zstd;q=1.0000", "identity")
	check([]string{"zstd", "identity"}, " zstd ; q=1.0, identity  ;  q=0.5  , *;q=0 ", "zstd")
}
