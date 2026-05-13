package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NativeLauncher creates Kubernetes resources for native k8s_job phases.
type NativeLauncher interface {
	LaunchNativePhase(ctx context.Context, req NativeLaunchRequest) ([]string, error)
}

type TestSlotPreparer interface {
	EnsureTestSlot(ctx context.Context, lease Lease) error
	ReturnTestSlot(ctx context.Context, lease Lease) error
}

type NativeLaunchRequest struct {
	Lease    Lease
	Workflow Workflow
	Phase    PhaseSpec
	Run      RunReplayData
}

type KubernetesNativeLauncher struct {
	Settings   Settings
	HTTPClient *http.Client
}

func NewKubernetesNativeLauncher(settings Settings) *KubernetesNativeLauncher {
	return &KubernetesNativeLauncher{Settings: settings}
}

func (l *KubernetesNativeLauncher) LaunchNativePhase(ctx context.Context, req NativeLaunchRequest) ([]string, error) {
	if len(req.Phase.Jobs) == 0 {
		return nil, fmt.Errorf("native phase %q has no jobs", req.Phase.Name)
	}
	attemptIndex := nativeAttemptIndex(req)
	attemptBase := compactResourceName("glim", runRefFromData(req.Run), attemptIndex)
	launched := make([]string, 0, len(req.Phase.Jobs))
	for _, job := range req.Phase.Jobs {
		if strings.TrimSpace(job.ID) == "" {
			return nil, fmt.Errorf("native phase %q has job with empty id", req.Phase.Name)
		}
		jobName := nativeJobName(attemptBase, job.ID)
		secretName := jobName + "-token"
		if _, err := l.ensureAttemptSecret(ctx, secretName, attemptBase, job.ID); err != nil {
			return nil, err
		}
		if err := l.createJob(ctx, nativeJobManifest(l.Settings, req, job, jobName, secretName, attemptBase)); err != nil {
			return nil, err
		}
		launched = append(launched, jobName)
	}
	return launched, nil
}

func (l *KubernetesNativeLauncher) EnsureTestSlot(ctx context.Context, lease Lease) error {
	if !l.Settings.NativeRunnerPlaywrightEnabled {
		return nil
	}
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	name := playwrightResourceName(lease.Project, slotName)
	if name == "" {
		return nil
	}
	labels := playwrightLabels(lease, name)
	if err := l.createDeployment(ctx, playwrightDeployment(l.Settings, name, labels)); err != nil {
		return err
	}
	return l.createService(ctx, playwrightService(l.Settings, name, labels))
}

func (l *KubernetesNativeLauncher) ReturnTestSlot(ctx context.Context, lease Lease) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	name := playwrightResourceName(lease.Project, slotName)
	if name == "" {
		return nil
	}
	for _, path := range []string{
		"/apis/apps/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/deployments/" + name,
		"/api/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/services/" + name,
	} {
		status, _, err := l.request(ctx, http.MethodDelete, path, nil)
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) ensureAttemptSecret(ctx context.Context, name, attemptBase, jobID string) (string, error) {
	token := uuid.New().String()
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": l.Settings.NativeRunnerNamespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by":         "glimmung",
				"glimmung.romaine.life/native-attempt": "true",
				"glimmung.romaine.life/attempt-base":   labelValue(attemptBase),
				"glimmung.romaine.life/job-id":         labelValue(jobID),
			},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"attempt-token": token},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/secrets", body)
	if err == nil || status == http.StatusConflict {
		if status == http.StatusConflict {
			existingStatus, existing, getErr := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/secrets/"+name, nil)
			if getErr != nil || existingStatus >= 400 {
				return "", fmt.Errorf("read existing native attempt secret: status=%d err=%w", existingStatus, getErr)
			}
			if encoded, ok := anyMap(existing["data"])["attempt-token"].(string); ok && encoded != "" {
				return encoded, nil
			}
		}
		return token, nil
	}
	return "", err
}

func (l *KubernetesNativeLauncher) createJob(ctx context.Context, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/jobs", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) createDeployment(ctx context.Context, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/apis/apps/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/deployments", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) createService(ctx context.Context, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/services", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) request(ctx context.Context, method, path string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.Settings.K8sAPIHost, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	token, err := os.ReadFile(l.Settings.K8sSATokenPath)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := l.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, Transport: l.transport()}
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
	var decoded map[string]any
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, decoded, nil
}

