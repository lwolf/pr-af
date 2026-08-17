package node

import (
	"reflect"
	"testing"

	"github.com/Agent-Field/pr-af/go/internal/config"
)

func TestHarnessConfigProviderAwareBinary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cases := []struct {
		name string
		conf config.AIIntegrationConfig
		want string
	}{
		{"codex uses SDK default", config.AIIntegrationConfig{Provider: "codex", OpencodeBin: "C:/bin/opencode-custom"}, ""},
		{"opencode uses configured binary", config.AIIntegrationConfig{Provider: "opencode", OpencodeBin: "C:/bin/opencode-custom"}, "C:/bin/opencode-custom"},
		{"generic override wins", config.AIIntegrationConfig{Provider: "codex", OpencodeBin: "C:/bin/opencode-custom", HarnessBin: "C:/bin/provider-custom"}, "C:/bin/provider-custom"},
		{"generic override wins for opencode", config.AIIntegrationConfig{Provider: "opencode", OpencodeBin: "C:/bin/opencode-custom", HarnessBin: "C:/bin/provider-custom"}, "C:/bin/provider-custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := harnessConfig(tc.conf).BinPath; got != tc.want {
				t.Errorf("BinPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHarnessConfigPreservesExistingFields(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	for _, key := range []string{"OPENROUTER_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GH_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("OPENAI_API_KEY", "openai-key")
	conf := config.AIIntegrationConfig{
		Provider: "opencode", HarnessModel: "test-model", MaxTurns: 17,
		OpencodeBin: "C:/bin/opencode-custom",
	}
	got := harnessConfig(conf)
	if got.Provider != conf.Provider || got.Model != conf.HarnessModel || got.MaxTurns != conf.MaxTurns || got.PermissionMode != "auto" || got.BinPath != conf.OpencodeBin {
		t.Errorf("harnessConfig fields = %+v", got)
	}
	if want := map[string]string{"OPENAI_API_KEY": "openai-key", "XDG_DATA_HOME": xdg}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %#v, want %#v", got.Env, want)
	}
}

// TestBuildAgentFromEnv is the main.go smoke: BuildAgent resolves node identity
// from the environment (with the pr-af / 8007 defaults), constructs the agent
// without a control plane or LLM key, and RegisterAll wires the full surface.
func TestBuildAgentFromEnv(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantNodeID string
		wantServer string
		wantListen string
	}{
		{
			name:       "defaults when env unset",
			env:        map[string]string{"NODE_ID": "", "PORT": "", "AGENTFIELD_SERVER": "", "OPENROUTER_API_KEY": "", "OPENCODE_API_KEY": ""},
			wantNodeID: "pr-af",
			wantServer: "http://localhost:8080",
			wantListen: ":8007",
		},
		{
			name: "env overrides",
			env: map[string]string{
				"NODE_ID":            "pr-af-canary",
				"PORT":               "9107",
				"AGENTFIELD_SERVER":  "http://cp.internal:8080",
				"OPENROUTER_API_KEY": "", // keep AIConfig off so New needs no key
				"OPENCODE_API_KEY":   "",
			},
			wantNodeID: "pr-af-canary",
			wantServer: "http://cp.internal:8080",
			wantListen: ":9107",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			n, err := BuildAgent("pr-af", "8007", "AI-Native Pull Request Review Agent")
			if err != nil {
				t.Fatalf("BuildAgent: %v", err)
			}
			if n.App == nil {
				t.Fatal("BuildAgent returned a nil App")
			}
			if n.NodeID != tc.wantNodeID {
				t.Errorf("NodeID = %q, want %q", n.NodeID, tc.wantNodeID)
			}
			if n.AgentFieldServer != tc.wantServer {
				t.Errorf("AgentFieldServer = %q, want %q", n.AgentFieldServer, tc.wantServer)
			}
			if n.ListenAddress != tc.wantListen {
				t.Errorf("ListenAddress = %q, want %q", n.ListenAddress, tc.wantListen)
			}

			n.RegisterAll()
			if got := len(n.RegisteredNames()); got != 17 {
				t.Errorf("registered %d reasoners, want 17", got)
			}
		})
	}
}

// TestBuildAgentWithLLMKey proves AIConfig attaches (and agent.New still
// succeeds) when OPENROUTER_API_KEY is present — the production path.
func TestBuildAgentWithLLMKey(t *testing.T) {
	t.Setenv("NODE_ID", "")
	t.Setenv("PORT", "")
	t.Setenv("AGENTFIELD_SERVER", "")
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")

	n, err := BuildAgent("pr-af", "8007", "desc")
	if err != nil {
		t.Fatalf("BuildAgent with LLM key: %v", err)
	}
	if n.App == nil {
		t.Fatal("nil App")
	}
}

