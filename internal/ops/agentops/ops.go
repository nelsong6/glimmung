package agentops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	RegistryName    = "romainecr"
	ImageRepository = "glimmung"
	ProdNamespace   = "glimmung"
	IssueChartPath  = "k8s/issue"
	RepoSlugDefault = "nelsong6/glimmung"
)

type Ops struct {
	Runner     Runner
	RepoRoot   string
	HTTPClient *http.Client
	Sleep      func(time.Duration)
	Stdout     io.Writer
	Stderr     io.Writer
}

func New(runner Runner) *Ops {
	return &Ops{Runner: runner}
}

func (o *Ops) runner() Runner {
	if o.Runner != nil {
		return o.Runner
	}
	return ExecRunner{}
}

func (o *Ops) repoRoot() string {
	if o.RepoRoot != "" {
		return o.RepoRoot
	}
	if root := os.Getenv("GLIMMUNG_REPO_ROOT"); root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			return abs
		}
		return root
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (o *Ops) sleep(d time.Duration) {
	if o.Sleep != nil {
		o.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (o *Ops) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (o *Ops) run(ctx context.Context, name string, args ...string) (string, error) {
	result, err := o.runner().Run(ctx, Command{Name: name, Args: args, Cwd: o.repoRoot()})
	return strings.TrimSpace(result.Stdout), err
}

func (o *Ops) runWithInput(ctx context.Context, input string, name string, args ...string) (string, error) {
	result, err := o.runner().Run(ctx, Command{Name: name, Args: args, Cwd: o.repoRoot(), Input: input})
	return strings.TrimSpace(result.Stdout), err
}

func (o *Ops) runAllowFailure(ctx context.Context, name string, args ...string) (Result, error) {
	return o.runner().Run(ctx, Command{Name: name, Args: args, Cwd: o.repoRoot(), AllowFailure: true})
}

func (o *Ops) runStreamAllowFailure(ctx context.Context, name string, args ...string) {
	_, _ = o.runner().Run(ctx, Command{
		Name:         name,
		Args:         args,
		Cwd:          o.repoRoot(),
		Stdout:       o.Stdout,
		Stderr:       o.Stderr,
		AllowFailure: true,
	})
}

func (o *Ops) ACRRepositoryTag(ctx context.Context, imageTag string) string {
	out, err := o.run(
		ctx,
		"az", "acr", "repository", "show-tags",
		"--name", RegistryName,
		"--repository", ImageRepository,
		"--query", fmt.Sprintf("[?@=='%s'] | [0]", imageTag),
		"--output", "tsv",
	)
	if err != nil {
		return ""
	}
	return out
}

type BuildPreviewImageResult struct {
	Image        string `json:"image"`
	ImageTag     string `json:"image_tag"`
	SkippedBuild bool   `json:"skipped_build"`
}

func (o *Ops) BuildPreviewImage(ctx context.Context, imageTag string) (BuildPreviewImageResult, error) {
	registryServer := RegistryName + ".azurecr.io"
	image := fmt.Sprintf("%s/%s:%s", registryServer, ImageRepository, imageTag)

	existingTag := o.ACRRepositoryTag(ctx, imageTag)
	if existingTag != imageTag {
		if _, err := o.run(
			ctx,
			"az", "acr", "build",
			"--registry", RegistryName,
			"--image", fmt.Sprintf("%s:%s", ImageRepository, imageTag),
			o.repoRoot(),
		); err != nil {
			return BuildPreviewImageResult{}, err
		}
	}

	verified := o.ACRRepositoryTag(ctx, imageTag)
	if verified != imageTag {
		return BuildPreviewImageResult{}, fmt.Errorf("image tag %q not present in %s/%s after build", imageTag, registryServer, ImageRepository)
	}
	return BuildPreviewImageResult{Image: image, ImageTag: imageTag, SkippedBuild: existingTag == imageTag}, nil
}

type RebuildValidationImageOptions struct {
	Release   string
	Namespace string
	Branch    string
	ImageTag  string
	RepoSlug  string
}

type RebuildValidationImageResult struct {
	Release      string `json:"release"`
	Namespace    string `json:"namespace"`
	Image        string `json:"image"`
	ImageTag     string `json:"image_tag"`
	SkippedBuild bool   `json:"skipped_build"`
}

func (o *Ops) RebuildValidationImage(ctx context.Context, opts RebuildValidationImageOptions) (RebuildValidationImageResult, error) {
	if opts.RepoSlug == "" {
		opts.RepoSlug = RepoSlugDefault
	}
	registryServer := RegistryName + ".azurecr.io"
	image := fmt.Sprintf("%s/%s:%s", registryServer, ImageRepository, opts.ImageTag)

	existingTag := o.ACRRepositoryTag(ctx, opts.ImageTag)
	if existingTag != opts.ImageTag {
		if _, err := o.run(
			ctx,
			"az", "acr", "build",
			"--registry", RegistryName,
			"--image", fmt.Sprintf("%s:%s", ImageRepository, opts.ImageTag),
			fmt.Sprintf("https://github.com/%s.git#%s", opts.RepoSlug, opts.Branch),
		); err != nil {
			return RebuildValidationImageResult{}, err
		}
	}
	if _, err := o.run(
		ctx,
		"kubectl", "-n", opts.Namespace, "set", "image",
		"deployment/"+opts.Release,
		"glimmung="+image,
	); err != nil {
		return RebuildValidationImageResult{}, err
	}
	if _, err := o.run(
		ctx,
		"kubectl", "-n", opts.Namespace, "rollout", "status",
		"deployment/"+opts.Release,
		"--timeout=5m",
	); err != nil {
		return RebuildValidationImageResult{}, err
	}
	return RebuildValidationImageResult{
		Release:      opts.Release,
		Namespace:    opts.Namespace,
		Image:        image,
		ImageTag:     opts.ImageTag,
		SkippedBuild: existingTag == opts.ImageTag,
	}, nil
}

type DeployPreviewOptions struct {
	Release    string
	Namespace  string
	ImageTag   string
	PublicHost string
	PRNumber   string
	Timeout    string
}

type DeployPreviewResult struct {
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	URL       string `json:"url"`
}

func (o *Ops) DeployPreview(ctx context.Context, opts DeployPreviewOptions) (DeployPreviewResult, error) {
	if opts.Timeout == "" {
		opts.Timeout = "5m"
	}
	args := []string{
		"upgrade", "--install", opts.Release, filepath.ToSlash(filepath.Join(o.repoRoot(), IssueChartPath)),
		"--namespace", opts.Namespace,
		"--set-string", "image.tag=" + opts.ImageTag,
		"--set-string", "hostname=" + opts.PublicHost,
		"--wait",
		"--timeout", opts.Timeout,
	}
	if opts.PRNumber != "" {
		args = append(args, "--set-string", "prNumber="+opts.PRNumber)
	}
	if _, err := o.run(ctx, "helm", args...); err != nil {
		return DeployPreviewResult{}, err
	}
	return DeployPreviewResult{Release: opts.Release, Namespace: opts.Namespace, URL: "https://" + opts.PublicHost}, nil
}

type LabelReleaseResult struct {
	Release    string `json:"release"`
	Namespace  string `json:"namespace"`
	PRNumber   string `json:"pr_number,omitempty"`
	BranchSlug string `json:"branch_slug,omitempty"`
}

func (o *Ops) LabelReleasePR(ctx context.Context, release, namespace, prNumber string) (LabelReleaseResult, error) {
	for _, kind := range []string{"deployment", "service", "httproute"} {
		if _, err := o.run(
			ctx,
			"kubectl", "-n", namespace, "label",
			kind, release,
			"glimmung.io/pr="+prNumber,
			"--overwrite",
		); err != nil {
			return LabelReleaseResult{}, err
		}
	}
	return LabelReleaseResult{Release: release, Namespace: namespace, PRNumber: prNumber}, nil
}

func (o *Ops) LabelReleaseBranch(ctx context.Context, release, namespace, branchSlug string) (LabelReleaseResult, error) {
	for _, kind := range []string{"deployment", "service", "httproute"} {
		if _, err := o.run(
			ctx,
			"kubectl", "-n", namespace, "label",
			kind, release,
			"glimmung.io/branch="+branchSlug,
			"--overwrite",
		); err != nil {
			return LabelReleaseResult{}, err
		}
	}
	return LabelReleaseResult{Release: release, Namespace: namespace, BranchSlug: branchSlug}, nil
}

type DestroyPreviewResult struct {
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	Destroyed bool   `json:"destroyed"`
}

func (o *Ops) DestroyPreview(ctx context.Context, release, namespace string) (DestroyPreviewResult, error) {
	_, err := o.runAllowFailure(ctx, "helm", "uninstall", release, "--namespace", namespace, "--wait")
	return DestroyPreviewResult{Release: release, Namespace: namespace, Destroyed: true}, err
}

type WaitReadyResult struct {
	Ready  bool   `json:"ready"`
	Status int    `json:"status"`
	URL    string `json:"url"`
}

func (o *Ops) WaitHTTPReady(ctx context.Context, targetURL string, timeout, interval time.Duration) (WaitReadyResult, error) {
	deadline := time.Now().Add(timeout)
	lastErr := ""
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return WaitReadyResult{}, err
		}
		resp, err := o.httpClient().Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return WaitReadyResult{Ready: true, Status: resp.StatusCode, URL: targetURL}, nil
			}
			lastErr = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err.Error()
		}
		o.sleep(interval)
	}
	return WaitReadyResult{}, fmt.Errorf("timed out waiting for %s: %s", targetURL, lastErr)
}

