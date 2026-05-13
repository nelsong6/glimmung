package agentops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

type HotSwapOptions struct {
	Contract           hotswap.Contract
	Namespace          string
	Pod                string
	Selector           string
	Container          string
	StaticOnly         bool
	BackendOnly        bool
	HealthBaseURL      string
	HealthTimeout      time.Duration
	HealthInterval     time.Duration
	ChangedFiles       []string
	AllowImageRequired bool
}

type HotSwapResult struct {
	Namespace            string                       `json:"namespace"`
	Pod                  string                       `json:"pod"`
	Pods                 []string                     `json:"pods,omitempty"`
	Container            string                       `json:"container,omitempty"`
	Static               *HotSwapStaticResult         `json:"static,omitempty"`
	Backend              *HotSwapBackendResult        `json:"backend,omitempty"`
	Health               *HotSwapHealthResult         `json:"health,omitempty"`
	Timings              map[string]string            `json:"timings"`
	ChangeClassification hotswap.ChangeClassification `json:"change_classification"`
}

type HotSwapStaticResult struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type HotSwapBackendResult struct {
	BuildCommand     string   `json:"build_command"`
	BuildExitCode    int      `json:"build_exit_code"`
	BuildStdoutTail  string   `json:"build_stdout_tail,omitempty"`
	BuildStderrTail  string   `json:"build_stderr_tail,omitempty"`
	Artifact         string   `json:"artifact"`
	Target           string   `json:"target"`
	NextTarget       string   `json:"next_target"`
	CopyContainer    string   `json:"copy_container,omitempty"`
	RestartContainer string   `json:"restart_container,omitempty"`
	RestartCommand   []string `json:"restart_command"`
	PostRestartLogs  string   `json:"post_restart_logs,omitempty"`
	PostRestartError string   `json:"post_restart_error,omitempty"`
}

type HotSwapHealthResult struct {
	URL          string `json:"url"`
	StatusCode   int    `json:"status_code"`
	ResponseTail string `json:"response_tail,omitempty"`
}

func (o *Ops) TestSlotHotSwap(ctx context.Context, opts HotSwapOptions) (HotSwapResult, error) {
	if err := opts.Contract.Validate(); err != nil {
		return HotSwapResult{}, err
	}
	if !opts.Contract.Enabled {
		return HotSwapResult{}, fmt.Errorf("test slot hot swap is not enabled")
	}
	if strings.TrimSpace(opts.Namespace) == "" {
		return HotSwapResult{}, fmt.Errorf("namespace is required")
	}
	if opts.StaticOnly && opts.BackendOnly {
		return HotSwapResult{}, fmt.Errorf("static-only and backend-only cannot both be set")
	}
	classification := hotswap.ClassifyPaths(opts.ChangedFiles)
	if classification.NeedsImage && !opts.AllowImageRequired {
		return HotSwapResult{Namespace: opts.Namespace, Container: opts.Container, Timings: map[string]string{}, ChangeClassification: classification}, fmt.Errorf("change requires image rebuild; refusing hot swap without allow-image-required")
	}
	start := time.Now()
	result := HotSwapResult{Namespace: opts.Namespace, Container: opts.Container, Timings: map[string]string{}, ChangeClassification: classification}
	pods := []string{strings.TrimSpace(opts.Pod)}
	if pods[0] == "" {
		var err error
		pods, err = o.resolveHotSwapPods(ctx, opts.Namespace, opts.Selector)
		if err != nil {
			return result, err
		}
	}
	result.Pod = pods[0]
	result.Pods = pods
	staticEnabled := opts.Contract.Static.Enabled && !opts.BackendOnly
	backendEnabled := opts.Contract.Backend.Enabled && !opts.StaticOnly
	if staticEnabled {
		stepStart := time.Now()
		for _, pod := range pods {
			if err := o.copyToPod(ctx, opts.Namespace, pod, opts.Container, staticCopySource(o.repoRoot(), opts.Contract.Static.Source), opts.Contract.Static.Target); err != nil {
				return result, err
			}
		}
		result.Static = &HotSwapStaticResult{Source: opts.Contract.Static.Source, Target: opts.Contract.Static.Target}
		result.Timings["static_copy"] = time.Since(stepStart).String()
	}
	if backendEnabled {
		stepStart := time.Now()
		for _, pod := range pods {
			backend, err := o.hotSwapBackend(ctx, opts, pod)
			result.Backend = &backend
			result.Timings["backend_swap"] = time.Since(stepStart).String()
			if err != nil {
				return result, err
			}
		}
		if strings.TrimSpace(opts.HealthBaseURL) != "" {
			healthStart := time.Now()
			health, err := o.pollHotSwapHealth(ctx, opts.HealthBaseURL, opts.Contract.Backend.HealthPath, opts.HealthTimeout, opts.HealthInterval)
			result.Health = &health
			result.Timings["health"] = time.Since(healthStart).String()
			if err != nil {
				return result, err
			}
		}
	}
	result.Timings["total"] = time.Since(start).String()
	return result, nil
}

