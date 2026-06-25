# git-spush

`git-spush` is a `git push`-like command for repositories hosted on GitHub.
Instead of sending commit objects through the Git smart protocol, it recreates
your local commits with GitHub's GraphQL `createCommitOnBranch` mutation.

The practical result is that pushed commits are created by GitHub and can be
verified as GitHub-signed commits.

## Quick Start

Install the command:

```sh
go install github.com/ryotarai/git-spush/cmd/git-spush@latest
```

Push the current branch to `origin/<current-branch>`:

```sh
git-spush
```

After the command finishes, your local branch is updated to the GitHub-created
signed commit history.

## What It Does

`git-spush` follows the common fast-forward `git push` shape:

1. Reads the target remote and branch.
2. Fetches the latest remote branch.
3. Finds local-only commits with `git rev-list --reverse <remote>..<local>`.
4. Recreates each local commit on GitHub with `createCommitOnBranch`.
5. Resets the local branch to the old remote head.
6. Runs `git pull --ff-only` so the local branch receives the GitHub-created
   signed commits.

Commit order and commit messages are preserved. Commit object IDs change because
GitHub creates new commit objects.

## Install

### Go Install

```sh
go install github.com/ryotarai/git-spush/cmd/git-spush@latest
```

### Prebuilt Binaries

GitHub Releases include archives for:

- macOS amd64 / arm64
- Linux amd64 / arm64
- Windows amd64 / arm64

Each release also includes `checksums.txt`.

### Local Build

```sh
go build -o git-spush ./cmd/git-spush
```

## Usage

```sh
git-spush [remote] [src[:dst]]
```

Examples:

```sh
# Push current branch to origin/<current-branch>.
git-spush

# Push local main to origin/main.
git-spush origin main

# Push HEAD to origin/main.
git-spush origin HEAD:main

# Push topic and set upstream to origin/topic.
git-spush -u origin topic
```

Defaults:

- `remote` defaults to `origin`.
- If no refspec is provided, the current branch is pushed to the same branch name.
- `-u` / `--set-upstream` configures the upstream after the signed commits are
  pulled.

## Authentication

`git-spush` needs a GitHub token that can write repository contents. For local
use, authenticating the GitHub CLI is usually enough:

```sh
gh auth login
git-spush
```

Token lookup order:

1. `GITHUB_TOKEN`
2. `git config --get github.token`
3. `gh auth token`

## Safety Model

`git-spush` requires a clean worktree and a clean index before it starts. It
rejects both staged and unstaged uncommitted changes because the command resets
the local branch after the GitHub API calls succeed.

Untracked files are ignored, matching normal `git push` behavior.

If the remote branch has advanced, `git-spush` rejects the push as
non-fast-forward before creating GitHub commits.

## Compatibility With Git Push

Supported:

- Existing GitHub remote branches.
- Fast-forward pushes.
- Multiple local commits.
- Basic refspecs such as `main` and `HEAD:main`.
- `-u` / `--set-upstream`.
- Add, modify, delete, copy, and rename file changes as content changes.

Explicitly unsupported:

- `--force`
- `--force-with-lease`
- Non-GitHub remotes
- Non-fast-forward updates

Known gaps:

- Creating a new remote branch is not implemented yet.
- Mode-only changes, symlinks, and submodule updates need more investigation
  because `createCommitOnBranch` accepts file content changes, not arbitrary Git
  object updates.
- Advanced `git push` refspec forms such as deletion refspecs are not supported.
- Commit metadata is not byte-for-byte identical to the local commit. GitHub
  creates new signed commits.

## Release Process

Pushing a tag that matches `v*` creates a GitHub Release and uploads binaries for
macOS, Linux, and Windows on amd64 and arm64:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The release workflow runs `go test ./...` before building release assets.

## Development

Run tests:

```sh
go test ./...
```

Build locally:

```sh
go build ./cmd/git-spush
```

The end-to-end test repository is `ryotarai/git-spush-e2e-sandbox`. Reuse that
repository for GitHub API testing instead of creating a new repository for every
test run.
