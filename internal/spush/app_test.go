package spush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestParseGitHubRemote(t *testing.T) {
	tests := []struct {
		name  string
		url   string
		owner string
		repo  string
	}{
		{name: "ssh", url: "git@github.com:ryotarai/git-spush.git", owner: "ryotarai", repo: "git-spush"},
		{name: "https", url: "https://github.com/ryotarai/git-spush.git", owner: "ryotarai", repo: "git-spush"},
		{name: "https without suffix", url: "https://github.com/ryotarai/git-spush", owner: "ryotarai", repo: "git-spush"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote, err := ParseGitHubRemote(tt.url)
			if err != nil {
				t.Fatalf("ParseGitHubRemote returned error: %v", err)
			}
			if remote.Owner != tt.owner || remote.Repo != tt.repo {
				t.Fatalf("remote = %#v, want owner=%q repo=%q", remote, tt.owner, tt.repo)
			}
		})
	}
}

func TestParsePushArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want PushOptions
	}{
		{
			name: "defaults to origin and current branch",
			args: nil,
			want: PushOptions{Remote: "origin", LocalRef: "HEAD"},
		},
		{
			name: "remote only",
			args: []string{"upstream"},
			want: PushOptions{Remote: "upstream", LocalRef: "HEAD"},
		},
		{
			name: "remote and branch refspec",
			args: []string{"origin", "feature"},
			want: PushOptions{Remote: "origin", LocalRef: "feature", RemoteBranch: "feature"},
		},
		{
			name: "remote and explicit src dst refspec",
			args: []string{"origin", "HEAD:main"},
			want: PushOptions{Remote: "origin", LocalRef: "HEAD", RemoteBranch: "main"},
		},
		{
			name: "accept set upstream flag",
			args: []string{"-u", "origin", "topic"},
			want: PushOptions{Remote: "origin", LocalRef: "topic", RemoteBranch: "topic", SetUpstream: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePushArgs(tt.args)
			if err != nil {
				t.Fatalf("ParsePushArgs returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("options = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParsePushArgsRejectsUnsupportedForce(t *testing.T) {
	_, err := ParsePushArgs([]string{"--force", "origin", "main"})
	if err == nil {
		t.Fatal("ParsePushArgs returned nil error for --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %q, want it to mention --force", err)
	}
}

func TestBuildFileChangesFromGitDiff(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git diff --name-status -z origin/main HEAD": "M\x00README.md\x00D\x00old.txt\x00A\x00cmd/git-spush/main.go\x00R100\x00before.txt\x00after.txt\x00",
		"git show HEAD:README.md":                    "updated readme",
		"git show HEAD:cmd/git-spush/main.go":        "package main\n",
		"git show HEAD:after.txt":                    "renamed content",
	})

	changes, err := BuildFileChanges(context.Background(), git, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("BuildFileChanges returned error: %v", err)
	}

	wantAdditions := []FileAddition{
		{Path: "README.md", Contents: "updated readme"},
		{Path: "cmd/git-spush/main.go", Contents: "package main\n"},
		{Path: "after.txt", Contents: "renamed content"},
	}
	wantDeletions := []FileDeletion{
		{Path: "old.txt"},
		{Path: "before.txt"},
	}
	if !slices.Equal(changes.Additions, wantAdditions) {
		t.Fatalf("additions = %#v, want %#v", changes.Additions, wantAdditions)
	}
	if !slices.Equal(changes.Deletions, wantDeletions) {
		t.Fatalf("deletions = %#v, want %#v", changes.Deletions, wantDeletions)
	}
}

func TestCreateCommitOnBranchRequest(t *testing.T) {
	var got struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer token-123" {
			t.Fatalf("Authorization = %q, want Bearer token-123", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"data":{"createCommitOnBranch":{"commit":{"oid":"signed-oid"}}}}`)
	}))
	defer server.Close()

	client := NewGitHubClient(server.URL, "token-123")
	oid, err := client.CreateCommitOnBranch(context.Background(), CreateCommitInput{
		RepositoryNameWithOwner: "ryotarai/git-spush",
		BranchName:              "main",
		ExpectedHeadOID:         "remote-oid",
		MessageHeadline:         "update files",
		MessageBody:             "body text",
		FileChanges: FileChanges{
			Additions: []FileAddition{{Path: "README.md", Contents: "hello"}},
			Deletions: []FileDeletion{{Path: "old.txt"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateCommitOnBranch returned error: %v", err)
	}
	if oid != "signed-oid" {
		t.Fatalf("oid = %q, want signed-oid", oid)
	}
	if !strings.Contains(got.Query, "createCommitOnBranch") {
		t.Fatalf("query does not contain mutation: %s", got.Query)
	}
	input := got.Variables["input"].(map[string]any)
	if input["expectedHeadOid"] != "remote-oid" {
		t.Fatalf("expectedHeadOid = %#v, want remote-oid", input["expectedHeadOid"])
	}
	branch := input["branch"].(map[string]any)
	if branch["repositoryNameWithOwner"] != "ryotarai/git-spush" || branch["branchName"] != "main" {
		t.Fatalf("branch = %#v", branch)
	}
	fileChanges := input["fileChanges"].(map[string]any)
	additions := fileChanges["additions"].([]any)
	firstAddition := additions[0].(map[string]any)
	if firstAddition["contents"] != "aGVsbG8=" {
		t.Fatalf("encoded contents = %#v, want base64 hello", firstAddition["contents"])
	}
}

func TestRunWithDepsCreatesCommitAndPulls(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git branch --show-current":                         "main\n",
		"git remote get-url origin":                         "git@github.com:ryotarai/git-spush.git\n",
		"git diff --quiet":                                  "",
		"git diff --cached --quiet":                         "",
		"git fetch origin main":                             "",
		"git rev-parse HEAD":                                "local-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git rev-list --reverse origin/main..HEAD":          "commit-1\n",
		"git diff --name-status -z origin/main commit-1":    "M\x00README.md\x00",
		"git show commit-1:README.md":                       "hello",
		"git log -1 --format=%B commit-1":                   "update readme\n\nbody\n",
		"git diff --quiet HEAD origin/main":                 "",
		"git reset --hard remote-oid":                       "",
		"git pull --ff-only origin main":                    "",
	})
	git.outputsSeq = map[string][]string{
		"git rev-parse origin/main": {"remote-oid\n", "signed-oid\n"},
	}
	client := &fakeCommitClient{oid: "signed-oid"}
	var out strings.Builder

	err := runWithDeps(context.Background(), []string{}, map[string]string{"GITHUB_TOKEN": "token"}, &out, git, client)
	if err != nil {
		t.Fatalf("runWithDeps returned error: %v", err)
	}
	if client.input.RepositoryNameWithOwner != "ryotarai/git-spush" {
		t.Fatalf("repository = %q", client.input.RepositoryNameWithOwner)
	}
	if client.input.BranchName != "main" || client.input.ExpectedHeadOID != "remote-oid" {
		t.Fatalf("branch/head = %q/%q", client.input.BranchName, client.input.ExpectedHeadOID)
	}
	if client.input.MessageHeadline != "update readme" || client.input.MessageBody != "body" {
		t.Fatalf("message = %q / %q", client.input.MessageHeadline, client.input.MessageBody)
	}
	if !slices.Contains(git.calls, "git pull --ff-only origin main") {
		t.Fatalf("git calls = %#v, want pull after create", git.calls)
	}
	if !calledBefore(git.calls, "git diff --quiet HEAD origin/main", "git reset --hard remote-oid") {
		t.Fatalf("git calls = %#v, want tree verification before reset", git.calls)
	}
	if !calledBefore(git.calls, "git reset --hard remote-oid", "git pull --ff-only origin main") {
		t.Fatalf("git calls = %#v, want reset to old remote head before pull", git.calls)
	}
	if !strings.Contains(out.String(), "signed-oid") {
		t.Fatalf("output = %q, want signed oid", out.String())
	}
}

func TestRunWithDepsCreatesOneSignedCommitPerLocalCommit(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git branch --show-current":                        "main\n",
		"git remote get-url origin":                        "git@github.com:ryotarai/git-spush.git\n",
		"git diff --quiet":                                 "",
		"git diff --cached --quiet":                        "",
		"git fetch origin main":                            "",
		"git rev-parse HEAD":                               "commit-2\n",
		"git merge-base --is-ancestor remote-oid commit-2": "",
		"git rev-list --reverse origin/main..HEAD":         "commit-1\ncommit-2\n",
		"git diff --name-status -z origin/main commit-1":   "A\x00one.txt\x00",
		"git show commit-1:one.txt":                        "one",
		"git log -1 --format=%B commit-1":                  "first local commit\n\nfirst body\n",
		"git diff --name-status -z commit-1 commit-2":      "M\x00one.txt\x00A\x00two.txt\x00",
		"git show commit-2:one.txt":                        "one updated",
		"git show commit-2:two.txt":                        "two",
		"git log -1 --format=%B commit-2":                  "second local commit\n",
		"git diff --quiet HEAD origin/main":                "",
		"git reset --hard remote-oid":                      "",
		"git pull --ff-only origin main":                   "",
	})
	git.outputsSeq = map[string][]string{
		"git rev-parse origin/main": {"remote-oid\n", "signed-2\n"},
	}
	client := &fakeCommitClient{oids: []string{"signed-1", "signed-2"}}

	err := runWithDeps(context.Background(), nil, map[string]string{"GITHUB_TOKEN": "token"}, io.Discard, git, client)
	if err != nil {
		t.Fatalf("runWithDeps returned error: %v", err)
	}
	if len(client.inputs) != 2 {
		t.Fatalf("created %d commits, want 2: %#v", len(client.inputs), client.inputs)
	}
	first := client.inputs[0]
	if first.ExpectedHeadOID != "remote-oid" || first.MessageHeadline != "first local commit" || first.MessageBody != "first body" {
		t.Fatalf("first input = %#v", first)
	}
	if !slices.Equal(first.FileChanges.Additions, []FileAddition{{Path: "one.txt", Contents: "one"}}) {
		t.Fatalf("first additions = %#v", first.FileChanges.Additions)
	}
	second := client.inputs[1]
	if second.ExpectedHeadOID != "signed-1" || second.MessageHeadline != "second local commit" {
		t.Fatalf("second input = %#v", second)
	}
	wantSecondAdditions := []FileAddition{{Path: "one.txt", Contents: "one updated"}, {Path: "two.txt", Contents: "two"}}
	if !slices.Equal(second.FileChanges.Additions, wantSecondAdditions) {
		t.Fatalf("second additions = %#v, want %#v", second.FileChanges.Additions, wantSecondAdditions)
	}
	if !calledBefore(git.calls, "git diff --name-status -z origin/main commit-1", "git diff --name-status -z commit-1 commit-2") {
		t.Fatalf("git calls = %#v, want per-commit diffs in order", git.calls)
	}
	if !calledBefore(git.calls, "git diff --quiet HEAD origin/main", "git reset --hard remote-oid") {
		t.Fatalf("git calls = %#v, want tree verification before reset", git.calls)
	}
}

func TestRunWithDepsDoesNotResetWhenCreatedRemoteHeadDoesNotMatch(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git branch --show-current":                         "main\n",
		"git remote get-url origin":                         "git@github.com:ryotarai/git-spush.git\n",
		"git diff --quiet":                                  "",
		"git diff --cached --quiet":                         "",
		"git fetch origin main":                             "",
		"git rev-parse HEAD":                                "local-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git rev-list --reverse origin/main..HEAD":          "commit-1\n",
		"git diff --name-status -z origin/main commit-1":    "M\x00README.md\x00",
		"git show commit-1:README.md":                       "hello",
		"git log -1 --format=%B commit-1":                   "update readme\n",
	})
	git.outputsSeq = map[string][]string{
		"git rev-parse origin/main": {"remote-oid\n", "unexpected-oid\n"},
	}
	client := &fakeCommitClient{oid: "signed-oid"}

	err := runWithDeps(context.Background(), nil, map[string]string{"GITHUB_TOKEN": "token"}, io.Discard, git, client)
	if err == nil {
		t.Fatal("runWithDeps returned nil error for mismatched remote head")
	}
	if !strings.Contains(err.Error(), "remote head") {
		t.Fatalf("error = %q, want remote head mismatch", err)
	}
	if slices.Contains(git.calls, "git reset --hard remote-oid") {
		t.Fatalf("git calls = %#v, reset should not run after verification failure", git.calls)
	}
}

func TestRunWithDepsDoesNotResetWhenCreatedRemoteTreeDiffers(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git branch --show-current":                         "main\n",
		"git remote get-url origin":                         "git@github.com:ryotarai/git-spush.git\n",
		"git diff --quiet":                                  "",
		"git diff --cached --quiet":                         "",
		"git fetch origin main":                             "",
		"git rev-parse HEAD":                                "local-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git rev-list --reverse origin/main..HEAD":          "commit-1\n",
		"git diff --name-status -z origin/main commit-1":    "M\x00README.md\x00",
		"git show commit-1:README.md":                       "hello",
		"git log -1 --format=%B commit-1":                   "update readme\n",
	})
	git.outputsSeq = map[string][]string{
		"git rev-parse origin/main": {"remote-oid\n", "signed-oid\n"},
	}
	git.errors = map[string]error{"git diff --quiet HEAD origin/main": fmt.Errorf("trees differ")}
	client := &fakeCommitClient{oid: "signed-oid"}

	err := runWithDeps(context.Background(), nil, map[string]string{"GITHUB_TOKEN": "token"}, io.Discard, git, client)
	if err == nil {
		t.Fatal("runWithDeps returned nil error for differing remote tree")
	}
	if !strings.Contains(err.Error(), "remote tree") {
		t.Fatalf("error = %q, want remote tree mismatch", err)
	}
	if slices.Contains(git.calls, "git reset --hard remote-oid") {
		t.Fatalf("git calls = %#v, reset should not run after verification failure", git.calls)
	}
}

func TestRunWithDepsSetUpstreamConfiguresTrackingBranch(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git remote get-url origin":                         "https://github.com/ryotarai/git-spush.git\n",
		"git diff --quiet":                                  "",
		"git diff --cached --quiet":                         "",
		"git fetch origin topic":                            "",
		"git rev-parse topic":                               "local-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git rev-list --reverse origin/topic..topic":        "topic\n",
		"git diff --name-status -z origin/topic topic":      "M\x00README.md\x00",
		"git show topic:README.md":                          "hello",
		"git log -1 --format=%B topic":                      "update readme\n",
		"git diff --quiet topic origin/topic":               "",
		"git reset --hard remote-oid":                       "",
		"git pull --ff-only origin topic":                   "",
		"git branch --set-upstream-to=origin/topic topic":   "",
	})
	git.outputsSeq = map[string][]string{
		"git rev-parse origin/topic": {"remote-oid\n", "signed-oid\n"},
	}
	client := &fakeCommitClient{oid: "signed-oid"}

	err := runWithDeps(context.Background(), []string{"-u", "origin", "topic"}, map[string]string{"GITHUB_TOKEN": "token"}, io.Discard, git, client)
	if err != nil {
		t.Fatalf("runWithDeps returned error: %v", err)
	}
	if !slices.Contains(git.calls, "git branch --set-upstream-to=origin/topic topic") {
		t.Fatalf("git calls = %#v, want upstream configuration", git.calls)
	}
}

func TestRunWithDepsRejectsDirtyWorktreeBeforeCreatingCommit(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git branch --show-current": "main\n",
		"git remote get-url origin": "git@github.com:ryotarai/git-spush.git\n",
	})
	git.errors = map[string]error{"git diff --quiet": fmt.Errorf("dirty")}
	client := &fakeCommitClient{oid: "signed-oid"}

	err := runWithDeps(context.Background(), nil, map[string]string{"GITHUB_TOKEN": "token"}, io.Discard, git, client)
	if err == nil {
		t.Fatal("runWithDeps returned nil error for dirty worktree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error = %q, want uncommitted changes message", err)
	}
	if client.input.RepositoryNameWithOwner != "" {
		t.Fatalf("client was called despite dirty worktree: %#v", client.input)
	}
}

func TestGitHubTokenIgnoresGHToken(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git config --get github.token": "config-token\n",
	})

	token, err := githubToken(context.Background(), git, map[string]string{
		"GH_TOKEN": "ignored-token",
	})
	if err != nil {
		t.Fatalf("githubToken returned error: %v", err)
	}
	if token != "config-token" {
		t.Fatalf("token = %q, want config-token", token)
	}
}

func TestGitHubTokenPrefersGitHubToken(t *testing.T) {
	git := newFakeGit(nil)

	token, err := githubToken(context.Background(), git, map[string]string{
		"GITHUB_TOKEN": "env-token",
		"GH_TOKEN":     "ignored-token",
	})
	if err != nil {
		t.Fatalf("githubToken returned error: %v", err)
	}
	if token != "env-token" {
		t.Fatalf("token = %q, want env-token", token)
	}
}

type fakeGit struct {
	outputs    map[string]string
	outputsSeq map[string][]string
	errors     map[string]error
	calls      []string
}

func newFakeGit(outputs map[string]string) *fakeGit {
	return &fakeGit{outputs: outputs}
}

func (g *fakeGit) Run(ctx context.Context, args ...string) (string, error) {
	key := "git " + strings.Join(args, " ")
	g.calls = append(g.calls, key)
	if err, ok := g.errors[key]; ok {
		return "", err
	}
	if outputs, ok := g.outputsSeq[key]; ok && len(outputs) > 0 {
		out := outputs[0]
		g.outputsSeq[key] = outputs[1:]
		return out, nil
	}
	out, ok := g.outputs[key]
	if !ok {
		return "", fmt.Errorf("unexpected git command: %s", key)
	}
	return out, nil
}

type fakeCommitClient struct {
	oid    string
	oids   []string
	input  CreateCommitInput
	inputs []CreateCommitInput
}

func (c *fakeCommitClient) CreateCommitOnBranch(ctx context.Context, input CreateCommitInput) (string, error) {
	c.input = input
	c.inputs = append(c.inputs, input)
	if len(c.oids) > 0 {
		oid := c.oids[0]
		c.oids = c.oids[1:]
		return oid, nil
	}
	return c.oid, nil
}

func calledBefore(calls []string, first, second string) bool {
	firstIndex := slices.Index(calls, first)
	secondIndex := slices.Index(calls, second)
	return firstIndex >= 0 && secondIndex >= 0 && firstIndex < secondIndex
}
