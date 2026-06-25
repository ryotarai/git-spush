package spush

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

const defaultGitHubGraphQLEndpoint = "https://api.github.com/graphql"

type PushOptions struct {
	Remote       string
	LocalRef     string
	RemoteBranch string
	SetUpstream  bool
}

type GitHubRemote struct {
	Owner string
	Repo  string
}

func (r GitHubRemote) NameWithOwner() string {
	return r.Owner + "/" + r.Repo
}

type GitRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

type execGit struct{}

func (execGit) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

type FileAddition struct {
	Path     string
	Contents string
}

type FileDeletion struct {
	Path string
}

type FileChanges struct {
	Additions []FileAddition
	Deletions []FileDeletion
}

type CreateCommitInput struct {
	RepositoryNameWithOwner string
	BranchName              string
	ExpectedHeadOID         string
	MessageHeadline         string
	MessageBody             string
	FileChanges             FileChanges
}

type GitHubClient struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

type commitClient interface {
	CreateCommitOnBranch(ctx context.Context, input CreateCommitInput) (string, error)
}

type refClient interface {
	RepositoryID(ctx context.Context, remote GitHubRemote) (string, error)
	CreateRef(ctx context.Context, repositoryID, name, oid string) error
}

func NewGitHubClient(endpoint, token string) *GitHubClient {
	if endpoint == "" {
		endpoint = defaultGitHubGraphQLEndpoint
	}
	return &GitHubClient{endpoint: endpoint, token: token, httpClient: http.DefaultClient}
}

func Main(ctx context.Context, args []string, env []string, stdout, stderr io.Writer) int {
	if err := Run(ctx, args, env, stdout); err != nil {
		fmt.Fprintf(stderr, "git-spush: %v\n", err)
		return 1
	}
	return 0
}

func Run(ctx context.Context, args []string, env []string, stdout io.Writer) error {
	environment := envMap(env)
	endpoint := environment["GITHUB_GRAPHQL_URL"]
	client := NewGitHubClient(endpoint, "")
	return runWithDeps(ctx, args, environment, stdout, execGit{}, client)
}

func runWithDeps(ctx context.Context, args []string, environment map[string]string, stdout io.Writer, git GitRunner, client commitClient) error {
	options, err := ParsePushArgs(args)
	if err != nil {
		return err
	}

	if options.RemoteBranch == "" {
		branch, err := currentBranch(ctx, git)
		if err != nil {
			return err
		}
		options.RemoteBranch = branch
	}

	remoteURL, err := git.Run(ctx, "remote", "get-url", options.Remote)
	if err != nil {
		return err
	}
	remote, err := ParseGitHubRemote(strings.TrimSpace(remoteURL))
	if err != nil {
		return err
	}

	if err := ensureCleanWorktree(ctx, git); err != nil {
		return err
	}

	remoteRef := options.Remote + "/" + options.RemoteBranch
	remoteBranchExists, err := fetchRemoteBranch(ctx, git, options.Remote, options.RemoteBranch)
	if err != nil {
		return err
	}
	if !remoteBranchExists {
		return createNewRemoteBranch(ctx, git, client, remote, options, remoteRef, environment, stdout)
	}

	localOID, err := trimmedGit(ctx, git, "rev-parse", options.LocalRef)
	if err != nil {
		return err
	}
	remoteOID, err := trimmedGit(ctx, git, "rev-parse", remoteRef)
	if err != nil {
		return fmt.Errorf("resolve remote ref %q: %w", remoteRef, err)
	}
	if localOID == remoteOID {
		fmt.Fprintln(stdout, "Everything up-to-date")
		return nil
	}
	if _, err := git.Run(ctx, "merge-base", "--is-ancestor", remoteOID, localOID); err != nil {
		return fmt.Errorf("remote %s is not an ancestor of %s; non-fast-forward pushes are not supported", remoteRef, options.LocalRef)
	}

	commits, err := localCommits(ctx, git, remoteRef, options.LocalRef)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		fmt.Fprintln(stdout, "Everything up-to-date")
		return nil
	}

	token, err := githubToken(ctx, git, environment)
	if err != nil {
		return err
	}
	if githubClient, ok := client.(*GitHubClient); ok {
		githubClient.token = token
	}

	expectedHeadOID := remoteOID
	baseRef := remoteRef
	createdOIDs := make([]string, 0, len(commits))
	for _, commit := range commits {
		changes, err := BuildFileChanges(ctx, git, baseRef, commit)
		if err != nil {
			return err
		}
		if len(changes.Additions) == 0 && len(changes.Deletions) == 0 {
			baseRef = commit
			continue
		}
		headline, body, err := commitMessage(ctx, git, commit)
		if err != nil {
			return err
		}
		oid, err := client.CreateCommitOnBranch(ctx, CreateCommitInput{
			RepositoryNameWithOwner: remote.NameWithOwner(),
			BranchName:              options.RemoteBranch,
			ExpectedHeadOID:         expectedHeadOID,
			MessageHeadline:         headline,
			MessageBody:             body,
			FileChanges:             changes,
		})
		if err != nil {
			return err
		}
		createdOIDs = append(createdOIDs, oid)
		expectedHeadOID = oid
		baseRef = commit
	}
	if len(createdOIDs) == 0 {
		fmt.Fprintln(stdout, "Everything up-to-date")
		return nil
	}
	if err := verifyCreatedRemoteBeforeReset(ctx, git, options.Remote, options.RemoteBranch, remoteRef, options.LocalRef, expectedHeadOID); err != nil {
		return err
	}

	if _, err := git.Run(ctx, "reset", "--hard", remoteOID); err != nil {
		return fmt.Errorf("created %d commits, but resetting local branch before pull failed: %w", len(createdOIDs), err)
	}
	pullArgs := []string{"pull", "--ff-only", options.Remote, options.RemoteBranch}
	if _, err := git.Run(ctx, pullArgs...); err != nil {
		return fmt.Errorf("created %d commits, but git pull failed: %w", len(createdOIDs), err)
	}
	if options.SetUpstream {
		upstream := options.Remote + "/" + options.RemoteBranch
		if _, err := git.Run(ctx, "branch", "--set-upstream-to="+upstream, options.LocalRef); err != nil {
			return fmt.Errorf("created %d commits and pulled them, but setting upstream failed: %w", len(createdOIDs), err)
		}
	}
	if len(createdOIDs) == 1 {
		fmt.Fprintf(stdout, "Created GitHub-signed commit %s and updated local branch\n", createdOIDs[0])
		return nil
	}
	fmt.Fprintf(stdout, "Created %d GitHub-signed commits and updated local branch\n", len(createdOIDs))
	return nil
}

