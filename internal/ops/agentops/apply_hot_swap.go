package agentops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

// ApplyHotSwapOptions describes the inputs to ApplyHotSwap. Caller is
// typically the /v1/test-slots/apply-hot-swap HTTP handler, which has
// already resolved the test-slot lease + project contract.
type ApplyHotSwapOptions struct {
	// Project identifies the source repo (used as a Job label, and the
	// source of the GitRepoURL fallback if RepoURL is empty).
	Project string
	// ArtifactKind selects the contract sub-block: "agent_runner" today.
	// Static and backend kinds route to the legacy ops.TestSlotHotSwap
	// path (called via the glimmung-agent CLI in verify-loop jobs); the
	// developer-driven apply endpoint covers only kinds whose consumers
	// have explicitly opted in.
	ArtifactKind string
	// GitRef is the branch name or commit SHA the init container will
	// clone. Defaults to the project's default branch if empty.
	GitRef string
	// RepoURL is the git URL the init container clones from. Typically
	// derived from the project metadata's github_repo field.
	RepoURL string
	// TargetNamespace + TargetPodSelector identify which pods the swap
	// container will kubectl-stream into.
	TargetNamespace   string
	TargetPodSelector string
	// JobNamespace is where the build-and-swap Job itself runs (typically
	// the glimmung namespace).
	JobNamespace string
	// SwapContainerImage is the image used by the main container in the
	// build-and-swap Job. It only needs `kubectl` + a shell. Pinned at
	// the call site so a registry outage doesn't depend on Docker Hub.
	SwapContainerImage string
	// ServiceAccount the Job pod runs as. Must have permissions to
	// kubectl-exec against TargetNamespace.
	ServiceAccount string
	// Contract carries the resolved hot-swap contract.
	Contract hotswap.Contract
	// Timeout bounds the entire build-and-swap operation. Includes Job
	// dispatch, init-container build, main-container swap, and log
	// collection. The HTTP endpoint clamps this to a hard server max.
	Timeout time.Duration
}

// ApplyHotSwapResult is the structured outcome returned to the caller.
// Includes everything a developer needs to diagnose a failure without
// re-running kubectl by hand.
type ApplyHotSwapResult struct {
	JobName       string            `json:"job_name"`
	JobNamespace  string            `json:"job_namespace"`
	ArtifactKind  string            `json:"artifact_kind"`
	GitRef        string            `json:"git_ref"`
	Outcome       string            `json:"outcome"` // persisted | build_failed | swap_failed | timeout
	TargetPods    []string          `json:"target_pods,omitempty"`
	BuildLogsTail string            `json:"build_logs_tail,omitempty"`
	SwapLogsTail  string            `json:"swap_logs_tail,omitempty"`
	Error         string            `json:"error,omitempty"`
	Timings       map[string]string `json:"timings"`
}