// resolveAIBackend picks the .ai() endpoint and normalises the model id. The
// routing prefix must be stripped: Python consumes it inside LiteLLM, so a
// prefixed PR_AF_MODEL (the deploy default) reaching the API verbatim is not a
// valid model id at either provider. Unprefixed models pass through untouched.
func TestResolveAIBackend(t *testing.T) {
	const (
		zenBase        = "https://opencode.ai/zen/v1"
		zenGoBase      = "https://opencode.ai/zen/go/v1"
		openRouterBase = "https://openrouter.ai/api/v1"
	)

	cases := []struct {
		name     string
		model    string
		env      map[string]string
		wantNil  bool
		wantMode string
		wantBase string
		wantKey  string
	}{
		{
			name:     "openrouter prefix strips and routes to openrouter",
			model:    "openrouter/moonshotai/kimi-k2.5",
			env:      map[string]string{"OPENROUTER_API_KEY": "sk-or"},
			wantMode: "moonshotai/kimi-k2.5",
			wantBase: openRouterBase,
			wantKey:  "sk-or",
		},
		{
			name:     "opencode prefix strips and routes to zen",
			model:    "opencode/claude-sonnet-5",
			env:      map[string]string{"OPENCODE_API_KEY": "sk-zen"},
			wantMode: "claude-sonnet-5",
			wantBase: zenBase,
			wantKey:  "sk-zen",
		},
		{
			// The footgun this ordering exists to prevent: a Zen model id must
			// never be posted to OpenRouter just because that key happens to be
			// the one present.
			name:    "prefixed model without its provider key is unconfigured",
			model:   "opencode/claude-sonnet-5",
			env:     map[string]string{"OPENROUTER_API_KEY": "sk-or"},
			wantNil: true,
		},
		{
			// Zen's two packages share OPENCODE_API_KEY but not their
			// catalogs, so the prefix must pick the endpoint. glm-5.3 is
			// Go-only: routing it to plain /zen/v1 would fail at request time.
			name:     "opencode-go prefix strips and routes to the Go package",
			model:    "opencode-go/glm-5.3",
			env:      map[string]string{"OPENCODE_API_KEY": "sk-zen"},
			wantMode: "glm-5.3",
			wantBase: zenGoBase,
			wantKey:  "sk-zen",
		},
		{
			// "opencode-go/" must not be swallowed by the "opencode/" entry.
			// HasPrefix separates them ('-' != '/'), and this pins that: the
			// same model id under the two prefixes must reach two endpoints.
			name:     "opencode prefix still routes to plain Zen, not the Go package",
			model:    "opencode/glm-5.2",
			env:      map[string]string{"OPENCODE_API_KEY": "sk-zen"},
			wantMode: "glm-5.2",
			wantBase: zenBase,
			wantKey:  "sk-zen",
		},
		{
			name:     "unprefixed model takes the first configured provider",
			model:    "minimax/minimax-m2.5",
			env:      map[string]string{"OPENCODE_API_KEY": "sk-zen"},
			wantMode: "minimax/minimax-m2.5",
			wantBase: zenGoBase,
			wantKey:  "sk-zen",
		},
		{
			name:     "explicit base URL overrides both providers",
			model:    "opencode/claude-sonnet-5",
			env:      map[string]string{"OPENCODE_API_KEY": "sk-zen", "PR_AF_AI_BASE_URL": "http://gateway.internal/v1", "PR_AF_AI_API_KEY": "sk-gw"},
			wantMode: "claude-sonnet-5",
			wantBase: "http://gateway.internal/v1",
			wantKey:  "sk-gw",
		},
		{
			name:    "no key at all leaves the gates unconfigured",
			model:   "minimax/minimax-m2.5",
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear every key this resolver reads, so an ambient value in the
			// developer's or CI's environment cannot decide the outcome.
			for _, key := range []string{"OPENCODE_API_KEY", "OPENROUTER_API_KEY", "PR_AF_AI_BASE_URL", "PR_AF_AI_API_KEY"} {
				t.Setenv(key, "")
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			got := resolveAIBackend(tc.model)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("resolveAIBackend(%q) = %+v, want nil", tc.model, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("resolveAIBackend(%q) = nil, want a config", tc.model)
			}
			if got.Model != tc.wantMode {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantMode)
			}
			if got.BaseURL != tc.wantBase {
				t.Errorf("BaseURL = %q, want %q", got.BaseURL, tc.wantBase)
			}
			if got.APIKey != tc.wantKey {
				t.Errorf("APIKey = %q, want %q", got.APIKey, tc.wantKey)
			}
		})
	}
}

// BuildOneShot must not invent a control-plane URL. An empty AgentFieldURL is
// what makes the SDK skip workflow-event emission, so a review that runs with
// no control plane does not stall five seconds per phase on a dead localhost
// port. BuildAgent keeps the localhost default for the served node.
func TestBuildOneShotDefaultsToNoControlPlane(t *testing.T) {
	for _, key := range []string{"NODE_ID", "PORT", "AGENTFIELD_SERVER", "OPENROUTER_API_KEY", "OPENCODE_API_KEY"} {
		t.Setenv(key, "")
	}

	one, err := BuildOneShot("pr-af", "8007", "desc")
	if err != nil {
		t.Fatalf("BuildOneShot: %v", err)
	}
	if one.AgentFieldServer != "" {
		t.Errorf("BuildOneShot AgentFieldServer = %q, want empty", one.AgentFieldServer)
	}

	served, err := BuildAgent("pr-af", "8007", "desc")
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if served.AgentFieldServer != defaultControlPlaneURL {
		t.Errorf("BuildAgent AgentFieldServer = %q, want %q", served.AgentFieldServer, defaultControlPlaneURL)
	}
}

// An explicit AGENTFIELD_SERVER still wins for the one-shot path — that is how
// a CI run reports its DAG to a self-hosted control plane.
func TestBuildOneShotHonoursExplicitControlPlane(t *testing.T) {
	for _, key := range []string{"NODE_ID", "PORT", "OPENROUTER_API_KEY", "OPENCODE_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("AGENTFIELD_SERVER", "https://cp.internal:8080")

	n, err := BuildOneShot("pr-af", "8007", "desc")
	if err != nil {
		t.Fatalf("BuildOneShot: %v", err)
	}
	if n.AgentFieldServer != "https://cp.internal:8080" {
		t.Errorf("AgentFieldServer = %q, want the explicit CP URL", n.AgentFieldServer)
	}
}