func ParsePushArgs(args []string) (PushOptions, error) {
	options := PushOptions{Remote: "origin", LocalRef: "HEAD"}
	positionals := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "-u", "--set-upstream":
			options.SetUpstream = true
		case "--force", "-f", "--force-with-lease":
			return PushOptions{}, fmt.Errorf("%s is not supported because createCommitOnBranch only supports fast-forward branch updates", arg)
		default:
			if strings.HasPrefix(arg, "-") {
				return PushOptions{}, fmt.Errorf("unsupported option %s", arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) > 2 {
		return PushOptions{}, errors.New("too many arguments; usage: git spush [remote] [src[:dst]]")
	}
	if len(positionals) >= 1 {
		options.Remote = positionals[0]
	}
	if len(positionals) == 2 {
		src, dst, ok := strings.Cut(positionals[1], ":")
		if src == "" {
			src = "HEAD"
		}
		options.LocalRef = src
		if ok {
			options.RemoteBranch = branchName(dst)
		} else {
			options.RemoteBranch = branchName(src)
		}
	}
	return options, nil
}

func ParseGitHubRemote(raw string) (GitHubRemote, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		return parseOwnerRepoPath(path)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return GitHubRemote{}, fmt.Errorf("parse remote URL %q: %w", raw, err)
	}
	if u.Host != "github.com" {
		return GitHubRemote{}, fmt.Errorf("remote URL host %q is not github.com", u.Host)
	}
	return parseOwnerRepoPath(strings.TrimPrefix(u.Path, "/"))
}

