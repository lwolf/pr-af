# Modifications on branch `neocad/zen-oneshot`

This branch modifies Agent-Field/pr-af (Apache-2.0). It is maintained for
`lwolf/neocad`, which runs PR-AF as a per-PR CI reviewer on a self-hosted
runner fleet. Both changes are intended to be upstreamable.

Base: `8593130` — "Bound review fan-out: shared agent budget, live ignore_paths,
evidence caps (#68)".

## 1. The `.ai()` gates can address any OpenAI-compatible provider

`go/internal/node/node.go` hard-coded `BaseURL: "https://openrouter.ai/api/v1"`
and read `OPENROUTER_API_KEY`, so the intake and coverage gates could only ever
reach OpenRouter — even when the harness itself was pointed elsewhere.

`resolveAIBackend` now picks the endpoint from the model's routing prefix and
the keys present in the environment: `opencode/*` + `OPENCODE_API_KEY` reaches
OpenCode Zen (`https://opencode.ai/zen/v1`), `openrouter/*` +
`OPENROUTER_API_KEY` reaches OpenRouter exactly as before, and
`PR_AF_AI_BASE_URL` / `PR_AF_AI_API_KEY` address anything else. A model that
names a provider whose key is missing leaves the gates unconfigured rather than
posting that model id to the other provider.

An existing OpenRouter deployment is unaffected: same base URL, same key, same
prefix-stripping behaviour.

No change was needed for the harness itself. The opencode CLI resolves the Zen
provider from `OPENCODE_API_KEY` in the environment with no config file, and
the SDK's harness runner merges the process environment into the subprocess
(`harness/cli.go`), so the key reaches it already.

## 2. `cmd/pr-af-review` — one review, in-process, no control plane

Upstream ships one binary, `cmd/pr-af`, which serves HTTP and registers with an
AgentField control plane; a CI run therefore needs the docker-compose stack and
`scripts/ci_runner.py` to poll it.

`cmd/pr-af-review` runs a single review in-process and exits. It needs no
compose stack and no control plane: the orchestrator dispatches its phases
through `agent.CallLocal`, which resolves registered reasoners inside the
process, and the control plane's remaining role — receiving workflow events —
is best-effort and skipped entirely when `AGENTFIELD_SERVER` is empty.
`node.BuildOneShot` therefore defaults that variable to empty instead of
`http://localhost:8080`. Setting it to a real control plane restores the
telemetry.

Verified: the full nine-phase pipeline completes against a local repository
with no control plane running and no CP contact attempted.

### `--require-dimensions`

The one-shot exits non-zero when the pipeline ran zero review dimensions.

This is not a stylistic choice. A review whose model backend is unreachable or
whose key is rejected does not fail — every phase absorbs its own error, all
nine complete, and the result is a well-formed `APPROVE` reading "No issues
found. This PR looks clean across all review dimensions." Measured with a
deliberately invalid key: 7 agent invocations, 0 findings, event `APPROVE`,
exit 0. In CI that is a gate that goes green precisely when the reviewer is
broken. `dimensions_run` separates an empty review from a clean one; findings
count cannot, because a genuinely clean PR has none either.
