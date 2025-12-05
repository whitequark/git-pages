package git_pages

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var httpAcceptRegexp = regexp.MustCompile(`` +
	// token optionally prefixed by whitespace
	`^[ \t]*([a-zA-Z0-9$!#$%&'*+.^_\x60|~-]+)` +
	// quality value prefixed by a semicolon optionally surrounded by whitespace
	`(?:[ \t]*;[ \t]*q=(0(?:\.[0-9]{1,3})?|1(?:\.0{1,3})?))?` +
	// optional whitespace followed by comma or end of line
	`[ \t]*(?:,|$)`,
)

type httpAcceptOffer struct {
	code string
	qval float64
}

type HTTPEncodings struct {
	encodings []httpAcceptOffer
}

func ParseHTTPAcceptEncoding(headerValue string) (result HTTPEncodings) {
	for headerValue != "" {
		matches := httpAcceptRegexp.FindStringSubmatch(headerValue)
		if matches == nil {
			return HTTPEncodings{}
		}
		enc := httpAcceptOffer{strings.ToLower(matches[1]), 1.0}
		if matches[2] != "" {
			enc.qval, _ = strconv.ParseFloat(matches[2], 64)
		}
		result.encodings = append(result.encodings, enc)
		headerValue = headerValue[len(matches[0]):]
	}
	if len(result.encodings) == 0 {
		// RFC 9110 says (https://httpwg.org/specs/rfc9110.html#field.accept-encoding):
		// "If no Accept-Encoding header field is in the request, any content
		// coding is considered acceptable by the user agent."
		// In practice, no client expects to receive a compressed response
		// without having sent Accept-Encoding in the request.
	}
	return
}

// Negotiate returns the most preferred encoding that is acceptable by the
// client, or an empty string if no encodings are acceptable.
func (e *HTTPEncodings) Negotiate(offers ...string) string {
	prefs := make(map[string]float64, len(offers))
	for _, code := range offers {
		prefs[code] = 0
	}
	implicitIdentity := true
	for _, enc := range e.encodings {
		if enc.code == "*" {
			for code := range prefs {
				prefs[code] = enc.qval
			}
			implicitIdentity = false
		} else if _, ok := prefs[enc.code]; ok {
			prefs[enc.code] = enc.qval
		}
		if enc.code == "*" || enc.code == "identity" {
			implicitIdentity = false
		}
	}
	if _, ok := prefs["identity"]; ok && implicitIdentity {
		prefs["identity"] = -1 // sort last
	}
	encs := make([]httpAcceptOffer, len(offers))
	for idx, code := range offers {
		encs[idx] = httpAcceptOffer{code, prefs[code]}
	}
	slices.SortStableFunc(encs, func(a, b httpAcceptOffer) int {
		return -cmp.Compare(a.qval, b.qval)
	})
	for _, enc := range encs {
		if enc.qval != 0 {
			return enc.code
		}
	}
	return ""
}
