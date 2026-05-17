package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

// ApplyHotSwapOptions describes the inputs to the server-side
// dispatcher. The HTTP handler builds this from the request + the
// resolved lease + the project contract.
type ApplyHotSwapOptions struct {
	Project            string
	ArtifactKind       string
	GitRef             string
	RepoURL            string
	TargetNamespace    string
	JobNamespace       string
	SwapContainerImage string
	ServiceAccount     string
	Contract           hotswap.Contract
	Timeout            time.Duration
}

// ApplyHotSwapResult is the structured outcome returned to the caller.
type ApplyHotSwapResult struct {
	JobName       string            `json:"job_name"`
	JobNamespace  string            `json:"job_namespace"`
	ArtifactKind  string            `json:"artifact_kind"`
	GitRef        string            `json:"git_ref"`
	Outcome       string            `json:"outcome"` // persisted | build_failed | swap_failed | timeout
	BuildLogsTail string            `json:"build_logs_tail,omitempty"`
	SwapLogsTail  string            `json:"swap_logs_tail,omitempty"`
	Error         string            `json:"error,omitempty"`
	Timings       map[string]string `json:"timings"`
}

// k8sJobClient is the surface ApplyHotSwap needs from the k8s API.
// In production this is implemented by httpK8sJobClient (talks to the
// kubernetes API over HTTP using the in-cluster SA token, exactly like
// KubernetesNativeLauncher.request). Tests inject a fake.
//
// Carving this as a small interface keeps ApplyHotSwap pure-logic and
// avoids the kubectl-shell-out approach that broke the first cut of
// this endpoint (glimmung pod has no kubectl in the runtime image).
type k8sJobClient interface {
	ApplyJob(ctx context.Context, namespace string, spec map[string]any) error
	WaitForJob(ctx context.Context, namespace, name string, timeout time.Duration) (terminal string, err error)
	GetPodLogs(ctx context.Context, namespace, podLabelSelector, container string) (string, error)
	DeleteJob(ctx context.Context, namespace, name string) error
}

// ApplyHotSwap dispatches the build-and-swap Job and watches it to
// completion. Synchronous; the caller (HTTP handler) blocks. The Job
// has two containers:
//
//  1. Init container (contract.<kind>.builder_image): git clone +
//     contract.<kind>.build_command. Leaves source at /work/source.
//  2. Main container (bitnami/kubectl): resolves target pods via
//     kubectl-get against contract.<kind>.pod_selector, then for each
//     pod tar-streams /work/source into contract.<kind>.target and
//     sends the configured restart signal.
//
// v1 supports artifact_kind=agent_runner only.
func ApplyHotSwap(ctx context.Context, k8s k8sJobClient, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
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
		// glimmung-runs is where the glimmung pod's SA has Job/create
		// RBAC (via the glimmung-native-launcher Role). The glimmung
		// namespace itself doesn't grant Job/create to the orchestrator's
		// own SA — by design, since glimmung's namespace is for the
		// orchestrator deployment, not for dispatched workloads.
		opts.JobNamespace = "glimmung-runs"
	}
	if opts.ServiceAccount == "" {
		// glimmung-native-runner is the SA for dispatched workloads in
		// glimmung-runs. The apply-hot-swap Job's swap container runs
		// `kubectl get/exec` against pods in the target slot's session
		// namespace; the cross-namespace pods/get+list+exec permission
		// is granted via charts/.../templates/native-runner-pods-exec-rbac.yaml
		// (ClusterRole bound to this SA).
		opts.ServiceAccount = "glimmung-native-runner"
	}
	if opts.SwapContainerImage == "" {
		// `bitnami/kubectl:1.31` (and other version-pinned tags) were
		// removed from Docker Hub in late 2025 when Bitnami changed
		// their tagging policy — only `:latest` + SHA digest tags are
		// published now. Use `:latest`; the swap container is short-
		// lived and only needs sh + kubectl, so version drift is low-
		// risk here. A caller can override SwapContainerImage to pin
		// to a specific digest if reproducibility matters.
		opts.SwapContainerImage = "bitnami/kubectl:latest"
	}

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
		TargetPodSelector:  opts.Contract.AgentRunner.PodSelector,
		TargetContainer:    opts.Contract.AgentRunner.Container,
		RestartSignal:      opts.Contract.AgentRunner.Restart,
	})

	applyStart := time.Now()
	if err := k8s.ApplyJob(ctx, opts.JobNamespace, spec); err != nil {
		result.Outcome = "swap_failed"
		result.Error = fmt.Sprintf("apply job: %v", err)
		result.Timings["job_apply"] = time.Since(applyStart).String()
		return result, err
	}
	result.Timings["job_apply"] = time.Since(applyStart).String()

	waitStart := time.Now()
	terminal, waitErr := k8s.WaitForJob(ctx, opts.JobNamespace, jobName, opts.Timeout)
	result.Timings["job_wait"] = time.Since(waitStart).String()

	// Always collect logs (success or fail). The pod log selector is the
	// Job's pod (label job-name=<name> is auto-added by the Job controller).
	podSelector := "job-name=" + jobName
	buildLogs, _ := k8s.GetPodLogs(ctx, opts.JobNamespace, podSelector, "build")
	result.BuildLogsTail = tailLog(buildLogs, 4000)
	swapLogs, _ := k8s.GetPodLogs(ctx, opts.JobNamespace, podSelector, "swap")
	result.SwapLogsTail = tailLog(swapLogs, 4000)

	if waitErr != nil {
		switch terminal {
		case "timeout":
			result.Outcome = "timeout"
		case "failed":
			// Distinguish build failure from swap failure by inspecting
			// the logs. If the build logs end with a non-zero shell exit
			// pattern OR the swap logs are empty, it was a build failure.
			if strings.TrimSpace(swapLogs) == "" || strings.Contains(strings.ToLower(buildLogs), "error") {
				result.Outcome = "build_failed"
			} else {
				result.Outcome = "swap_failed"
			}
		default:
			result.Outcome = "swap_failed"
		}
		result.Error = waitErr.Error()
		return result, waitErr
	}

	result.Outcome = "persisted"
	result.Timings["total"] = time.Since(start).String()

	// Best-effort cleanup; ttlSecondsAfterFinished covers it if delete fails.
	_ = k8s.DeleteJob(ctx, opts.JobNamespace, jobName)

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
	TargetPodSelector  string
	TargetContainer    string
	RestartSignal      string
}

