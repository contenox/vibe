package localtools

// BrowseFilter returns the listing predicate list_dir and find_files apply under default policy (the workspace .gitignore at absRoot plus default skip-directory basenames), reporting true for entries a listing omits; it is a noise filter, never access control.
func BrowseFilter(absRoot string) func(rel, base string, isDir bool) bool {
	f := entryFilter{
		skipDirs: skipDirNameSet(defaultSkipDirNames),
		ignore:   gitignoreFor(absRoot),
	}
	return f.skip
}
