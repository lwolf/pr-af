// Package node is the PR-AF wiring wave (T4.2): it constructs the shared
// *agent.Agent from the environment and registers the exact Python reasoner
// surface (design §B.1) so the Go node is a drop-in opt-in sibling of the Python
// pr-af node.
//
// node.go owns agent construction (env -> agent.Config, mirroring
// src/pr_af/app.py:26-50) plus the custom HTTP server that wraps the SDK handler
// with the /webhook/github route (app.py:365-367). register.go owns the
// per-reasoner registration (the 17 names of §B.1); webhook.go owns the GitHub
// @mention webhook handler.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Agent-Field/agentfield/sdk/go/agent"
	"github.com/Agent-Field/agentfield/sdk/go/ai"

	"github.com/Agent-Field/pr-af/go/internal/config"
	"github.com/Agent-Field/pr-af/go/internal/github"
	"github.com/Agent-Field/pr-af/go/internal/orch"
	"github.com/Agent-Field/pr-af/go/internal/schemas"
)

// Node bundles the constructed agent with the resolved environment config and
// the collaborators the review handler and webhook thread through.
type Node struct {
	// App is the SDK agent. It satisfies orch.App (Harness/AI/Pause/Note) and
	// the reasoner Deps interfaces directly, so register.go points every Deps
	// field at it and Serve mounts App.Handler() as the fallback route.
	App *agent.Agent

	// labelDedupe bounds duplicate label-triggered review dispatches. It is
	// process-local by design; see webhookDedupe.claim for the limitation.
	labelDedupe webhookDedupe

	// webhookClient is nil in production (fireReview uses a bounded default).
	// Tests inject a transport so webhook dispatches need no listening socket.
	webhookClient *http.Client

	// NodeID is the resolved node id (NODE_ID env, or the pr-af default).
	NodeID string

	// AgentFieldServer is the control-plane base URL (AGENTFIELD_SERVER). The
	// webhook fires the async review at "{AgentFieldServer}/api/v1/execute/async/
	// {NodeID}.review" and the HITL gate derives the approval webhook URL from it.
	AgentFieldServer string

	// ListenAddress is the ":port" the custom server binds (":"+PORT).
	ListenAddress string

	// reviewApp is the agent-capability seam the review handler feeds into
	// orch.Deps.App AND emits the pipeline-failure note through. It defaults to
	// App; the error-mapping tests override it with a fake that records notes.
	reviewApp orch.App

	// gh is the GitHub client injected into orch.Deps.GH. Tests that stub
	// runReview leave it unused (nil).
	gh github.Client

	// localCaller is the tracked same-process invocation seam fed into
	// orch.Deps.Local: production points it at App so every pipeline phase is
	// reported to the control plane as a child execution (the review DAG).
	// Tests that stub runReview or need direct-call phases leave it nil.
	localCaller orch.LocalCaller

	// runReview is the orchestrator-construct-and-run seam. Production builds and
	// runs the real orchestrator; the error-mapping tests inject ErrBadInput /
	// other failures without a live harness (design §F "seam for the orchestrator
	// constructor").
	runReview func(ctx context.Context, deps orch.Deps, in schemas.ReviewInput, cfg config.ReviewConfig) (schemas.ReviewResult, error)

	// registered records every reasoner name passed through the single
	// registration path, in order, so the parity test (V1) can assert the exact
	// surface. tags records the tags registered per name (review -> nil; the 16
	// internal reasoners -> ["review","pr"]).
	registered []string
	tags       map[string][]string
}

// RegisteredNames returns a copy of the reasoner names registered on this node,
// in registration order — the functional/unit parity source of truth (V1).
func (n *Node) RegisteredNames() []string {
	return append([]string(nil), n.registered...)
}

// TagsFor returns a copy of the tags registered for name (nil when none).
func (n *Node) TagsFor(name string) []string {
	return append([]string(nil), n.tags[name]...)
}