func BuildFileChanges(ctx context.Context, git GitRunner, baseRef, headRef string) (FileChanges, error) {
	out, err := git.Run(ctx, "diff", "--name-status", "-z", baseRef, headRef)
	if err != nil {
		return FileChanges{}, err
	}
	parts := strings.Split(out, "\x00")
	changes := FileChanges{}
	for i := 0; i < len(parts) && parts[i] != ""; {
		status := parts[i]
		i++
		switch status[0] {
		case 'A', 'M', 'T':
			if i >= len(parts) {
				return FileChanges{}, errors.New("malformed git diff output")
			}
			path := parts[i]
			i++
			content, err := git.Run(ctx, "show", headRef+":"+path)
			if err != nil {
				return FileChanges{}, err
			}
			changes.Additions = append(changes.Additions, FileAddition{Path: path, Contents: content})
		case 'D':
			if i >= len(parts) {
				return FileChanges{}, errors.New("malformed git diff output")
			}
			changes.Deletions = append(changes.Deletions, FileDeletion{Path: parts[i]})
			i++
		case 'R', 'C':
			if i+1 >= len(parts) {
				return FileChanges{}, errors.New("malformed git diff output")
			}
			oldPath := parts[i]
			newPath := parts[i+1]
			i += 2
			if status[0] == 'R' {
				changes.Deletions = append(changes.Deletions, FileDeletion{Path: oldPath})
			}
			content, err := git.Run(ctx, "show", headRef+":"+newPath)
			if err != nil {
				return FileChanges{}, err
			}
			changes.Additions = append(changes.Additions, FileAddition{Path: newPath, Contents: content})
		default:
			return FileChanges{}, fmt.Errorf("unsupported git diff status %q", status)
		}
	}
	return changes, nil
}

