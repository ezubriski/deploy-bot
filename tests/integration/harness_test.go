//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecr"
	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

// tagPool is populated in TestMain from the live ECR cache. It holds the
// most recent tags available for the test application. Tests call pickTagFor
// to get a tag that is not currently deployed, so deploys always produce real
// changes regardless of prior run state.
var tagPool []string

// env holds all shared state for the integration test suite.
var env *testEnv

type testEnv struct {
	ctx               context.Context
	cancel            context.CancelFunc
	store             *store.Store
	ghClient          *githubpkg.Client
	bot               *bot.Bot
	requesterID       string
	requesterUsername string
	approverID        string
	app               string
	environment       string
	// tag is any valid ECR tag used by tests that only need ECR validation
	// (e.g. TestValidateTag_CacheMiss). Deployment tests call pickTagFor instead.
	tag           string
	defaultBranch string
	deployChannel string
	cfg           *config.Config
	metrics       *metrics.Metrics
	log           *zap.Logger
}

func TestMain(m *testing.M) {
	requesterID := requireEnv("INTEGRATION_REQUESTER_ID")
	requesterUsername := requireEnv("INTEGRATION_REQUESTER_USERNAME")
	approverID := requireEnv("INTEGRATION_APPROVER_ID")
	app := requireEnv("INTEGRATION_APP")

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "testdata/config.json"
	}

	log, _ := zap.NewDevelopment()

	cfg, err := config.Load(configPath)
	if err != nil {
		fatalf("load config: %v", err)
	}
	cfgHolder := config.NewHolder(cfg, configPath)

	ctx, cancel := context.WithCancel(context.Background())

	secrets, err := config.LoadSecrets(ctx, requireEnv("AWS_SECRET_NAME"))
	if err != nil {
		fatalf("load secrets: %v", err)
	}
	if err := secrets.Validate(); err != nil {
		fatalf("invalid secrets: %v", err)
	}

	redisStore := store.New(secrets.RedisAddr, secrets.RedisToken)
	if err := redisStore.Ping(ctx); err != nil {
		fatalf("redis ping: %v", err)
	}

	ghHTTP, err := secrets.GitHubHTTPClient(cfg.GitHub.Repo)
	if err != nil {
		fatalf("github client: %v", err)
	}
	maxRetries, retryWait := cfg.GitHub.RateLimitConfig()
	ghClient := githubpkg.NewClient(ghHTTP, cfg.GitHub.Org, cfg.GitHub.Repo, log, githubpkg.RetryConfig{MaxRetries: maxRetries, RetryWait: retryWait})

	defaultBranch, err := ghClient.GetDefaultBranch(ctx)
	if err != nil {
		fatalf("get default branch: %v", err)
	}
	rawSlack := slack.New(secrets.SlackBotToken, slack.OptionAppLevelToken(secrets.SlackAppToken))
	slackMaxRetries, slackRetryWait := cfg.Slack.RateLimitConfig()
	slackClient := slackclient.New(rawSlack, slackMaxRetries, slackRetryWait, log)

	m2 := metrics.NewDefault()

	ecrCache, err := ecr.NewCache(ctx, cfg, m2, log)
	if err != nil {
		fatalf("init ecr cache: %v", err)
	}
	ecrCache.Populate(ctx)

	// Build tag pool from real ECR tags so pickTagFor never attempts a tag that
	// doesn't exist. Pull enough entries that the pool survives multiple
	// successive deploys within a single test run.
	tagPool = ecrCache.Tags(app, 10)
	if len(tagPool) < 2 {
		fatalf("ECR cache for app %q returned fewer than 2 tags; cannot run integration tests", app)
	}

	auditLog, err := audit.NewLogger(ctx, cfg, log)
	if err != nil {
		fatalf("init audit logger: %v", err)
	}

	valHTTP, valErr := secrets.ValidatorHTTPClient()
	if valErr != nil {
		fatalf("validator github client: %v", valErr)
	}
	val := validator.New(valHTTP, rawSlack, redisStore.Redis(), cfg, log)
	b := bot.New(slackClient, redisStore, ghClient, ecrCache, val, auditLog, m2, cfgHolder, log)

	// Delete any leftover stream from a previous test run. This clears the
	// consumer group and PEL so ghost events from failed runs are not replayed.
	if err := redisStore.Redis().Del(ctx, queue.StreamKey).Err(); err != nil {
		fatalf("flush event stream: %v", err)
	}

	qw := queue.NewWorker(redisStore.Redis(), log)
	if err := qw.Init(ctx); err != nil {
		fatalf("init queue consumer group: %v", err)
	}
	go qw.Run(ctx, b.HandleEvent)

	appCfg, ok := cfg.AppByName(app)
	if !ok {
		fatalf("app %q not found in config", app)
	}

	env = &testEnv{
		ctx:               ctx,
		cancel:            cancel,
		store:             redisStore,
		ghClient:          ghClient,
		bot:               b,
		requesterID:       requesterID,
		requesterUsername: requesterUsername,
		approverID:        approverID,
		app:               app,
		environment:       appCfg.Environment,
		tag:               tagPool[len(tagPool)-1], // any valid ECR tag; deploy tests use pickTagFor
		defaultBranch:     defaultBranch,
		deployChannel:     cfg.Slack.DeployChannel,
		cfg:               cfg,
		metrics:           m2,
		log:               log,
	}

	code := m.Run()

	cancel()
	os.Exit(code)
}

