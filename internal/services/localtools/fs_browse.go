package localtools

// BrowseFilter returns the listing predicate list_dir and find_files apply
// under default policy: the workspace .gitignore at absRoot plus the default
// skip-directory basenames. It reports true for entries a listing omits.
//
// Exported for surfaces outside the toolset that browse the same tree (the
// editor bus's workspace explorer), so the operator's file tree and the
// agent's find_files see one tree rather than two that drift.
//
// Invariant: this is a noise filter, never an access control. Containment is
// vfs.Contain's job and denial is _denied_path_substrings'.
//
// rel is the slash-separated path relative to absRoot, base its final segment.
func BrowseFilter(absRoot string) func(rel, base string, isDir bool) bool {
	f := entryFilter{
		skipDirs: skipDirNameSet(defaultSkipDirNames),
		ignore:   gitignoreFor(absRoot),
	}
	return f.skip
}