func renderApplyHotSwapJobSpec(in applyHotSwapJobInputs) map[string]any {
	labels := map[string]any{
		"app.kubernetes.io/name":          "glimmung-apply-hot-swap",
		"glimmung.io/project":             in.Project,
		"glimmung.io/apply-hot-swap-kind": "agent_runner",
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
	// Init container: clone the repo at GIT_REF, run the build command,
	// leave the resulting source dir at /work/source.
	//
	// `git` is always required (it's how the source enters the container)
	// but it isn't always preinstalled in minimal builder images — e.g.
	// `node:20-alpine` ships node + npm but no git. Rather than push that
	// concern onto every contract author, the build prelude detects the
	// package manager and installs git if missing. Costs ~5s on first run
	// per builder image; not worth optimizing past that for v1.
	return strings.Join([]string{
		"set -e",
		"set -x",
		// Best-effort git install if missing. Handles alpine (apk),
		// debian/ubuntu (apt-get), and the rare yum/dnf builder. If
		// none match, the subsequent `git clone` fails with a clear
		// "git: not found" that surfaces to the caller's build_logs_tail.
		`if ! command -v git >/dev/null 2>&1; then`,
		`  if command -v apk >/dev/null 2>&1; then apk add --no-cache git;`,
		`  elif command -v apt-get >/dev/null 2>&1; then apt-get update -qq && apt-get install -y -qq git;`,
		`  elif command -v dnf >/dev/null 2>&1; then dnf install -y -q git;`,
		`  elif command -v yum >/dev/null 2>&1; then yum install -y -q git;`,
		`  fi`,
		`fi`,
		`git clone --depth=1 --branch "$GIT_REF" "$REPO_URL" /work/repo`,
		`cd /work/repo`,
		in.BuildCommand,
		`cp -R "/work/repo/` + in.Source + `" /work/source`,
		`ls -la /work/source | head`,
	}, "\n")
}

func swapScriptFor(in applyHotSwapJobInputs) string {
	// Swap container resolves target pods at run time (rather than the
	// dispatcher resolving them up front — that was the kubectl-in-
	// glimmung-pod bug from the first cut of this endpoint). Then for
	// each pod tar-streams /work/source into Target and signals PID 1.
	restartCmd := restartCommandFor(in.RestartSignal)
	return strings.Join([]string{
		"set -e",
		"set -x",
		`pods=$(kubectl -n ` + shellQuote(in.TargetNamespace) + ` get pods -l ` + shellQuote(in.TargetPodSelector) +
			` -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')`,
		`if [ -z "$pods" ]; then echo "no pods matched selector ` + shellQuote(in.TargetPodSelector) + ` in namespace ` + shellQuote(in.TargetNamespace) + `"; exit 1; fi`,
		`for pod in $pods; do`,
		`  echo "==> swapping into $pod"`,
		`  kubectl -n ` + shellQuote(in.TargetNamespace) + ` exec "$pod" -c ` + shellQuote(in.TargetContainer) +
			` -- sh -c ` + shellQuote("mkdir -p "+in.Target) + ` < /dev/null`,
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
		return "sh -c 'kill -HUP 1'"
	}
}

func tailLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func randHex(n int) string {
	buf := make([]byte, n/2+1)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:n]
}

// ─────────────────────────────────────────────────────────────────────────────
// Production k8s client (HTTP API, no kubectl) — mirrors the
// KubernetesNativeLauncher.request pattern.
// ─────────────────────────────────────────────────────────────────────────────

type httpK8sJobClient struct {
	Settings   Settings
	HTTPClient *http.Client
}

func newHTTPK8sJobClient(settings Settings) *httpK8sJobClient {
	return &httpK8sJobClient{Settings: settings}
}

func (c *httpK8sJobClient) ApplyJob(ctx context.Context, namespace string, spec map[string]any) error {
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs"
	status, _, err := c.request(ctx, http.MethodPost, path, spec)
	if err != nil && status != http.StatusConflict {
		return err
	}
	return nil
}

func (c *httpK8sJobClient) WaitForJob(ctx context.Context, namespace, name string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("namespace + name required")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name
	for {
		_, job, err := c.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		statusMap, _ := job["status"].(map[string]any)
		conditions, _ := statusMap["conditions"].([]any)
		for _, raw := range conditions {
			cond, _ := raw.(map[string]any)
			t, _ := cond["type"].(string)
			s, _ := cond["status"].(string)
			r, _ := cond["reason"].(string)
			if t == "Complete" && s == "True" {
				return "complete", nil
			}
			if t == "Failed" && s == "True" {
				return "failed", fmt.Errorf("job failed: %s", r)
			}
		}
		select {
		case <-deadline.C:
			return "timeout", fmt.Errorf("job did not complete within %s", timeout)
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (c *httpK8sJobClient) GetPodLogs(ctx context.Context, namespace, labelSelector, container string) (string, error) {
	// First find the pod name(s) matching the selector.
	listPath := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + httpQueryEscape(labelSelector)
	_, list, err := c.request(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		return "", err
	}
	items, _ := list["items"].([]any)
	if len(items) == 0 {
		return "", nil
	}
	// Take the first pod (Job typically has one pod since backoffLimit=0).
	pod, _ := items[0].(map[string]any)
	metadata, _ := pod["metadata"].(map[string]any)
	podName, _ := metadata["name"].(string)
	if podName == "" {
		return "", nil
	}
	logsPath := "/api/v1/namespaces/" + namespace + "/pods/" + podName + "/log?container=" + httpQueryEscape(container) + "&tailLines=200"
	body, err := c.requestRaw(ctx, http.MethodGet, logsPath)
	if err != nil {
		return "", err
	}
	return body, nil
}

func (c *httpK8sJobClient) DeleteJob(ctx context.Context, namespace, name string) error {
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name + "?propagationPolicy=Background"
	_, _, err := c.request(ctx, http.MethodDelete, path, nil)
	return err
}

func (c *httpK8sJobClient) request(ctx context.Context, method, path string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Settings.K8sAPIHost, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	token, err := os.ReadFile(c.Settings.K8sSATokenPath)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: c.transport()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil, fmt.Errorf("kubernetes %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if len(respBody) == 0 {
		return resp.StatusCode, map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

func (c *httpK8sJobClient) requestRaw(ctx context.Context, method, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Settings.K8sAPIHost, "/")+path, nil)
	if err != nil {
		return "", err
	}
	token, err := os.ReadFile(c.Settings.K8sSATokenPath)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: c.transport()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("kubernetes %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func (c *httpK8sJobClient) transport() http.RoundTripper {
	tr := &http.Transport{}
	if c.Settings.K8sCACertPath != "" {
		caCert, err := os.ReadFile(c.Settings.K8sCACertPath)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caCert) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return tr
}

func httpQueryEscape(s string) string {
	// Minimal URL escaping for label selectors and container names. Both
	// contain alphanumerics + a small set of punctuation that's safe-
	// looking enough; we escape just `=` and ` ` which appear in selectors.
	r := strings.NewReplacer(" ", "%20", "=", "%3D")
	return r.Replace(s)
}