// defaultRunReview constructs the real orchestrator and runs it. Kept as a
// package function so BuildAgent can point Node.runReview at it and tests can
// swap in a stub.
func defaultRunReview(ctx context.Context, deps orch.Deps, in schemas.ReviewInput, cfg config.ReviewConfig) (schemas.ReviewResult, error) {
	return orch.New(deps, in, cfg).Run(ctx)
}

// harnessConfig maps the resolved AI integration configuration to the SDK
// harness configuration. An empty BinPath lets the SDK select the configured
// provider's default executable.
func harnessConfig(c config.AIIntegrationConfig) *agent.HarnessConfig {
	return &agent.HarnessConfig{
		Provider:       c.Provider,
		Model:          c.HarnessModel,
		MaxTurns:       c.MaxTurns,
		PermissionMode: "auto",
		Env:            c.ProviderEnv(),
		BinPath:        resolvedHarnessBin(c),
	}
}

func resolvedHarnessBin(c config.AIIntegrationConfig) string {
	if c.HarnessBin != "" {
		return c.HarnessBin
	}
	if c.Provider == "opencode" {
		return c.OpencodeBin
	}
	return ""
}

// BuildAgent constructs the PR-AF agent from the environment exactly as the
// Python entry point does (app.py:26-50):
//
//   - NODE_ID            default "pr-af" (the maintained package identity).
//   - AGENTFIELD_SERVER  default "http://localhost:8080".
//   - AGENTFIELD_API_KEY -> Config.Token (control-plane bearer).
//   - PORT               default "8007" -> ListenAddress ":8007".
//   - AGENT_CALLBACK_URL -> Config.PublicURL — the base URL the CP uses to reach
//     this node; unset falls back to the SDK's http://localhost:<port>.
//   - HarnessConfig / AIConfig — the harness (opencode) + LLM credentials the
//     reasoners rely on. Every reasoner calls the harness with only Cwd set, so
//     the agent's default HarnessConfig Provider/Model must be present, and the
//     two .ai() gates (intake/coverage) need AIConfig. Mirrors app.py's
//     harness_config=/ai_config=.
//
// Divergence from Python (documented, deliberate): the Go SDK's ai.Config
// rejects an empty API key at construction, whereas Python's AIConfig accepts
// os.getenv("OPENROUTER_API_KEY","") == "". So AIConfig is attached ONLY when
// OPENROUTER_API_KEY is set — construction succeeds without a key (matching
// Python), and the AI call fails at call time either way when the key is absent.
func BuildAgent(defaultNodeID, defaultPort, description string) (*Node, error) {
	return buildAgent(defaultNodeID, defaultPort, description, defaultControlPlaneURL)
}

// BuildOneShot builds the same node as BuildAgent, but defaults
// AGENTFIELD_SERVER to EMPTY instead of http://localhost:8080.
//
// It exists for cmd/pr-af-review, which runs one review in-process and exits.
// That path never calls Serve, so it never registers with a control plane; the
// only thing a CP URL would still do is receive workflow events. Those are
// emitted best-effort — the SDK returns early when AgentFieldURL is empty
// (agent.emitWorkflowEvent) and only logs a send failure otherwise — so an
// empty default means a one-shot review needs no control plane at all, and
// does not spend five seconds per phase timing out against a localhost port
// with nothing behind it.
//
// Set AGENTFIELD_SERVER to a real control plane and the one-shot run reports
// its DAG there exactly as the long-running node does.
func BuildOneShot(defaultNodeID, defaultPort, description string) (*Node, error) {
	return buildAgent(defaultNodeID, defaultPort, description, "")
}

// defaultControlPlaneURL is the CP URL the long-running node assumes when
// AGENTFIELD_SERVER is unset (docker-compose.go.yml sets it explicitly).
const defaultControlPlaneURL = "http://localhost:8080"