func (o *Ops) WaitPublicPreview(ctx context.Context, previewURL string, timeoutSeconds int) (WaitReadyResult, error) {
	parsed, err := url.Parse(strings.TrimRight(previewURL, "/") + "/")
	if err != nil {
		return WaitReadyResult{}, err
	}
	healthURL := parsed.ResolveReference(&url.URL{Path: "healthz"}).String()
	return o.WaitHTTPReady(ctx, healthURL, time.Duration(timeoutSeconds)*time.Second, 5*time.Second)
}

type ApplyAgentJobOptions struct {
	Namespace         string
	JobName           string
	IssueNumber       string
	IssueID           string
	IssueTitle        string
	IssueURL          string
	ValidationURL     string
	BranchName        string
	ProxyIP           string
	AgentContainerTag string
	RepoSlug          string
}

type ApplyAgentJobResult struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Applied   string `json:"applied"`
}

func (o *Ops) ApplyAgentJob(ctx context.Context, opts ApplyAgentJobOptions) (ApplyAgentJobResult, error) {
	spec := AgentJobSpec(opts)
	payload, err := json.Marshal(spec)
	if err != nil {
		return ApplyAgentJobResult{}, err
	}
	out, err := o.runWithInput(ctx, string(payload), "kubectl", "apply", "-f", "-")
	if err != nil {
		return ApplyAgentJobResult{}, fmt.Errorf("kubectl apply failed: %w", err)
	}
	return ApplyAgentJobResult{Namespace: opts.Namespace, Job: opts.JobName, Applied: out}, nil
}