func (o *Ops) resolveHotSwapPods(ctx context.Context, namespace, selector string) ([]string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("pod or selector is required")
	}
	out, err := o.run(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	pods := strings.Fields(out)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pod matched selector %q in namespace %q", selector, namespace)
	}
	return pods, nil
}

func (o *Ops) hotSwapBackend(ctx context.Context, opts HotSwapOptions, pod string) (HotSwapBackendResult, error) {
	backend := opts.Contract.Backend
	build := HotSwapBackendResult{
		BuildCommand:     backend.BuildCommand,
		Artifact:         backend.Artifact,
		Target:           backend.Target,
		NextTarget:       backend.Target + ".next",
		CopyContainer:    firstNonEmptyString(backend.CopyContainer, opts.Container),
		RestartContainer: firstNonEmptyString(backend.RestartContainer, opts.Container),
		RestartCommand:   backend.RestartCommand,
	}
	if len(build.RestartCommand) == 0 {
		build.RestartCommand = []string{"sh", "-c", "kill -HUP 1"}
	}
	buildResult, err := o.runner().Run(ctx, Command{
		Name:         "sh",
		Args:         []string{"-c", backend.BuildCommand},
		Cwd:          o.repoRoot(),
		AllowFailure: true,
	})
	build.BuildExitCode = buildResult.ExitCode
	build.BuildStdoutTail = tailString(buildResult.Stdout, 4000)
	build.BuildStderrTail = tailString(buildResult.Stderr, 4000)
	if err != nil {
		return build, err
	}
	if buildResult.ExitCode != 0 {
		return build, fmt.Errorf("backend build failed with exit code %d", buildResult.ExitCode)
	}
	if _, statErr := os.Stat(backend.Artifact); statErr != nil {
		return build, fmt.Errorf("backend artifact %q is not readable: %w", backend.Artifact, statErr)
	}
	if err := o.copyToPod(ctx, opts.Namespace, pod, build.CopyContainer, backend.Artifact, build.NextTarget); err != nil {
		return build, err
	}
	if _, err := o.kubectlExec(ctx, opts.Namespace, pod, build.CopyContainer, "sh", "-c", "chmod +x "+shellQuote(build.NextTarget)+" && mv -f "+shellQuote(build.NextTarget)+" "+shellQuote(backend.Target)); err != nil {
		return build, err
	}
	if _, err := o.kubectlExec(ctx, opts.Namespace, pod, build.RestartContainer, build.RestartCommand...); err != nil {
		return build, err
	}
	logs := o.hotSwapLogs(ctx, opts.Namespace, pod, build.RestartContainer)
	build.PostRestartLogs = tailString(logs.Stdout, 4000)
	build.PostRestartError = tailString(logs.Stderr, 1000)
	return build, nil
}

func (o *Ops) copyToPod(ctx context.Context, namespace, pod, container, source, target string) error {
	args := []string{"-n", namespace, "cp", source, pod + ":" + target}
	if strings.TrimSpace(container) != "" {
		args = append(args, "-c", container)
	}
	_, err := o.run(ctx, "kubectl", args...)
	return err
}

func (o *Ops) kubectlExec(ctx context.Context, namespace, pod, container string, command ...string) (string, error) {
	args := []string{"-n", namespace, "exec", pod}
	if strings.TrimSpace(container) != "" {
		args = append(args, "-c", container)
	}
	args = append(args, "--")
	args = append(args, command...)
	return o.run(ctx, "kubectl", args...)
}

func (o *Ops) pollHotSwapHealth(ctx context.Context, baseURL, healthPath string, timeout, interval time.Duration) (HotSwapHealthResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	url := strings.TrimRight(baseURL, "/") + healthPath
	deadline := time.Now().Add(timeout)
	var last HotSwapHealthResult
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return last, err
		}
		resp, err := o.httpClient().Do(req)
		if err == nil {
			body, _ := ioReadAllLimited(resp.Body, 4096)
			_ = resp.Body.Close()
			last = HotSwapHealthResult{URL: url, StatusCode: resp.StatusCode, ResponseTail: tailString(string(body), 1000)}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return last, nil
			}
		} else {
			last = HotSwapHealthResult{URL: url, ResponseTail: err.Error()}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("health check did not pass before timeout")
		}
		o.sleep(interval)
	}
}

func staticSourcePath(root, source string) string {
	if filepath.IsAbs(source) {
		return source
	}
	return filepath.Join(root, source)
}

func staticCopySource(root, source string) string {
	path := staticSourcePath(root, source)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Join(path, ".")
	}
	return path
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (o *Ops) hotSwapLogs(ctx context.Context, namespace, pod, container string) Result {
	args := []string{"-n", namespace, "logs", pod, "--tail=120"}
	if strings.TrimSpace(container) != "" {
		args = append(args, "-c", container)
	}
	result, _ := o.runner().Run(ctx, Command{Name: "kubectl", Args: args, Cwd: o.repoRoot(), AllowFailure: true})
	return result
}

func tailString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ioReadAllLimited(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(reader)
	}
	return io.ReadAll(io.LimitReader(reader, limit))
}