func buildAgent(defaultNodeID, defaultPort, description, defaultServer string) (*Node, error) {
	nodeID := envOr("NODE_ID", defaultNodeID)
	server := envOr("AGENTFIELD_SERVER", defaultServer)
	token := os.Getenv("AGENTFIELD_API_KEY")
	port := envOr("PORT", defaultPort)

	aiConf, err := config.AIConfigFromEnv()
	if err != nil {
		// Python constructs AIIntegrationConfig at module import, so a malformed
		// numeric env var (e.g. PR_AF_MAX_TURNS=abc) crashes the node at boot.
		return nil, err
	}

	cfg := agent.Config{
		NodeID:        nodeID,
		Version:       "0.1.0",
		AgentFieldURL: server,
		Token:         token,
		ListenAddress: ":" + port,
		PublicURL:     os.Getenv("AGENT_CALLBACK_URL"),
		CLIConfig:     &agent.CLIConfig{AppDescription: description},
		HarnessConfig: harnessConfig(aiConf),
	}
	cfg.AIConfig = resolveAIBackend(aiConf.AIModel)

	app, err := agent.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create agent %q: %w", nodeID, err)
	}

	n := &Node{
		App:              app,
		NodeID:           nodeID,
		AgentFieldServer: server,
		ListenAddress:    ":" + port,
		reviewApp:        app,
		gh:               github.NewClient(""), // reads GH_TOKEN internally (app.py GitHubClient())
		localCaller:      app,
		runReview:        defaultRunReview,
		tags:             map[string][]string{},
	}
	return n, nil
}

