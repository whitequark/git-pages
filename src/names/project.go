package names

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var ErrInvalidProjectName = NameError{"invalid project name"}
var ErrInvalidProjectURL = NameError{"invalid project URL"}

const indexProject = ".index"

// Name of a git-pages project. The index project corresponds to the root URL of a domain, and
// non-index projects are distinguished by the first path segment of the URL.
//
// Most operations (other than IsValid) on an invalid project name will panic.
type Project struct {
	name string
}

var IndexProject = Project{".index"}

// IsValid checks whether the project name is valid. The zero value is the only invalid value.
func (p Project) IsValid() bool {
	return p.name != ""
}

// MustValid ensures the project name is valid by panicking if it is not.
func (p Project) MustValid() Project {
	if !p.IsValid() {
		panic("nil project name")
	}
	return p
}

// String returns the project name (".index" for the index project).
func (p Project) String() string {
	return p.MustValid().name
}

// IsIndex checks whether this name corresponds to the index project.
func (p Project) IsIndex() bool {
	return p.MustValid() == IndexProject
}

// GetPullRequest returns (baseName, prNumber, isPreview), where isPreview is true if this is
// a preview project name for a pull request. In the latter case, prNumber is set correspondingly,
// and baseName contains the repository name. Otherwise, baseName equals p.String().
func (p Project) GetPullRequest() (baseName string, prNumber int64, isPreview bool) {
	if repoName, atSuffix, isPreview := strings.Cut(p.String(), "@"); isPreview {
		if prNumber, err := strconv.ParseInt(atSuffix, 10, 64); err == nil && prNumber > 0 {
			baseName = repoName
			return repoName, prNumber, true
		}
	}
	return p.String(), 0, false
}

func validateProjectName(name string) (proj Project, err error) {
	if strings.HasPrefix(name, ".") {
		err = fmt.Errorf("%w: must not start with %q", ErrInvalidProjectName, ".")
		return
	}

	forbiddenChars := "%*\\/"
	if strings.ContainsAny(name, forbiddenChars) {
		err = fmt.Errorf("%w: must not contain any of %q", ErrInvalidProjectName, forbiddenChars)
		return
	}

	proj.name = name
	return
}

// ParseProject parses the given string as a project name. The string must not be empty, and
// ".index" represents the index project.
func ParseProject(name string) (Project, error) {
	if name == IndexProject.name {
		return IndexProject, nil
	}

	return validateProjectName(name)
}

// ParseProjectURL parses the given project URL and extracts the project name. The URL must have
// zero or one path segments,
func ParseProjectURL(url *url.URL) (Project, error) {
	// path must be either `/`, `/foo`, or `/foo/`
	path := strings.TrimPrefix(strings.TrimSuffix(url.Path, "/"), "/")
	if strings.Contains(path, "/") {
		return Project{}, fmt.Errorf("%w: must contain zero or one path segments", ErrInvalidProjectURL)
	} else if path == "" {
		return IndexProject, nil
	} else if name, err := validateProjectName(path); err != nil {
		return Project{}, fmt.Errorf("%w: %w", ErrInvalidProjectURL, err)
	} else {
		return name, nil
	}
}
