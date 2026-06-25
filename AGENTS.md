# Repository Instructions

- Communicate with users in Japanese, but write materials such as code and documents in English.
- When a GitHub or `gh` command reports an authentication or repository visibility problem inside the sandbox, do not assume the user is logged out or that a private repository is missing. Re-run the minimal diagnostic command with escalated permissions first, then use that result.
- Before writing to any file in a git-managed directory, always create a git worktree and make the changes there. Worktree directories should be located under `./tmp/worktrees` and ignored by git.
- For GitHub end-to-end tests, reuse one private test repository instead of creating a new repository for every test run. Use `ryotarai/git-spush-e2e-sandbox` unless the user specifies another repository. Create it only if it does not already exist, reset its test branches between cases, and do not delete it after the run.