// Serve runs the custom HTTP server and registers with the control plane.
//
// It mounts /webhook/github on the PR-AF handler and delegates every other path
// (/health, /reasoners/, /execute, /discover, …) to the SDK's App.Handler(),
// mirroring Python's app.add_api_route("/webhook/github", …) grafted onto the
// SDK's own routes.
//
// Ordering (design §G): bind the listener BEFORE App.Initialize so the control
// plane's post-registration health check reaches a live server — the same
// startServer→Initialize order agent.Serve uses, reproduced here because the
// webhook route forces a bespoke mux instead of App.Serve.
func (n *Node) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", n.webhookGitHub)
	mux.Handle("/", n.App.Handler())

	ln, err := net.Listen("tcp", n.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen %s: %w", n.ListenAddress, err)
	}
	srv := &http.Server{Handler: mux}

	serveErr := make(chan error, 1)
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			serveErr <- serr
		}
	}()

	if err := n.App.Initialize(ctx); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return fmt.Errorf("initialize node %q: %w", n.NodeID, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		log.Printf("pr-af: received signal %s, shutting down", sig)
	case err := <-serveErr:
		return fmt.Errorf("webhook server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// RunReview executes the `review` reasoner in-process and returns its result,
// exactly as an inbound control-plane execution would — the same handler, the
// same repo resolution, the same orchestrator, the same budget caps.
//
// What it skips is the plumbing around that handler: no listener is bound, no
// registration is sent, and nothing polls. The orchestrator's phases run
// through App.CallLocal, which dispatches registered reasoners inside this
// process, so the whole DAG completes without a control plane in the loop.
//
// Call RegisterAll first — CallLocal resolves phases by registered name.
func (n *Node) RunReview(ctx context.Context, input map[string]any) (any, error) {
	return n.reviewHandler(ctx, input)
}

// envOr returns the value of key, or def when the env var is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// aiProvider is one OpenAI-compatible endpoint the .ai() gates can post to.
//
// Both fields of the pair matter. Prefix is the routing prefix opencode uses in
// a model id ("openrouter/moonshotai/kimi-k2.5", "opencode-go/glm-5.2");
// the harness model KEEPS it, because that is the form the opencode CLI's -m
// flag expects. The .ai() path must STRIP it: Python's .ai() runs through
// LiteLLM, which consumes the prefix as its own routing hint, whereas the Go
// SDK's ai client posts the model string verbatim to BaseURL — where a
// provider-prefixed id is not a valid model.
type aiProvider struct {
	prefix  string
	envKey  string
	baseURL string
}

// aiProviders is ordered: an unprefixed model picks the first entry whose key
// is present. OpenCode Go leads because it is the package this fork is deployed
// against; plain Zen follows; OpenRouter stays as upstream's original backend so
// a checkout configured the upstream way behaves exactly as it did before.
//
// Zen ships as TWO packages behind one OPENCODE_API_KEY, and they are not
// interchangeable — /zen/v1 served 62 models and /zen/go/v1 served 26 when this
// was written, with each carrying ids the other lacks (glm-5.3 is Go-only;
// gpt-5.x and the Claude tiers are plain-Zen-only). Sending an id to the wrong
// one fails at request time, which is why they are separate entries rather than
// a single endpoint with a shared catalog.
//
// The prefixes deliberately reuse the opencode CLI's OWN provider ids, taken
// from its embedded registry ("opencode-go", api https://opencode.ai/zen/go/v1,
// env OPENCODE_API_KEY). Because the harness passes PR_AF_MODEL to `-m`
// verbatim while these gates strip the prefix, any divergence between the two
// vocabularies would need a translation table; matching the CLI means one
// string is correct in both places. Note the HYPHEN: "opencode-go", not
// "opencode_go". No collision with "opencode/" — HasPrefix distinguishes them.
var aiProviders = []aiProvider{
	{prefix: "opencode-go/", envKey: "OPENCODE_API_KEY", baseURL: "https://opencode.ai/zen/go/v1"},
	{prefix: "opencode/", envKey: "OPENCODE_API_KEY", baseURL: "https://opencode.ai/zen/v1"},
	{prefix: "openrouter/", envKey: "OPENROUTER_API_KEY", baseURL: "https://openrouter.ai/api/v1"},
}

// resolveAIBackend picks the endpoint for the two .ai() gates (intake and
// coverage), returning nil when none is configured — the SDK rejects an empty
// API key at construction, so "no key" must mean "no AIConfig", and the gate
// then fails at call time with the SDK's own message. That is upstream's
// behaviour and it is preserved.
//
// Precedence:
//
//  1. PR_AF_AI_BASE_URL + PR_AF_AI_API_KEY — an explicit escape hatch for any
//     OpenAI-compatible endpoint that is neither of the two known providers.
//  2. A model that names a provider ("opencode-go/...", "opencode/...",
//     "openrouter/...") pins the backend to that provider. If its key is missing
//     we return nil rather than silently sending a Zen model id to OpenRouter,
//     or vice versa. Note this pins the PACKAGE too: "opencode-go/glm-5.3" and
//     "opencode/glm-5.3" share a key, but only the first has that model.
//  3. An unprefixed model ("minimax/minimax-m2.5", the code default) takes the
//     first provider whose key is set — OpenCode Go, when OPENCODE_API_KEY is
//     the one present.
func resolveAIBackend(model string) *ai.Config {
	if baseURL := os.Getenv("PR_AF_AI_BASE_URL"); baseURL != "" {
		apiKey := os.Getenv("PR_AF_AI_API_KEY")
		if apiKey == "" {
			log.Printf("pr-af: PR_AF_AI_BASE_URL is set but PR_AF_AI_API_KEY is empty; .ai() gates are unconfigured")
			return nil
		}
		return &ai.Config{Model: stripProviderPrefix(model), APIKey: apiKey, BaseURL: baseURL}
	}

	for _, p := range aiProviders {
		if !strings.HasPrefix(model, p.prefix) {
			continue
		}
		apiKey := os.Getenv(p.envKey)
		if apiKey == "" {
			log.Printf("pr-af: model %q selects provider %q but %s is empty; .ai() gates are unconfigured",
				model, strings.TrimSuffix(p.prefix, "/"), p.envKey)
			return nil
		}
		return &ai.Config{Model: strings.TrimPrefix(model, p.prefix), APIKey: apiKey, BaseURL: p.baseURL}
	}

	for _, p := range aiProviders {
		if apiKey := os.Getenv(p.envKey); apiKey != "" {
			return &ai.Config{Model: model, APIKey: apiKey, BaseURL: p.baseURL}
		}
	}
	return nil
}

// stripProviderPrefix removes a known routing prefix from a model id, leaving
// an unknown or absent prefix alone.
func stripProviderPrefix(model string) string {
	for _, p := range aiProviders {
		if strings.HasPrefix(model, p.prefix) {
			return strings.TrimPrefix(model, p.prefix)
		}
	}
	return model
}