// requireEnv returns the value of an environment variable or exits if unset.
func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("required environment variable %s is not set", key)
	}
	return v
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// pickTagFor returns a tag from tagPool that is (a) not in the exclude list
// and (b) not the tag currently deployed for app according to the live
// kustomization file on GitHub. Call this at the start of any test that
// deploys, so the deploy is guaranteed to produce a real change.
//
// If the GitHub read fails, the first non-excluded pool member is returned.
// If all pool members are excluded the test fails immediately.
func pickTagFor(t *testing.T, app string, exclude ...string) string {
	t.Helper()
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	appCfg, ok := env.cfg.AppByName(app)
	if !ok {
		t.Fatalf("pickTagFor: app %q not found in config", app)
	}
	content, _ := env.ghClient.GetFileContent(env.ctx, appCfg.KustomizePath, env.defaultBranch)
	for _, tag := range tagPool {
		if !excluded[tag] && !strings.Contains(content, "newTag: "+tag) {
			return tag
		}
	}
	// All non-excluded tags happen to be current (pool exhausted) — fall back
	// to the first non-excluded entry and let GitHub reject the no-op naturally.
	for _, tag := range tagPool {
		if !excluded[tag] {
			t.Logf("pickTagFor: all pool tags appear current for %s; using %s as best-effort", app, tag)
			return tag
		}
	}
	t.Fatalf("pickTagFor: all %d pool tags are excluded for app %q", len(tagPool), app)
	return ""
}

// resetAppState removes any leftover Redis state for the test app so each test
// starts clean. Does not flush the entire database.
func resetAppState(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_ = env.store.ReleaseLock(ctx, env.environment, env.app)
	deploys, err := env.store.GetAll(ctx)
	if err != nil {
		t.Fatalf("resetAppState: get all deploys: %v", err)
	}
	for _, d := range deploys {
		if d.App == env.app {
			_ = env.store.Delete(ctx, d.PRNumber)
		}
	}
}