type WaitAgentJobResult struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Pod       string `json:"pod"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}

func (o *Ops) WaitAgentJob(ctx context.Context, namespace, jobName string, timeoutSeconds int) (WaitAgentJobResult, error) {
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)

	podName := ""
	phase := ""
	for time.Now().Before(deadline) {
		out, err := o.run(
			ctx,
			"kubectl", "-n", namespace, "get", "pods",
			"-l", "job-name="+jobName,
			"-o", "jsonpath={.items[0].metadata.name}",
		)
		if err != nil {
			return WaitAgentJobResult{}, err
		}
		podName = out
		if podName != "" {
			phase, err = o.run(
				ctx,
				"kubectl", "-n", namespace, "get", "pod", podName,
				"-o", "jsonpath={.status.phase}",
			)
			if err != nil {
				return WaitAgentJobResult{}, err
			}
			if phase == "Running" || phase == "Succeeded" || phase == "Failed" {
				break
			}
		}
		o.sleep(3 * time.Second)
	}
	if podName == "" {
		return WaitAgentJobResult{}, fmt.Errorf("agent pod for Job %q never appeared", jobName)
	}

	if o.Stdout != nil {
		fmt.Fprintf(o.Stdout, "agent pod %s (phase=%s) - streaming logs\n", podName, phase)
	}
	o.runStreamAllowFailure(ctx, "kubectl", "-n", namespace, "logs", "-f", podName)

	succeededRaw := ""
	failedRaw := ""
	for time.Now().Before(deadline) {
		var err error
		succeededRaw, err = o.run(ctx, "kubectl", "-n", namespace, "get", "job", jobName, "-o", "jsonpath={.status.succeeded}")
		if err != nil {
			return WaitAgentJobResult{}, err
		}
		failedRaw, err = o.run(ctx, "kubectl", "-n", namespace, "get", "job", jobName, "-o", "jsonpath={.status.failed}")
		if err != nil {
			return WaitAgentJobResult{}, err
		}
		if succeededRaw != "" || failedRaw != "" {
			break
		}
		o.sleep(2 * time.Second)
	}

	succeeded := atoiDefault(succeededRaw)
	failed := atoiDefault(failedRaw)
	if succeeded >= 1 {
		return WaitAgentJobResult{Namespace: namespace, Job: jobName, Pod: podName, Succeeded: succeeded, Failed: failed}, nil
	}
	return WaitAgentJobResult{}, fmt.Errorf("agent Job %q failed (succeeded=%d, failed=%d)", jobName, succeeded, failed)
}

func atoiDefault(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return value
}
