package names

import (
	"net/url"
	"testing"
)

func TestParseProjectName(t *testing.T) {
	checkOk(t, ParseProject, ".index", Project{".index"})
	checkOk(t, ParseProject, "foo", Project{"foo"})
	checkOk(t, ParseProject, "foo@10", Project{"foo@10"})

	checkErr(t, ParseProject, ".foo", ErrInvalidProjectName)
	checkErr(t, ParseProject, "meow/miau", ErrInvalidProjectName)
	checkErr(t, ParseProject, "meow\\miau", ErrInvalidProjectName)
	checkErr(t, ParseProject, "meow%miau", ErrInvalidProjectName)
	checkErr(t, ParseProject, "meow*", ErrInvalidProjectName)
}

func TestParseProjectURL(t *testing.T) {
	checkOk(t, ParseProjectURL, must(url.Parse("")), Project{".index"})
	checkOk(t, ParseProjectURL, must(url.Parse("/")), Project{".index"})
	checkOk(t, ParseProjectURL, must(url.Parse("/foo")), Project{"foo"})
	checkOk(t, ParseProjectURL, must(url.Parse("/foo/")), Project{"foo"})

	checkErr(t, ParseProjectURL, must(url.Parse("/foo/bar")), ErrInvalidProjectURL)
	checkErr(t, ParseProjectURL, must(url.Parse("/foo*")), ErrInvalidProjectURL)
	checkErr(t, ParseProjectURL, must(url.Parse("/.index")), ErrInvalidProjectURL)
}

func TestProjectProperties(t *testing.T) {
	checkEq(t, Project{}.IsValid(), false)
	checkEq(t, Project{"foo"}.IsValid(), true)

	checkPanic(t, func() Project { return Project{}.MustValid() })
	checkEq(t, Project{"foo"}.MustValid(), Project{"foo"})

	checkPanic(t, func() string { return Project{}.String() })
	checkEq(t, Project{"foo"}.String(), "foo")
	checkEq(t, Project{".index"}.String(), ".index")

	checkPanic(t, func() bool { return Project{}.IsIndex() })
	checkEq(t, Project{"foo"}.IsIndex(), false)
	checkEq(t, Project{".index"}.IsIndex(), true)

	checkPanic(t, func() string { x, _, _ := Project{}.GetPullRequest(); return x })

	baseName, prNumber, isPreview := Project{"foo"}.GetPullRequest()
	checkEq(t, baseName, "foo")
	checkEq(t, prNumber, 0)
	checkEq(t, isPreview, false)

	baseName, prNumber, isPreview = Project{"foo@10@11"}.GetPullRequest()
	checkEq(t, baseName, "foo@10@11")
	checkEq(t, prNumber, 0)
	checkEq(t, isPreview, false)

	baseName, prNumber, isPreview = Project{"foo@-1"}.GetPullRequest()
	checkEq(t, baseName, "foo@-1")
	checkEq(t, prNumber, 0)
	checkEq(t, isPreview, false)

	baseName, prNumber, isPreview = Project{"foo@0"}.GetPullRequest()
	checkEq(t, baseName, "foo@0")
	checkEq(t, prNumber, 0)
	checkEq(t, isPreview, false)

	baseName, prNumber, isPreview = Project{"foo@meow"}.GetPullRequest()
	checkEq(t, baseName, "foo@meow")
	checkEq(t, prNumber, 0)
	checkEq(t, isPreview, false)

	baseName, prNumber, isPreview = Project{"foo@10"}.GetPullRequest()
	checkEq(t, baseName, "foo")
	checkEq(t, prNumber, 10)
	checkEq(t, isPreview, true)
}