// poll calls condition every 500ms until it returns true or timeout elapses.
func poll(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// waitForPR polls until a pending deploy for env.app and env.tag appears in
// Redis and returns its PR number.
func waitForPR(t *testing.T) int {
	t.Helper()
	return waitForPRWithTag(t, env.tag)
}

// waitForPRWithTag polls until a pending deploy for env.app and the given tag
// appears in Redis and returns its PR number.
func waitForPRWithTag(t *testing.T, tag string) int {
	t.Helper()
	var prNumber int
	if !poll(t, 60*time.Second, func() bool {
		deploys, _ := env.store.GetAll(context.Background())
		for _, d := range deploys {
			if d.App == env.app && d.Tag == tag {
				prNumber = d.PRNumber
				return true
			}
		}
		return false
	}) {
		t.Fatalf("timed out waiting for deploy PR (tag %s) to be created in Redis", tag)
	}
	return prNumber
}

// cleanupPR closes a GitHub PR (if it is still open) and deletes its branch.
// Errors are logged but do not fail the test — cleanup is best-effort.
func cleanupPR(t *testing.T, prNumber int) {
	t.Helper()
	cleanupPRWithTag(t, prNumber, env.tag)
}

// cleanupPRWithTag is like cleanupPR but uses the given tag to derive the
// branch name. Use this when the PR was created for a tag other than env.tag.
func cleanupPRWithTag(t *testing.T, prNumber int, tag string) {
	t.Helper()
	cleanupPRForApp(t, prNumber, env.app, tag)
}

// cleanupPRForApp closes a PR and deletes its branch for a specific app.
// Use this when the PR was created for an app other than env.app.
func cleanupPRForApp(t *testing.T, prNumber int, app, tag string) {
	t.Helper()
	if prNumber == 0 {
		return
	}
	ctx := context.Background()
	if err := env.ghClient.ClosePR(ctx, prNumber); err != nil {
		t.Logf("cleanup: close PR #%d: %v (may already be closed)", prNumber, err)
	}
	branch := deployBranch(env.environment, app, tag)
	if err := env.ghClient.DeleteBranch(ctx, branch); err != nil {
		t.Logf("cleanup: delete branch %s: %v (may already be deleted)", branch, err)
	}
}

// deployBranch reconstructs the branch name the bot creates for a deploy.
// Must stay in sync with sanitizeBranchName in internal/github/pr.go.
func deployBranch(env, app, tag string) string {
	r := strings.NewReplacer("/", "-", ":", "-", "+", "-", " ", "-")
	return "deploy/" + env + "-" + app + "-" + r.Replace(tag)
}

// startExtraWorker creates a second queue worker with a distinct consumer name
// and runs it for the lifetime of the test. Use this to simulate multi-worker
// deployments. The worker uses the same bot instance as the primary worker, so
// it processes events identically.
func startExtraWorker(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	t.Cleanup(cancel)
	w := queue.NewWorkerWithName(env.store.Redis(), name, env.log)
	// Init is idempotent — the consumer group already exists.
	if err := w.Init(ctx); err != nil {
		t.Fatalf("startExtraWorker %s: init: %v", name, err)
	}
	go w.Run(ctx, env.bot.HandleEvent)
}

// injectDeployRequest enqueues a deploy modal submission directly to Redis,
// bypassing the receiver and Slack Socket Mode.
func injectDeployRequest(t *testing.T, reason string) {
	t.Helper()
	injectDeployRequestWithTag(t, env.tag, reason)
}

// injectDeployRequestWithTag is like injectDeployRequest but uses an explicit tag.
func injectDeployRequestWithTag(t *testing.T, tag, reason string) {
	t.Helper()
	evt := socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type: slack.InteractionTypeViewSubmission,
			View: slack.View{
				CallbackID: bot.ModalCallbackDeploy,
				State: &slack.ViewState{
					Values: map[string]map[string]slack.BlockAction{
						bot.BlockApp: {
							bot.ActionApp: {SelectedOption: slack.OptionBlockObject{Value: env.app}},
						},
						bot.BlockTag: {
							bot.ActionTag: {SelectedOption: slack.OptionBlockObject{}},
						},
						bot.BlockTagManual: {
							bot.ActionTagManual: {Value: tag},
						},
						bot.BlockReason: {
							bot.ActionReason: {Value: reason},
						},
						bot.BlockApprover: {
							bot.ActionApprover: {SelectedUser: env.approverID},
						},
					},
				},
			},
			User: slack.User{ID: env.requesterID},
		},
	}
	if err := queue.Enqueue(context.Background(), env.store.Redis(), evt); err != nil {
		t.Fatalf("inject deploy request: %v", err)
	}
}

// injectSlashCommand enqueues a /deploy slash command event directly to Redis.
func injectSlashCommand(t *testing.T, text string) {
	t.Helper()
	evt := socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{
			Command:   "/deploy",
			Text:      text,
			UserID:    env.requesterID,
			UserName:  env.requesterUsername,
			ChannelID: env.deployChannel,
		},
	}
	if err := queue.Enqueue(context.Background(), env.store.Redis(), evt); err != nil {
		t.Fatalf("inject slash command: %v", err)
	}
}

// injectApprove enqueues an approve button action directly to Redis.
func injectApprove(t *testing.T, prNumber int) {
	t.Helper()
	evt := socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type: slack.InteractionTypeBlockActions,
			ActionCallback: slack.ActionCallbacks{
				BlockActions: []*slack.BlockAction{
					{ActionID: bot.ActionApprove, Value: fmt.Sprintf("%d", prNumber)},
				},
			},
			User: slack.User{ID: env.approverID},
		},
	}
	if err := queue.Enqueue(context.Background(), env.store.Redis(), evt); err != nil {
		t.Fatalf("inject approve: %v", err)
	}
}

// injectRejectSubmit enqueues a reject modal submission directly to Redis,
// skipping the button click that would open the modal.
func injectRejectSubmit(t *testing.T, prNumber int, reason string) {
	t.Helper()
	evt := socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type: slack.InteractionTypeViewSubmission,
			View: slack.View{
				CallbackID:      bot.ModalCallbackReject,
				PrivateMetadata: fmt.Sprintf("%d", prNumber),
				State: &slack.ViewState{
					Values: map[string]map[string]slack.BlockAction{
						bot.BlockRejReason: {
							bot.ActionRejReason: {Value: reason},
						},
					},
				},
			},
			User: slack.User{ID: env.approverID},
		},
	}
	if err := queue.Enqueue(context.Background(), env.store.Redis(), evt); err != nil {
		t.Fatalf("inject reject submit: %v", err)
	}
}