func (l *KubernetesNativeLauncher) transport() http.RoundTripper {
	ca, err := os.ReadFile(l.Settings.K8sCACertPath)
	if err != nil {
		return http.DefaultTransport
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return http.DefaultTransport
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
}

func nativeJobManifest(settings Settings, req NativeLaunchRequest, job NativeJobSpec, jobName, secretName, attemptBase string) map[string]any {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":       "glimmung",
		"glimmung.romaine.life/native-job":   "true",
		"glimmung.romaine.life/project":      labelValue(req.Lease.Project),
		"glimmung.romaine.life/workflow":     labelValue(req.Workflow.Name),
		"glimmung.romaine.life/run-ref":      labelValue(runRefFromData(req.Run)),
		"glimmung.romaine.life/phase":        labelValue(req.Phase.Name),
		"glimmung.romaine.life/attempt-base": labelValue(attemptBase),
		"glimmung.romaine.life/job-id":       labelValue(job.ID),
	}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	podLabels["azure.workload.identity/use"] = "true"
	container := map[string]any{
		"name":  dnsLabel(job.ID),
		"image": job.Image,
		"env":   nativeJobEnv(settings, req, job.ID, secretName),
		"volumeMounts": []any{
			map[string]any{"name": "glimmung-attempt-token", "mountPath": "/var/run/glimmung", "readOnly": true},
			map[string]any{"name": "codex-credentials", "mountPath": settings.NativeRunnerCodexMountPath, "readOnly": true},
		},
	}
	if len(job.Command) > 0 {
		container["command"] = job.Command
	}
	if len(job.Args) > 0 {
		container["args"] = job.Args
	}
	podSpec := map[string]any{
		"serviceAccountName": settings.NativeRunnerServiceAccount,
		"restartPolicy":      "Never",
		"volumes": []any{
			map[string]any{"name": "glimmung-attempt-token", "secret": map[string]any{"secretName": secretName}},
			map[string]any{"name": "codex-credentials", "secret": map[string]any{"secretName": settings.NativeRunnerCodexSecret, "optional": false}},
		},
		"containers": []any{container},
	}
	if job.TimeoutSeconds != nil && *job.TimeoutSeconds > 0 {
		podSpec["activeDeadlineSeconds"] = *job.TimeoutSeconds
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      jobName,
			"namespace": settings.NativeRunnerNamespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": settings.NativeRunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec":     podSpec,
			},
		},
	}
}

func nativeJobEnv(settings Settings, req NativeLaunchRequest, jobID, secretName string) []map[string]any {
	metadata := req.Lease.Metadata
	baseURL := strings.TrimRight(settings.NativeRunnerCallbackBaseURL, "/")
	callback := ""
	if req.Run.CallbackToken != nil {
		callback = *req.Run.CallbackToken
	}
	nativePath := "/v1/run-callbacks/" + callback + "/native"
	env := []map[string]any{
		{"name": "GLIMMUNG_BASE_URL", "value": baseURL},
		{"name": "GLIMMUNG_PROJECT", "value": req.Lease.Project},
		{"name": "GLIMMUNG_WORKFLOW", "value": req.Workflow.Name},
		{"name": "GLIMMUNG_PHASE", "value": req.Phase.Name},
		{"name": "GLIMMUNG_RUN_ID", "value": req.Run.ID},
		{"name": "GLIMMUNG_RUN_REF", "value": runRefFromData(req.Run)},
		{"name": "GLIMMUNG_JOB_ID", "value": jobID},
		{"name": "GLIMMUNG_ATTEMPT_INDEX", "value": strconv.Itoa(nativeAttemptIndex(req))},
		{"name": "GLIMMUNG_LEASE_REF", "value": leasePublicRef(req.Lease)},
		{"name": "GLIMMUNG_EVENTS_URL", "value": baseURL + nativePath + "/events"},
		{"name": "GLIMMUNG_STATUS_URL", "value": baseURL + nativePath + "/status"},
		{"name": "GLIMMUNG_COMPLETED_URL", "value": baseURL + nativePath + "/completed"},
		{"name": "GLIMMUNG_FAILED_URL", "value": baseURL + nativePath + "/failed"},
		{"name": "GLIMMUNG_GITHUB_TOKEN_URL", "value": baseURL + nativePath + "/github-token"},
		{"name": "GLIMMUNG_ATTEMPT_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "attempt-token"}}},
	}
	for _, key := range []string{"issue_repo", "issue_number", "issue_title", "issue_body", "native_slot_index", "native_slot_name", "work_context_id", "work_context_branch", "work_context_base_ref", "work_context_state"} {
		if value, ok := metadata[key]; ok {
			env = append(env, map[string]any{"name": "GLIMMUNG_" + envName(key), "value": fmt.Sprint(value)})
		}
	}
	if phaseInputs := anyMap(metadata["phase_inputs"]); len(phaseInputs) > 0 {
		for k, v := range phaseInputs {
			env = append(env, map[string]any{"name": "GLIMMUNG_INPUT_" + envName(k), "value": fmt.Sprint(v)})
		}
	}
	if settings.NativeRunnerPlaywrightEnabled {
		if slotName, ok := metadata["native_slot_name"].(string); ok && slotName != "" {
			endpoint := fmt.Sprintf("ws://glim-pw-%s.%s.svc.cluster.local:%s", dnsLabel(req.Lease.Project+"-"+slotName), settings.NativeRunnerNamespace, settings.NativeRunnerPlaywrightPort)
			env = append(env,
				map[string]any{"name": "GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT", "value": endpoint},
				map[string]any{"name": "PLAYWRIGHT_WS_ENDPOINT", "value": endpoint},
				map[string]any{"name": "PW_TEST_CONNECT_WS_ENDPOINT", "value": endpoint},
			)
		}
	}
	return env
}

func playwrightResourceName(project, slotName string) string {
	if strings.TrimSpace(slotName) == "" {
		return ""
	}
	return compactResourceName("glim-pw", project+"-"+slotName, 0)
}

func playwrightLabels(lease Lease, name string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":           "glimmung",
		"app.kubernetes.io/part-of":              "glimmung-native-runner",
		"app.kubernetes.io/name":                 name,
		"glimmung.romaine.life/slot-playwright":  "true",
		"glimmung.romaine.life/project":          labelValue(lease.Project),
		"glimmung.romaine.life/native-slot-name": labelValue(mapStringValueOrEmpty(lease.Metadata, "native_slot_name")),
	}
	if value := mapStringValueOrEmpty(lease.Metadata, "native_slot_index"); value != "" {
		labels["glimmung.romaine.life/native-slot-index"] = labelValue(value)
	}
	if ref := LeasePublicRefFromLease(lease); ref != "" {
		labels["glimmung.romaine.life/lease-ref"] = labelValue(ref)
	}
	return labels
}

