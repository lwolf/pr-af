// Command pr-af-review runs ONE review, in this process, and exits.
//
// It is the CI-shaped counterpart to cmd/pr-af: same node, same 17-reasoner
// surface, same orchestrator — but no HTTP listener, no control-plane
// registration, and no polling loop. Where the served node is reached at
// POST {CP}/api/v1/execute/async/pr-af.review and scripts/ci_runner.py waits on
// the execution, this binary calls the review handler directly and blocks until
// it returns.
//
// # WHY THIS EXISTS
//
// The docker-compose stack (control plane + node) is a lot of moving parts for
// a CI job whose whole shape is "review this PR, then exit". The pipeline does
// not actually need any of it: the orchestrator drives its phases through
// agent.CallLocal, which dispatches registered reasoners INSIDE this process,
// and the control plane's remaining role is to receive workflow events — which
// the SDK emits best-effort and skips entirely when AGENTFIELD_SERVER is empty.
// So a review reduces to one static binary plus the opencode CLI and git.
//
// Point AGENTFIELD_SERVER at a control plane and the run reports its DAG there
// as usual; leave it unset and the review is fully self-contained.
//
// USAGE
//
//	pr-af-review --pr https://github.com/owner/repo/pull/123
//	pr-af-review --pr <url> --depth deep --dry-run
//	pr-af-review --repo-path . --base-ref master --head-ref HEAD
//
// Exit status is 0 when the review completed (findings or not) and 1 when the
// pipeline failed. A review that completes and posts nothing is a SUCCESS —
// "no findings worth posting" is a valid verdict, not an error.
//
// The exception is a review that ran ZERO dimensions, which exits 1: see
// checkDimensionsRun for why an empty review is indistinguishable from a clean
// one in the result payload, and why CI must not treat it as a pass.
//
// # ENVIRONMENT
//
// The same variables the served node reads. The ones that matter here:
//
//	OPENCODE_API_KEY   OpenCode Zen key — the harness CLI reads it directly, and
//	                   the .ai() gates use it when PR_AF_MODEL names opencode/*
//	OPENROUTER_API_KEY OpenRouter key (upstream's original backend)
//	PR_AF_MODEL        harness model, provider-prefixed (opencode/claude-sonnet-5)
//	GH_TOKEN           GitHub token: fetch the PR, clone it, post the review
//	AGENTFIELD_SERVER  optional control plane for workflow events
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Agent-Field/pr-af/go/internal/node"
	"github.com/Agent-Field/pr-af/go/internal/schemas"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "pr-af-review: %v\n", err)
		os.Exit(1)
	}
}

// run is main's testable body: flags in, review out, error on failure.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("pr-af-review", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		prURL    = fs.String("pr", "", "GitHub pull request URL to review")
		repoPath = fs.String("repo-path", "", "local repository path (alternative to --pr)")
		baseRef  = fs.String("base-ref", "", "base ref for a --repo-path review")
		headRef  = fs.String("head-ref", "", "head ref for a --repo-path review")
		depth    = fs.String("depth", "standard", "review depth: auto | quick | standard | deep")
		focus    = fs.String("focus", "", "review focus: auto | security | correctness | performance | tests")
		dryRun   = fs.Bool("dry-run", false, "run the pipeline but do not post the review to GitHub")
		out      = fs.String("out", "", "write the full review result as JSON to this path")

		// The SDK writes its structured execution log to stdout unconditionally
		// (agent.writeStructuredExecutionLog), so stdout is a mixed stream and
		// --out is the machine-readable channel.
		minDimensions = fs.Int("require-dimensions", 1,
			"fail unless the pipeline actually ran at least this many review dimensions (0 disables)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prURL == "" && *repoPath == "" {
		fs.Usage()
		return errors.New("one of --pr or --repo-path is required")
	}

	input := map[string]any{
		"depth":   *depth,
		"dry_run": *dryRun,
	}
	for key, value := range map[string]string{
		"pr_url":    *prURL,
		"repo_path": *repoPath,
		"base_ref":  *baseRef,
		"head_ref":  *headRef,
		"focus":     *focus,
	} {
		if value != "" {
			input[key] = value
		}
	}

	// SIGINT/SIGTERM cancels the review. The harness runner propagates
	// cancellation to the opencode child process group, so a cancelled CI job
	// does not leave an orphaned CLI holding the workspace.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	n, err := node.BuildOneShot("pr-af", "8007", "AI-Native Pull Request Review Agent")
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	n.RegisterAll()

	result, err := n.RunReview(ctx, input)
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
	}
	fmt.Fprintln(stdout, string(encoded))

	return checkDimensionsRun(result, *minDimensions)
}

// checkDimensionsRun turns a silently-empty review into a failure.
//
// A review whose model backend never worked does NOT surface as an error. Every
// phase catches its own failure, the pipeline completes all nine of them, and
// the result is a well-formed APPROVE carrying "No issues found. This PR looks
// clean across all review dimensions." — the exact output of a clean review.
// Observed with a deliberately invalid API key: 7 agent invocations, 0 findings,
// event APPROVE, exit 0. Wired to CI as-is, that is a gate that turns green
// precisely when the reviewer is broken.
//
// dimensions_run is what separates the two. A real review plans and runs at
// least one dimension; a review whose intake and planning gates both failed
// runs zero, because there was never a plan to execute. Findings count cannot
// serve here — a genuinely clean PR also has none.
func checkDimensionsRun(result any, minimum int) error {
	if minimum <= 0 {
		return nil
	}
	review, ok := result.(schemas.ReviewResult)
	if !ok {
		return fmt.Errorf("unexpected review result type %T", result)
	}
	if got := review.Summary.DimensionsRun; got < minimum {
		return fmt.Errorf(
			"review ran %d dimensions, want at least %d: the pipeline completed but reviewed nothing, "+
				"which usually means the model backend was unreachable or the API key was rejected "+
				"(check the harness errors above; re-run with --require-dimensions=0 to accept this)",
			got, minimum)
	}
	return nil
}