func (c *GitHubClient) CreateCommitOnBranch(ctx context.Context, input CreateCommitInput) (string, error) {
	if c.token == "" {
		return "", errors.New("GitHub token is empty")
	}
	variables := map[string]any{
		"input": map[string]any{
			"branch": map[string]any{
				"repositoryNameWithOwner": input.RepositoryNameWithOwner,
				"branchName":              input.BranchName,
			},
			"expectedHeadOid": input.ExpectedHeadOID,
			"message": map[string]any{
				"headline": input.MessageHeadline,
				"body":     input.MessageBody,
			},
			"fileChanges": graphQLFileChanges(input.FileChanges),
		},
	}
	body, err := json.Marshal(map[string]any{"query": createCommitOnBranchMutation, "variables": variables})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub GraphQL HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var decoded struct {
		Data struct {
			CreateCommitOnBranch struct {
				Commit struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Errors) > 0 {
		return "", fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.CreateCommitOnBranch.Commit.OID == "" {
		return "", errors.New("GitHub GraphQL response did not include commit oid")
	}
	return decoded.Data.CreateCommitOnBranch.Commit.OID, nil
}

func (c *GitHubClient) RepositoryID(ctx context.Context, remote GitHubRemote) (string, error) {
	variables := map[string]any{"owner": remote.Owner, "name": remote.Repo}
	body, err := json.Marshal(map[string]any{"query": repositoryIDQuery, "variables": variables})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub GraphQL HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var decoded struct {
		Data struct {
			Repository struct {
				ID string `json:"id"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Errors) > 0 {
		return "", fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.Repository.ID == "" {
		return "", errors.New("GitHub GraphQL response did not include repository id")
	}
	return decoded.Data.Repository.ID, nil
}

func (c *GitHubClient) CreateRef(ctx context.Context, repositoryID, name, oid string) error {
	variables := map[string]any{
		"input": map[string]any{
			"repositoryId": repositoryID,
			"name":         name,
			"oid":          oid,
		},
	}
	body, err := json.Marshal(map[string]any{"query": createRefMutation, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub GraphQL HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var decoded struct {
		Data struct {
			CreateRef struct {
				Ref struct {
					ID string `json:"id"`
				} `json:"ref"`
			} `json:"createRef"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return err
	}
	if len(decoded.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.CreateRef.Ref.ID == "" {
		return errors.New("GitHub GraphQL response did not include ref id")
	}
	return nil
}

const createCommitOnBranchMutation = `
mutation CreateCommitOnBranch($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) {
    commit {
      oid
    }
  }
}`

const repositoryIDQuery = `
query RepositoryID($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
  }
}`

const createRefMutation = `
mutation CreateRef($input: CreateRefInput!) {
  createRef(input: $input) {
    ref {
      id
    }
  }
}`

func parseOwnerRepoPath(path string) (GitHubRemote, error) {
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return GitHubRemote{}, fmt.Errorf("remote URL does not look like owner/repo: %q", path)
	}
	return GitHubRemote{Owner: parts[0], Repo: parts[1]}, nil
}

func graphQLFileChanges(changes FileChanges) map[string]any {
	out := map[string]any{}
	if len(changes.Additions) > 0 {
		additions := make([]map[string]any, 0, len(changes.Additions))
		for _, addition := range changes.Additions {
			additions = append(additions, map[string]any{
				"path":     addition.Path,
				"contents": base64.StdEncoding.EncodeToString([]byte(addition.Contents)),
			})
		}
		out["additions"] = additions
	}
	if len(changes.Deletions) > 0 {
		deletions := make([]map[string]any, 0, len(changes.Deletions))
		for _, deletion := range changes.Deletions {
			deletions = append(deletions, map[string]any{"path": deletion.Path})
		}
		out["deletions"] = deletions
	}
	return out
}

func currentBranch(ctx context.Context, git GitRunner) (string, error) {
	return trimmedGit(ctx, git, "branch", "--show-current")
}

func commitMessage(ctx context.Context, git GitRunner, ref string) (string, string, error) {
	message, err := trimmedGit(ctx, git, "log", "-1", "--format=%B", ref)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(message, "\n")
	headline := strings.TrimSpace(lines[0])
	if headline == "" {
		headline = "Apply local changes"
	}
	body := ""
	if len(lines) > 1 {
		body = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return headline, body, nil
}

func localCommits(ctx context.Context, git GitRunner, baseRef, headRef string) ([]string, error) {
	out, err := git.Run(ctx, "rev-list", "--reverse", baseRef+".."+headRef)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

func newRemoteBranchCommits(ctx context.Context, git GitRunner, remote, headRef string) ([]string, error) {
	out, err := git.Run(ctx, "rev-list", "--reverse", headRef, "--not", "--remotes="+remote)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

func githubToken(ctx context.Context, git GitRunner, env map[string]string) (string, error) {
	if token := env["GITHUB_TOKEN"]; token != "" {
		return token, nil
	}
	token, err := trimmedGit(ctx, git, "config", "--get", "github.token")
	if err == nil && token != "" {
		return token, nil
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out)), nil
	}
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		return "", fmt.Errorf("set GITHUB_TOKEN or git config github.token, or authenticate gh: %s", strings.TrimSpace(stderr.String()))
	}
	return "", errors.New("set GITHUB_TOKEN or git config github.token, or authenticate gh")
}

func ensureCleanWorktree(ctx context.Context, git GitRunner) error {
	if _, err := git.Run(ctx, "diff", "--quiet"); err != nil {
		return errors.New("working tree has uncommitted changes; commit or stash them before git-spush")
	}
	if _, err := git.Run(ctx, "diff", "--cached", "--quiet"); err != nil {
		return errors.New("index has uncommitted changes; commit or stash them before git-spush")
	}
	return nil
}

func verifyCreatedRemoteBeforeReset(ctx context.Context, git GitRunner, remote, remoteBranch, remoteRef, localRef, expectedHeadOID string) error {
	if _, err := git.Run(ctx, "fetch", remote, remoteBranch); err != nil {
		return fmt.Errorf("created remote commits, but verifying remote before reset failed: %w", err)
	}
	actualHeadOID, err := trimmedGit(ctx, git, "rev-parse", remoteRef)
	if err != nil {
		return fmt.Errorf("created remote commits, but resolving %s before reset failed: %w", remoteRef, err)
	}
	if actualHeadOID != expectedHeadOID {
		return fmt.Errorf("created remote commits, but remote head verification failed before reset: %s is %s, want %s", remoteRef, actualHeadOID, expectedHeadOID)
	}
	if _, err := git.Run(ctx, "diff", "--quiet", localRef, remoteRef); err != nil {
		return fmt.Errorf("created remote commits, but remote tree verification failed before reset: %s differs from %s", remoteRef, localRef)
	}
	return nil
}

func fetchRemoteBranch(ctx context.Context, git GitRunner, remote, remoteBranch string) (bool, error) {
	if _, err := git.Run(ctx, "fetch", remote, remoteBranch); err != nil {
		if isMissingRemoteRefError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isMissingRemoteRefError(err error) bool {
	return strings.Contains(err.Error(), "couldn't find remote ref")
}

func createNewRemoteBranch(ctx context.Context, git GitRunner, client commitClient, remote GitHubRemote, options PushOptions, remoteRef string, environment map[string]string, stdout io.Writer) error {
	refCreator, ok := client.(refClient)
	if !ok {
		return errors.New("GitHub client does not support creating refs")
	}
	if _, err := git.Run(ctx, "fetch", options.Remote); err != nil {
		return err
	}
	localOID, err := trimmedGit(ctx, git, "rev-parse", options.LocalRef)
	if err != nil {
		return err
	}
	commits, err := newRemoteBranchCommits(ctx, git, options.Remote, options.LocalRef)
	if err != nil {
		return err
	}

	token, err := githubToken(ctx, git, environment)
	if err != nil {
		return err
	}
	if githubClient, ok := client.(*GitHubClient); ok {
		githubClient.token = token
	}
	repositoryID, err := refCreator.RepositoryID(ctx, remote)
	if err != nil {
		return err
	}

	if len(commits) == 0 {
		if err := refCreator.CreateRef(ctx, repositoryID, "refs/heads/"+options.RemoteBranch, localOID); err != nil {
			return err
		}
		if err := verifyCreatedRemoteBeforeReset(ctx, git, options.Remote, options.RemoteBranch, remoteRef, options.LocalRef, localOID); err != nil {
			return err
		}
		if options.SetUpstream {
			upstream := options.Remote + "/" + options.RemoteBranch
			if _, err := git.Run(ctx, "branch", "--set-upstream-to="+upstream, options.LocalRef); err != nil {
				return fmt.Errorf("created remote branch, but setting upstream failed: %w", err)
			}
		}
		fmt.Fprintf(stdout, "Created remote branch %s\n", remoteRef)
		return nil
	}

	baseOID, err := trimmedGit(ctx, git, "rev-parse", commits[0]+"^")
	if err != nil {
		return fmt.Errorf("resolve new branch base for %s: %w", commits[0], err)
	}
	if err := refCreator.CreateRef(ctx, repositoryID, "refs/heads/"+options.RemoteBranch, baseOID); err != nil {
		return err
	}

	expectedHeadOID := baseOID
	baseRef := baseOID
	createdOIDs := make([]string, 0, len(commits))
	for _, commit := range commits {
		changes, err := BuildFileChanges(ctx, git, baseRef, commit)
		if err != nil {
			return err
		}
		if len(changes.Additions) == 0 && len(changes.Deletions) == 0 {
			baseRef = commit
			continue
		}
		headline, body, err := commitMessage(ctx, git, commit)
		if err != nil {
			return err
		}
		oid, err := client.CreateCommitOnBranch(ctx, CreateCommitInput{
			RepositoryNameWithOwner: remote.NameWithOwner(),
			BranchName:              options.RemoteBranch,
			ExpectedHeadOID:         expectedHeadOID,
			MessageHeadline:         headline,
			MessageBody:             body,
			FileChanges:             changes,
		})
		if err != nil {
			return err
		}
		createdOIDs = append(createdOIDs, oid)
		expectedHeadOID = oid
		baseRef = commit
	}
	if len(createdOIDs) == 0 {
		fmt.Fprintln(stdout, "Everything up-to-date")
		return nil
	}
	if err := verifyCreatedRemoteBeforeReset(ctx, git, options.Remote, options.RemoteBranch, remoteRef, options.LocalRef, expectedHeadOID); err != nil {
		return err
	}
	if _, err := git.Run(ctx, "reset", "--hard", baseOID); err != nil {
		return fmt.Errorf("created %d commits, but resetting local branch before pull failed: %w", len(createdOIDs), err)
	}
	pullArgs := []string{"pull", "--ff-only", options.Remote, options.RemoteBranch}
	if _, err := git.Run(ctx, pullArgs...); err != nil {
		return fmt.Errorf("created %d commits, but git pull failed: %w", len(createdOIDs), err)
	}
	if options.SetUpstream {
		upstream := options.Remote + "/" + options.RemoteBranch
		if _, err := git.Run(ctx, "branch", "--set-upstream-to="+upstream, options.LocalRef); err != nil {
			return fmt.Errorf("created %d commits and pulled them, but setting upstream failed: %w", len(createdOIDs), err)
		}
	}
	if len(createdOIDs) == 1 {
		fmt.Fprintf(stdout, "Created GitHub-signed commit %s and updated local branch\n", createdOIDs[0])
		return nil
	}
	fmt.Fprintf(stdout, "Created %d GitHub-signed commits and updated local branch\n", len(createdOIDs))
	return nil
}

func trimmedGit(ctx context.Context, git GitRunner, args ...string) (string, error) {
	out, err := git.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func branchName(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return ref
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return map[string]string{
			"GITHUB_TOKEN":       os.Getenv("GITHUB_TOKEN"),
			"GITHUB_GRAPHQL_URL": os.Getenv("GITHUB_GRAPHQL_URL"),
		}
	}
	return out
}
