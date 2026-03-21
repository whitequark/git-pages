package git_pages

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/gitattributes"
)

func ReadGitAttributes(ctx context.Context, manifest *Manifest) gitattributes.Matcher {
	type entryPair struct {
		parts []string
		entry *Entry
	}

	// Collect all .gitattributes files.
	var files []entryPair
	for name, entry := range manifest.GetContents() {
		switch entry.GetType() {
		case Type_InlineFile, Type_ExternalFile:
			parts := strings.Split(name, "/")
			if parts[len(parts)-1] == ".gitattributes" {
				files = append(files, entryPair{parts, entry})
			}
		}
	}

	// Sort the file list by depth, then by name.
	slices.SortFunc(files, func(a entryPair, b entryPair) int {
		return cmp.Or(
			cmp.Compare(len(a.parts), len(b.parts)),
			slices.Compare(a.parts, b.parts),
		)
	})

	// Gather all .gitattributes rules, sorted by depth.
	var rules []gitattributes.MatchAttribute
	for _, pair := range files {
		parts, entry := pair.parts, pair.entry
		data, err := GetEntryContents(ctx, entry)
		if err != nil {
			continue
		}
		dirs := parts[:len(parts)-1]
		isRoot := len(parts) == 1
		newRules, err := gitattributes.ReadAttributes(bytes.NewReader(data), dirs, isRoot)
		if err != nil {
			AddProblem(manifest, strings.Join(parts, "/"), "parsing .gitattributes: %v", err)
			continue
		}
		rules = append(rules, newRules...)
	}

	// gitattributes.Matcher applies rules in reverse.
	slices.Reverse(rules)
	matcher := gitattributes.NewMatcher(rules)
	return matcher
}
