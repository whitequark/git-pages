package names

import (
	"cmp"
	"fmt"
	"strings"
)

var ErrInvalidSiteName = NameError{"invalid site name"}

const siteSeparator = "/"

// Name of a git-pages site. Consists of a Domain and a Project name.
type Site struct {
	Domain  Domain
	Project Project
}

func (s Site) IsValid() bool {
	return s.Domain.IsValid() && s.Project.IsValid()
}

// MustValid ensures the domain name is valid by panicking if it is not.
func (s Site) MustValid() Site {
	if !s.IsValid() {
		panic("nil site name")
	}
	return s
}

// String returns the Domain and Project names separated by a "/".
func (s Site) String() string {
	return fmt.Sprint(s.Domain.MustValid(), siteSeparator, s.Project.MustValid())
}

// CompareSite orders sites first by domain, then by project.
func CompareSite(a Site, b Site) int {
	if n := cmp.Compare(a.Domain.name, b.Domain.name); n != 0 {
		return n
	} else {
		return cmp.Compare(a.Project.name, b.Project.name)
	}
}

// ParseSite parses the format returned by String.
func ParseSite(value string) (s Site, err error) {
	if domainPart, projectPart, ok := strings.Cut(value, siteSeparator); ok {
		if s.Domain, err = ParseDomain(domainPart, ParseDomainRelaxed); err != nil {
			s = Site{}
			return
		}
		if s.Project, err = ParseProject(projectPart); err != nil {
			s = Site{}
			return
		}
	} else {
		err = ErrInvalidSiteName
	}
	return
}
