package git_pages

import (
	"cmp"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var httpAcceptRegexp = regexp.MustCompile(`` +
	// token optionally prefixed by whitespace
	`^[ \t]*([a-zA-Z0-9$!#$%&'*+./^_\x60|~-]+)` +
	// quality value prefixed by a semicolon optionally surrounded by whitespace
	`(?:[ \t]*;[ \t]*q=(0(?:\.[0-9]{1,3})?|1(?:\.0{1,3})?))?` +
	// optional whitespace followed by comma or end of line
	`[ \t]*(?:,|$)`,
)

type httpAcceptOffer struct {
	code string
	qval float64
}

func parseGenericAcceptHeader(headerValue string) (result []httpAcceptOffer) {
	for headerValue != "" {
		matches := httpAcceptRegexp.FindStringSubmatch(headerValue)
		if matches == nil {
			return
		}
		offer := httpAcceptOffer{strings.ToLower(matches[1]), 1.0}
		if matches[2] != "" {
			offer.qval, _ = strconv.ParseFloat(matches[2], 64)
		}
		result = append(result, offer)
		headerValue = headerValue[len(matches[0]):]
	}
	return
}

func preferredAcceptOffer(offers []httpAcceptOffer) string {
	slices.SortStableFunc(offers, func(a, b httpAcceptOffer) int {
		return -cmp.Compare(a.qval, b.qval)
	})
	for _, offer := range offers {
		if offer.qval != 0 {
			return offer.code
		}
	}
	return ""
}

type HTTPContentTypes struct {
	contentTypes []httpAcceptOffer
}

func ParseAcceptHeader(headerValue string) (result HTTPContentTypes) {
	if headerValue == "" {
		headerValue = "*/*"
	}
	result = HTTPContentTypes{parseGenericAcceptHeader(headerValue)}
	return
}

func (e *HTTPContentTypes) Negotiate(offers ...string) string {
	prefs := make(map[string]float64, len(offers))
	for _, code := range offers {
		prefs[code] = 0
	}
	for _, ctyp := range e.contentTypes {
		if ctyp.code == "*/*" {
			for code := range prefs {
				prefs[code] = ctyp.qval
			}
		} else if _, ok := prefs[ctyp.code]; ok {
			prefs[ctyp.code] = ctyp.qval
		}
	}
	ctyps := make([]httpAcceptOffer, len(offers))
	for idx, code := range offers {
		ctyps[idx] = httpAcceptOffer{code, prefs[code]}
	}
	return preferredAcceptOffer(ctyps)
}

type HTTPEncodings struct {
	encodings []httpAcceptOffer
}

func ParseAcceptEncodingHeader(headerValue string) (result HTTPEncodings) {
	result = HTTPEncodings{parseGenericAcceptHeader(headerValue)}
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
	return preferredAcceptOffer(encs)
}

func chainHTTPMiddleware(middleware ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		for idx := len(middleware) - 1; idx >= 0; idx-- {
			handler = middleware[idx](handler)
		}
		return handler
	}
}

func remoteAddrMiddleware(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var readXForwardedFor bool
		switch config.Audit.IncludeIPs {
		case "X-Forwarded-For":
			readXForwardedFor = true
		case "RemoteAddr", "":
			readXForwardedFor = false
		default:
			panic(fmt.Errorf("config.Audit.IncludeIPs is set to an unknown value (%q)",
				config.Audit.IncludeIPs))
		}

		usingOriginalRemoteAddr := true
		if readXForwardedFor {
			forwardedFor := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
			if len(forwardedFor) > 0 {
				remoteAddr := strings.TrimSpace(forwardedFor[len(forwardedFor)-1])
				if remoteAddr != "" {
					r.RemoteAddr = remoteAddr
					usingOriginalRemoteAddr = false
				}
			}
		}
		if usingOriginalRemoteAddr {
			if ipAddress, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				r.RemoteAddr = ipAddress
			}
		}

		handler.ServeHTTP(w, r)
	})
}