// ApplyHotSwap dispatches a one-off Kubernetes Job that:
//
//  1. Init container: pulls Contract.<kind>.BuilderImage, clones the
//     repo at GitRef, runs the contract's build_command, leaves the
//     resulting source dir in a shared emptyDir.
//
//  2. Main container: pulls SwapContainerImage (kubectl + shell), tar-
//     streams the source dir into Contract.<kind>.Target inside the
//     target pod, then sends the configured restart signal to PID 1.
//
// The function blocks on `kubectl wait --for=condition=complete` with
// Timeout. On any failure path (Job apply, build, swap, timeout) the
// function returns a result with Outcome set to the matching label and
// the relevant logs tail populated. The caller is responsible for
// recording hot-swap history; ApplyHotSwap does not write to the lease
// store directly.
//
// Today only ArtifactKind="agent_runner" is implemented. The static and
// backend kinds route to ops.TestSlotHotSwap via the glimmung-agent CLI
// path and are out of scope for the developer-driven apply endpoint
// until their consumers explicitly opt in.
func (o *Ops) ApplyHotSwap(ctx context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
	start := time.Now()
	result := ApplyHotSwapResult{
		ArtifactKind: opts.ArtifactKind,
		GitRef:       opts.GitRef,
		JobNamespace: opts.JobNamespace,
		Outcome:      "swap_failed",
		Timings:      map[string]string{},
	}

	if opts.ArtifactKind != "agent_runner" {
		result.Error = fmt.Sprintf("artifact_kind %q is not supported by the apply endpoint in v1 (use the glimmung-agent CLI for static/backend)", opts.ArtifactKind)
		return result, fmt.Errorf("%s", result.Error)
	}
	if !opts.Contract.AgentRunner.Enabled {
		result.Error = "contract.agent_runner is not enabled"
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(opts.Contract.AgentRunner.BuilderImage) == "" {
		result.Error = "contract.agent_runner.builder_image is required"
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(opts.TargetNamespace) == "" {
		result.Error = "target_namespace is required"
		return result, fmt.Errorf("%s", result.Error)
	}
	if strings.TrimSpace(opts.RepoURL) == "" {
		result.Error = "repo_url is required (typically from project metadata's github_repo)"
		return result, fmt.Errorf("%s", result.Error)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.JobNamespace == "" {
		opts.JobNamespace = "glimmung"
	}
	if opts.SwapContainerImage == "" {
		// bitnami/kubectl is a small, well-maintained kubectl-only image.
		// Pinned at the call site to a specific tag in production; this
		// fallback is only used in unit tests.
		opts.SwapContainerImage = "bitnami/kubectl:1.31"
	}

	// Resolve target pods up front so the Job's swap command can be
	// rendered with literal pod names (no in-Job kubectl-get gymnastics).
	pods, err := o.resolveHotSwapPods(ctx, opts.TargetNamespace, opts.TargetPodSelector)
	if err != nil {
		result.Error = fmt.Sprintf("resolve target pods: %v", err)
		return result, err
	}
	result.TargetPods = pods

	jobName := "apply-hot-swap-" + randHex(8)
	result.JobName = jobName

	spec := renderApplyHotSwapJobSpec(applyHotSwapJobInputs{
		JobName:            jobName,
		JobNamespace:       opts.JobNamespace,
		ServiceAccount:     opts.ServiceAccount,
		Project:            opts.Project,
		GitRef:             opts.GitRef,
		RepoURL:            opts.RepoURL,
		BuilderImage:       opts.Contract.AgentRunner.BuilderImage,
		BuildCommand:       opts.Contract.AgentRunner.BuildCommand,
		SwapContainerImage: opts.SwapContainerImage,
		Source:             opts.Contract.AgentRunner.Source,
		Target:             opts.Contract.AgentRunner.Target,
		TargetNamespace:    opts.TargetNamespace,
		TargetPods:         pods,
		TargetContainer:    opts.Contract.AgentRunner.Container,
		RestartSignal:      opts.Contract.AgentRunner.Restart,
	})

	specJSON, err := json.Marshal(spec)
	if err != nil {
		result.Error = fmt.Sprintf("render job spec: %v", err)
		return result, err
	}

	applyStart := time.Now()
	applyResult, err := o.runner().Run(ctx, Command{
		Name:         "kubectl",
		Args:         []string{"-n", opts.JobNamespace, "apply", "-f", "-"},
		Input:        string(specJSON),
		AllowFailure: true,
	})
	result.Timings["job_apply"] = time.Since(applyStart).String()
	if err != nil || applyResult.ExitCode != 0 {
		result.Outcome = "swap_failed"
		result.Error = fmt.Sprintf("kubectl apply job: exit=%d err=%v stderr=%s", applyResult.ExitCode, err, tailString(applyResult.Stderr, 1000))
		return result, fmt.Errorf("%s", result.Error)
	}

	// kubectl wait blocks until the Job's "complete" condition is true,
	// or the timeout elapses. On timeout the wait returns non-zero; we
	// translate that into Outcome=timeout. On failed condition we
	// collect logs and translate to build_failed or swap_failed.
	waitStart := time.Now()
	waitResult, waitErr := o.runner().Run(ctx, Command{
		Name: "kubectl",
		Args: []string{
			"-n", opts.JobNamespace,
			"wait", "--for=condition=complete",
			"job/" + jobName,
			"--timeout=" + fmt.Sprintf("%ds", int(opts.Timeout.Seconds())),
		},
		AllowFailure: true,
	})
	result.Timings["job_wait"] = time.Since(waitStart).String()

	// Always collect logs (success or fail).
	initLogs, _ := o.runner().Run(ctx, Command{
		Name:         "kubectl",
		Args:         []string{"-n", opts.JobNamespace, "logs", "job/" + jobName, "-c", "build", "--tail=200"},
		AllowFailure: true,
	})
	result.BuildLogsTail = tailString(initLogs.Stdout, 4000)
	swapLogs, _ := o.runner().Run(ctx, Command{
		Name:         "kubectl",
		Args:         []string{"-n", opts.JobNamespace, "logs", "job/" + jobName, "-c", "swap", "--tail=200"},
		AllowFailure: true,
	})
	result.SwapLogsTail = tailString(swapLogs.Stdout, 4000)

	if waitErr != nil || waitResult.ExitCode != 0 {
		// Distinguish build failure (init container exited non-zero)
		// from swap failure (main container exited non-zero).
		failedCondition, _ := o.runner().Run(ctx, Command{
			Name: "kubectl",
			Args: []string{
				"-n", opts.JobNamespace,
				"get", "job/" + jobName,
				"-o", "jsonpath={.status.conditions[?(@.type=='Failed')].reason}",
			},
			AllowFailure: true,
		})
		reason := strings.TrimSpace(failedCondition.Stdout)
		if strings.Contains(strings.ToLower(reason), "deadlineexceeded") || strings.Contains(strings.ToLower(waitResult.Stderr), "timed out") {
			result.Outcome = "timeout"
		} else if strings.Contains(result.BuildLogsTail, "exit") || strings.Contains(strings.ToLower(initLogs.Stderr), "error") {
			result.Outcome = "build_failed"
		} else {
			result.Outcome = "swap_failed"
		}
		result.Error = fmt.Sprintf("job wait failed: exit=%d reason=%q stderr=%s", waitResult.ExitCode, reason, tailString(waitResult.Stderr, 400))
		return result, fmt.Errorf("%s", result.Error)
	}

	result.Outcome = "persisted"
	result.Timings["total"] = time.Since(start).String()

	// Best-effort cleanup. ttlSecondsAfterFinished also covers this if
	// the delete fails (e.g., RBAC). Errors here don't change the result.
	_, _ = o.runner().Run(ctx, Command{
		Name:         "kubectl",
		Args:         []string{"-n", opts.JobNamespace, "delete", "job", jobName, "--ignore-not-found"},
		AllowFailure: true,
	})

	return result, nil
}

type applyHotSwapJobInputs struct {
	JobName            string
	JobNamespace       string
	ServiceAccount     string
	Project            string
	GitRef             string
	RepoURL            string
	BuilderImage       string
	BuildCommand       string
	SwapContainerImage string
	Source             string
	Target             string
	TargetNamespace    string
	TargetPods         []string
	TargetContainer    string
	RestartSignal      string
}

// renderApplyHotSwapJobSpec produces the Job YAML/JSON document the
// dispatcher applies. Init container clones + builds; main container
// kubectl-streams + signals.
//
// Both containers share an emptyDir at /work. The init container ends
// with the built source at /work/source/. The main container reads from
// /work/source/, tars its contents into the target pod's container
// filesystem at Contract.AgentRunner.Target, then sends the configured
// restart signal to PID 1 of the target container.
func renderApplyHotSwapJobSpec(in applyHotSwapJobInputs) map[string]any {
	labels := map[string]any{
		"app.kubernetes.io/name":             "glimmung-apply-hot-swap",
		"glimmung.io/project":                in.Project,
		"glimmung.io/apply-hot-swap-kind":    "agent_runner",
	}
	buildScript := buildScriptFor(in)
	swapScript := swapScriptFor(in)
	podSpec := map[string]any{
		"restartPolicy": "Never",
		"volumes": []any{
			map[string]any{"name": "work", "emptyDir": map[string]any{}},
		},
		"initContainers": []any{
			map[string]any{
				"name":    "build",
				"image":   in.BuilderImage,
				"command": []any{"sh", "-c"},
				"args":    []any{buildScript},
				"env": []any{
					map[string]any{"name": "GIT_REF", "value": in.GitRef},
					map[string]any{"name": "REPO_URL", "value": in.RepoURL},
				},
				"volumeMounts": []any{
					map[string]any{"name": "work", "mountPath": "/work"},
				},
			},
		},
		"containers": []any{
			map[string]any{
				"name":    "swap",
				"image":   in.SwapContainerImage,
				"command": []any{"sh", "-c"},
				"args":    []any{swapScript},
				"volumeMounts": []any{
					map[string]any{"name": "work", "mountPath": "/work"},
				},
			},
		},
	}
	if strings.TrimSpace(in.ServiceAccount) != "" {
		podSpec["serviceAccountName"] = in.ServiceAccount
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      in.JobName,
			"namespace": in.JobNamespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func buildScriptFor(in applyHotSwapJobInputs) string {
	// The init container's job is end-to-end deterministic: clone the
	// repo at the requested ref, run the contract's build command, leave
	// the resulting source directory at /work/source. Failures here
	// propagate via container exit code; the wait loop translates them
	// into Outcome=build_failed.
	return strings.Join([]string{
		"set -e",
		"set -x",
		// Clone shallow + ref. Some refs are branches (no leading "refs/")
		// and some are commit SHAs; --depth=1 + --branch covers branches.
		// For SHA clones we'd need a fetch+checkout dance; v1 supports
		// branches and tags only.
		`git clone --depth=1 --branch "$GIT_REF" "$REPO_URL" /work/repo`,
		`cd /work/repo`,
		// Build command runs in the cloned repo root. The contract's
		// build_command is responsible for placing the artifact at the
		// path Contract.<kind>.Source names (e.g., agent-runner/dist).
		in.BuildCommand,
		// Move the built source into a stable location for the swap
		// container. The contract names Source as a path relative to the
		// repo root.
		`cp -R "/work/repo/` + in.Source + `" /work/source`,
		`ls -la /work/source | head`,
	}, "\n")
}

func swapScriptFor(in applyHotSwapJobInputs) string {
	// The swap container's job is also end-to-end deterministic: for
	// each target pod, stream the built source into the target volume
	// via `tar | kubectl exec`, then send the configured restart signal.
	//
	// Multiple target pods: stream sequentially. Failure on any pod
	// fails the whole swap (exit on the first non-zero); for v1 the
	// session-pod target set is always 1, so this is bounded.
	podList := strings.Join(quoteShellArgs(in.TargetPods), " ")
	restartCmd := restartCommandFor(in.RestartSignal)
	return strings.Join([]string{
		"set -e",
		"set -x",
		`pods="` + podList + `"`,
		`for pod in $pods; do`,
		`  echo "==> swapping into $pod"`,
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("mkdir -p "+in.Target+" && cd "+in.Target+" && tar xf -") +
			` < /dev/null &`,
		`  pid=$!`,
		`  tar c -C /work/source . | kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec -i "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("cd "+in.Target+" && tar xf -"),
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- ` + restartCmd,
		`done`,
		`echo done`,
	}, "\n")
}

func restartCommandFor(signal string) string {
	switch strings.ToUpper(strings.TrimSpace(signal)) {
	case "SIGHUP":
		return "sh -c 'kill -HUP 1'"
	default:
		// Validator rejects anything but SIGHUP; this is defense in depth.
		return "sh -c 'kill -HUP 1'"
	}
}

func quoteShellArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return out
}

func randHex(n int) string {
	buf := make([]byte, n/2+1)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:n]
}
