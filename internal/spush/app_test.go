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
		"git rev-parse HEAD":                                "local-oid\n",
		"git rev-parse origin/main":                         "remote-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git diff --name-status -z origin/main HEAD":        "M\x00README.md\x00",
		"git show HEAD:README.md":                           "hello",
		"git log -1 --format=%B HEAD":                       "update readme\n\nbody\n",
		"git pull --ff-only origin main":                    "",
	})
	client := &fakeCommitClient{oid: "signed-oid"}
	var out strings.Builder

	err := runWithDeps(context.Background(), []string{}, map[string]string{"GH_TOKEN": "token"}, &out, git, client)
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
	if !strings.Contains(out.String(), "signed-oid") {
		t.Fatalf("output = %q, want signed oid", out.String())
	}
}

func TestRunWithDepsSetUpstreamConfiguresTrackingBranch(t *testing.T) {
	git := newFakeGit(map[string]string{
		"git remote get-url origin":                         "https://github.com/ryotarai/git-spush.git\n",
		"git rev-parse topic":                               "local-oid\n",
		"git rev-parse origin/topic":                        "remote-oid\n",
		"git merge-base --is-ancestor remote-oid local-oid": "",
		"git diff --name-status -z origin/topic topic":      "M\x00README.md\x00",
		"git show topic:README.md":                          "hello",
		"git log -1 --format=%B topic":                      "update readme\n",
		"git pull --ff-only origin topic":                   "",
		"git branch --set-upstream-to=origin/topic topic":   "",
	})
	client := &fakeCommitClient{oid: "signed-oid"}

	err := runWithDeps(context.Background(), []string{"-u", "origin", "topic"}, map[string]string{"GH_TOKEN": "token"}, io.Discard, git, client)
	if err != nil {
		t.Fatalf("runWithDeps returned error: %v", err)
	}
	if !slices.Contains(git.calls, "git branch --set-upstream-to=origin/topic topic") {
		t.Fatalf("git calls = %#v, want upstream configuration", git.calls)
	}
}

type fakeGit struct {
	outputs map[string]string
	calls   []string
}

func newFakeGit(outputs map[string]string) *fakeGit {
	return &fakeGit{outputs: outputs}
}

func (g *fakeGit) Run(ctx context.Context, args ...string) (string, error) {
	key := "git " + strings.Join(args, " ")
	g.calls = append(g.calls, key)
	out, ok := g.outputs[key]
	if !ok {
		return "", fmt.Errorf("unexpected git command: %s", key)
	}
	return out, nil
}

type fakeCommitClient struct {
	oid   string
	input CreateCommitInput
}

func (c *fakeCommitClient) CreateCommitOnBranch(ctx context.Context, input CreateCommitInput) (string, error) {
	c.input = input
	return c.oid, nil
}