func playwrightDeployment(settings Settings, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.NativeRunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": settings.NativeRunnerNamespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":  "playwright",
						"image": settings.NativeRunnerPlaywrightImage,
						"command": []string{
							"npx", "playwright", "run-server",
							"--host", "0.0.0.0",
							"--port", strconv.Itoa(port),
						},
						"ports": []any{map[string]any{"name": "ws", "containerPort": port}},
						"env":   []any{map[string]any{"name": "PLAYWRIGHT_BROWSERS_PATH", "value": "/ms-playwright"}},
						"resources": map[string]any{
							"requests": map[string]string{"cpu": "100m", "memory": "256Mi"},
							"limits":   map[string]string{"cpu": "1000m", "memory": "1Gi"},
						},
					}},
				},
			},
		},
	}
}

func playwrightService(settings Settings, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.NativeRunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      name,
			"namespace": settings.NativeRunnerNamespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"selector": map[string]string{"app.kubernetes.io/name": name},
			"ports":    []any{map[string]any{"name": "ws", "port": port, "targetPort": "ws"}},
		},
	}
}

func mapStringValueOrEmpty(values map[string]any, key string) string {
	value, _ := stringFromMap(values, key)
	return value
}

func nativeAttemptIndex(req NativeLaunchRequest) int {
	if v, ok := req.Lease.Metadata["attempt_index"]; ok {
		if n, err := strconv.Atoi(fmt.Sprint(v)); err == nil && n >= 0 {
			return n
		}
	}
	if len(req.Run.Attempts) > 0 {
		return req.Run.Attempts[len(req.Run.Attempts)-1].AttemptIndex
	}
	return 0
}

func nativeJobName(attemptBase, jobID string) string {
	suffix := dnsLabel(jobID)
	candidate := attemptBase + "-" + suffix
	if len(candidate) <= 63 {
		return strings.Trim(candidate, "-")
	}
	hash := sha256.Sum256([]byte(jobID))
	head := strings.TrimRight(attemptBase[:min(len(attemptBase), 54)], "-")
	return head + "-" + hex.EncodeToString(hash[:])[:8]
}

var nonDNSLabel = regexp.MustCompile(`[^a-z0-9-]+`)
var nonEnvName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func dnsLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = nonDNSLabel.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "job"
	}
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func labelValue(value string) string {
	value = dnsLabel(value)
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func envName(value string) string {
	value = strings.ToUpper(nonEnvName.ReplaceAllString(value, "_"))
	return strings.Trim(value, "_")
}

func compactResourceName(prefix, value string, attemptIndex int) string {
	hash := sha256.Sum256([]byte(value))
	base := dnsLabel(prefix + "-" + value + "-" + strconv.Itoa(attemptIndex))
	if len(base) <= 63 {
		return base
	}
	return dnsLabel(prefix + "-" + hex.EncodeToString(hash[:])[:16] + "-" + strconv.Itoa(attemptIndex))
}

func anyMap(raw any) map[string]any {
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	return map[string]any{}
}
