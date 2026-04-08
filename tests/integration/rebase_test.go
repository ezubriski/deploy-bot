//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v60/github"

	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
)

// TestRebaseDeployBranchAgainstRealGitHub validates the GraphQL rebase
// mutation (updateRef force + createCommitOnBranch chained in one request)
// against the live GitHub API.
//
// Flow:
//  1. Pick two distinct ECR tags A and B that differ from the currently
//     deployed tag.
//  2. Create a deploy PR for tag A via the github client directly (bypasses
//     the bot worker — we're exercising the github package, not the bot
//     glue). This produces a deploy branch one commit ahead of base.
//  3. Independently push a new commit to base that changes the same
//     kustomization file to tag B. The deploy branch is now both behind
//     base AND has a conflicting modification on the same line.
//  4. Call RebaseDeployBranch with the original tag A. The expected
//     post-state is: deploy branch is fast-forwarded to the new base SHA
//     and has exactly one commit on top that re-applies tag A.
//  5. Inspect the deploy branch HEAD: its parent must equal the new base
//     SHA, and the kustomization file at HEAD must contain newTag: A.
//  6. Cleanup the PR and branches.
func TestRebaseDeployBranchAgainstRealGitHub(t *testing.T) {
	ctx := context.Background()
	resetAppState(t)

	tagA := pickTagFor(t, env.app)
	tagB := pickTagFor(t, env.app, tagA)
	if tagA == tagB {
		t.Fatalf("need two distinct tags from the pool; got %s twice", tagA)
	}

	appCfg, ok := env.cfg.AppByName(env.app)
	if !ok {
		t.Fatalf("app %q not found in config", env.app)
	}

	params := githubpkg.CreatePRParams{
		App:           appCfg.App,
		Environment:   appCfg.Environment,
		Tag:           tagA,
		KustomizePath: appCfg.KustomizePath,
		BaseBranch:    env.defaultBranch,
		Requester:     "rebase integration test",
		Reason:        "rebase integration test",
		Labels:        nil,
	}

	prNumber, _, err := env.ghClient.CreateDeployPR(ctx, params)
	if err != nil {
		t.Fatalf("CreateDeployPR(tagA=%s): %v", tagA, err)
	}
	branch := deployBranch(appCfg.Environment, appCfg.App, tagA)
	t.Cleanup(func() { cleanupPRWithTag(t, prNumber, tagA) })
	t.Logf("created deploy PR #%d on branch %s with tag %s", prNumber, branch, tagA)

	// Step 3: directly push a conflicting commit to base. We use the raw
	// REST client here so the test owns this side effect outright instead of
	// going through the bot's deploy machinery.
	baseFileBefore, _, _, err := env.ghRaw.Repositories.GetContents(ctx, env.cfg.GitHub.Org, env.cfg.GitHub.Repo, appCfg.KustomizePath, &gh.RepositoryContentGetOptions{
		Ref: env.defaultBranch,
	})
	if err != nil {
		t.Fatalf("get base file: %v", err)
	}
	baseText, err := baseFileBefore.GetContent()
	if err != nil {
		t.Fatalf("decode base file: %v", err)
	}
	advancedText := replaceNewTag(t, baseText, tagB)
	if advancedText == baseText {
		t.Fatalf("rewriting base file produced no change; baseText=%q tagB=%q", baseText, tagB)
	}
	commitMsg := "test: advance " + appCfg.KustomizePath + " to " + tagB
	if _, _, err := env.ghRaw.Repositories.UpdateFile(ctx, env.cfg.GitHub.Org, env.cfg.GitHub.Repo, appCfg.KustomizePath, &gh.RepositoryContentFileOptions{
		Message: gh.String(commitMsg),
		Content: []byte(advancedText),
		SHA:     gh.String(baseFileBefore.GetSHA()),
		Branch:  gh.String(env.defaultBranch),
	}); err != nil {
		t.Fatalf("advance base branch: %v", err)
	}

	// Capture the new base SHA so we can assert the deploy branch parent
	// matches it after the rebase.
	baseRef, _, err := env.ghRaw.Git.GetRef(ctx, env.cfg.GitHub.Org, env.cfg.GitHub.Repo, "refs/heads/"+env.defaultBranch)
	if err != nil {
		t.Fatalf("get base ref after advance: %v", err)
	}
	newBaseSHA := baseRef.Object.GetSHA()
	t.Logf("base branch advanced to %s with tag %s", newBaseSHA, tagB)

	// Step 4: rebase the deploy branch via GraphQL.
	if err := env.ghClient.RebaseDeployBranch(ctx, params); err != nil {
		t.Fatalf("RebaseDeployBranch: %v", err)
	}

	// Step 5: verify the deploy branch is fast-forwarded with one extra
	// commit and the kustomization file at HEAD has tag A.
	deployRef, _, err := env.ghRaw.Git.GetRef(ctx, env.cfg.GitHub.Org, env.cfg.GitHub.Repo, "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("get deploy ref after rebase: %v", err)
	}
	deployHead := deployRef.Object.GetSHA()
	deployCommit, _, err := env.ghRaw.Git.GetCommit(ctx, env.cfg.GitHub.Org, env.cfg.GitHub.Repo, deployHead)
	if err != nil {
		t.Fatalf("get deploy head commit: %v", err)
	}
	if len(deployCommit.Parents) != 1 {
		t.Fatalf("deploy head has %d parents, want 1", len(deployCommit.Parents))
	}
	if got := deployCommit.Parents[0].GetSHA(); got != newBaseSHA {
		t.Errorf("deploy head parent = %s, want new base SHA %s — branch was not rebased onto current base", got, newBaseSHA)
	}

	deployFile, err := env.ghClient.GetFileContent(ctx, appCfg.KustomizePath, branch)
	if err != nil {
		t.Fatalf("get deploy file: %v", err)
	}
	if !strings.Contains(deployFile, "newTag: "+tagA) {
		t.Errorf("deploy file at HEAD missing newTag: %s\ngot: %q", tagA, deployFile)
	}
	if strings.Contains(deployFile, "newTag: "+tagB) {
		t.Errorf("deploy file at HEAD still has tag B (%s) from base — rebase did not re-apply tag A: %q", tagB, deployFile)
	}
}

// replaceNewTag rewrites the newTag value in a kustomization.yaml string.
// Mirrors the regex used by internal/github/pr.go but kept inline so the
// test does not depend on an unexported helper.
func replaceNewTag(t *testing.T, content, newTag string) string {
	t.Helper()
	idx := strings.Index(content, "newTag:")
	if idx < 0 {
		t.Fatalf("replaceNewTag: newTag not found in content: %q", content)
	}
	end := idx + len("newTag:")
	// Skip whitespace then take the value up to whitespace/newline.
	rest := content[end:]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	valStart := end + i
	valEnd := valStart
	for valEnd < len(content) && content[valEnd] != '\n' && content[valEnd] != ' ' && content[valEnd] != '\t' {
		valEnd++
	}
	return content[:valStart] + newTag + content[valEnd:]
}
