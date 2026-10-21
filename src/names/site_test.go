package names

import (
	"testing"
)

func TestParseSite(t *testing.T) {
	checkOk(t, ParseSite, "example.org/foo", Site{Domain{"example.org"}, Project{"foo"}})
	checkOk(t, ParseSite, "example.org/.index", Site{Domain{"example.org"}, Project{".index"}})
	checkOk(t, ParseSite, "foo_bar.example.org/.index", Site{Domain{"foo_bar.example.org"}, Project{".index"}})

	checkErr(t, ParseSite, "example.org", ErrInvalidSiteName)
	checkErr(t, ParseSite, "example.org/a*", ErrInvalidProjectName)
	checkErr(t, ParseSite, "_foo.example.org/.index", ErrInvalidDomainName)
}

func TestSiteProperties(t *testing.T) {
	checkEq(t, Site{}.IsValid(), false)
	checkEq(t, Site{Domain{"example.org"}, Project{}}.IsValid(), false)
	checkEq(t, Site{Domain{}, Project{"foo"}}.IsValid(), false)
	checkEq(t, Site{Domain{"example.org"}, Project{"foo"}}.IsValid(), true)

	checkPanic(t, func() Site { return Site{}.MustValid() })
	checkEq(t, Site{Domain{"example.org"}, Project{"foo"}}.MustValid().Project, Project{"foo"})

	checkPanic(t, func() string { return Site{}.String() })
	checkEq(t, Site{Domain{"example.org"}, Project{"foo"}}.String(), "example.org/foo")
	checkEq(t, Site{Domain{"example.org"}, IndexProject}.String(), "example.org/.index")
}
