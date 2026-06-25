# git-spush

`git-spush` is a `git push`-like command that creates the remote commit through
GitHub's GraphQL `createCommitOnBranch` mutation instead of sending local commits
with the Git smart protocol.

The command compares the local ref with the selected remote branch, sends the
tree changes as one GitHub-created commit, resets the local branch to the old
remote head, and then runs `git pull --ff-only` so the local branch ends at the
GitHub-signed commit.

## Install

```sh
go install github.com/ryotarai/git-spush/cmd/git-spush@latest
```

For local development:

```sh
go build -o git-spush ./cmd/git-spush
```

## Usage

```sh
GH_TOKEN="$(gh auth token)" git-spush [remote] [src[:dst]]
```

Examples:

```sh
GH_TOKEN="$(gh auth token)" git-spush
GH_TOKEN="$(gh auth token)" git-spush origin main
GH_TOKEN="$(gh auth token)" git-spush -u origin HEAD:main
```

Defaults match common `git push` usage:

- `remote` defaults to `origin`.
- If no refspec is provided, the current branch is pushed to the same branch name.
- `-u` / `--set-upstream` configures the upstream after the signed commit is
  pulled.

Unsupported options such as `--force` and `--force-with-lease` fail explicitly.
`createCommitOnBranch` requires an expected remote head and cannot represent a
non-fast-forward push.

## Safety

`git-spush` requires a clean worktree and clean index. This is necessary because
the local unsigned commits are replaced by the GitHub-created signed commit after
the API call succeeds.

The resulting remote commit has the same final tree as the local ref, but it is a
single commit whose parent is the previous remote branch head.
