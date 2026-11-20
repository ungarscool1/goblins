package core

import "context"

// RepoFile is a regular file of a repository's tree. ID names its content, so equal IDs are
// equal content.
type RepoFile struct {
	Path string
	ID   string
	Size int64
}

// RepoTree lists a repository's regular files at one commit. Links and submodules are not
// files, and are left out.
type RepoTree struct {
	ID        string
	Files     []RepoFile
	Truncated bool
}

// RepositoryReader reads a repository's files at a commit through the SCM's API, with no
// checkout and no filesystem, so nothing in the repository can point a read elsewhere.
type RepositoryReader interface {
	Tree(ctx context.Context, repo RepoRef, ref string) (RepoTree, error)
	Blob(ctx context.Context, repo RepoRef, id string) ([]byte, error)
}
