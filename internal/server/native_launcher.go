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
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	testSlotInstallerJobPrefix    = "glim-slot-apply"
	testSlotInstallerSecretPrefix = "glim-slot-clone"
	testSlotUninstallJobPrefix    = "glim-slot-uninstall"
	testSlotRenderModeWarm        = "warm"
	testSlotRenderModeHot         = "hot"
)

// NativeLauncher creates Kubernetes resources for native k8s_job phases.
type NativeLauncher interface {
	LaunchNativePhase(ctx context.Context, req NativeLaunchRequest) ([]string, error)
}

type TestSlotPreparer interface {
	EnsureTestSlotPreliminaries(ctx context.Context, lease Lease, project Project) error
	ActivateTestSlotRuntime(ctx context.Context, lease Lease, project Project, minter NativeGitHubTokenMinter) error
	ReturnTestSlotRuntime(ctx context.Context, lease Lease, project Project) error
	DeprovisionTestSlot(ctx context.Context, lease Lease, project Project) error
}

type TestSlotInstallerCleaner interface {
	CleanupTestSlotInstaller(ctx context.Context, lease Lease, project Project) error
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
	if err := l.ensurePlaywrightForNativePhase(ctx, req); err != nil {
		return nil, err
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

func (l *KubernetesNativeLauncher) EnsureTestSlotPreliminaries(ctx context.Context, lease Lease, project Project) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	if strings.TrimSpace(slotName) == "" {
		return nil
	}
	return l.ensureTestSlotPreliminaryAccess(ctx, lease, project, strings.TrimSpace(slotName))
}

func (l *KubernetesNativeLauncher) ensureTestSlotPreliminaryAccess(ctx context.Context, lease Lease, project Project, slotName string) error {
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if err := l.ensureNamespace(ctx, slotName, testSlotLabels(lease, slotName)); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
		if err := l.ensureNamespace(ctx, sessionsNamespace, testSlotLabels(lease, slotName)); err != nil {
			return err
		}
		if err := l.ensureTestSlotInstallerAccess(ctx, lease, slotName); err != nil {
			return err
		}
		if err := l.ensureTestSlotInstallerAccess(ctx, lease, sessionsNamespace); err != nil {
			return err
		}
		substitutions := testSlotSubstitutions(lease, project, slotName, sessionsNamespace)
		if err := l.ensureTestSlotClusterRoleBindings(ctx, lease, config.ClusterRoleBindings, substitutions, slotName); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) ActivateTestSlotRuntime(ctx context.Context, lease Lease, project Project, minter NativeGitHubTokenMinter) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if err := l.ensureTestSlotPreliminaryAccess(ctx, lease, project, slotName); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		if strings.TrimSpace(project.GitHubRepo) == "" {
			return fmt.Errorf("github_repo is required for test slot runtime activation")
		}
		if minter == nil {
			return fmt.Errorf("github token minter is required for test slot runtime activation")
		}
		if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeWarm); err != nil {
			return err
		}
		if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeHot); err != nil {
			return err
		}
	}
	return l.ensurePlaywrightForSlot(ctx, lease, slotName, true)
}

func (l *KubernetesNativeLauncher) ReturnTestSlotRuntime(ctx context.Context, lease Lease, project Project) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	if strings.TrimSpace(slotName) == "" {
		return nil
	}
	// 1. Delete the installer Job + clone Secret first. The Job's pods run
	//    `helm install`, which creates the slot-scoped Deployments / Pods /
	//    Services we're about to delete. If we deleted those outputs first
	//    while the Job was still running, helm would recreate them faster
	//    than we could delete them and waitForNoPodsInNamespaces would spin
	//    until its 5-minute timeout fired — the failure mode that previously
	//    stranded the slot in `error`. See docs/test-slot-lifecycle.md.
	if err := l.deleteTestSlotInstaller(ctx, lease); err != nil {
		return err
	}
	// 2. Wait for the installer Job's pods to terminate. The helm-install
	//    process lives in the pod; once the pod is gone, helm cannot create
	//    further K8s objects. Background propagation on the Job DELETE marks
	//    its pods for GC; we poll until the GC catches up so the next steps
	//    don't race the helm tail.
	if err := l.waitForInstallerPodsBySlotTerminated(ctx, l.Settings.NativeRunnerNamespace, slotName, 90*time.Second); err != nil {
		return err
	}
	if config, ok := testSlotHelmConfig(project); ok {
		if err := l.runTestSlotHelmUninstall(ctx, lease, project, config, testSlotRenderModeHot); err != nil {
			return err
		}
		if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
			return err
		}
		if err := l.deleteTestSlotRuntimeWorkloads(ctx, lease, project, slotName); err != nil {
			return err
		}
		return nil
	}
	// 3. Delete the lease-scoped Playwright Deployment and Service. Cleanup
	//    creates these via ensurePlaywrightForSlot during activation; the
	//    in-process activation goroutine that creates them has already been
	//    cancelled and awaited by beginTestSlotCleanup before reaching this
	//    function — so this delete sees a static target.
	if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
		return err
	}
	// 4. Delete the remaining slot-namespace workloads (project app
	//    Deployments, Services, leftover Jobs/Pods) and wait for every pod
	//    in the slot namespace(s) to terminate. With the installer Job dead
	//    and the activation goroutine unwound, no producer is left racing
	//    this final sweep.
	if err := l.deleteTestSlotRuntimeResources(ctx, lease, project, slotName); err != nil {
		return err
	}
	return nil
}

// waitForInstallerPodsTerminated polls for the helm-install pod(s) spawned
// by the installer Job to be gone. The Job's DELETE call (with Background
// propagation) marks the pods for GC; this poll waits for the GC to catch
// up so the cleanup path that follows doesn't race the helm tail still
// running inside an undead pod. Selector is the standard K8s job-name
// label that controller-runtime attaches to Job pods.
func (l *KubernetesNativeLauncher) waitForInstallerPodsTerminated(ctx context.Context, namespace, jobName string, timeout time.Duration) error {
	namespace = strings.TrimSpace(namespace)
	jobName = strings.TrimSpace(jobName)
	if namespace == "" || jobName == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	selector := "job-name=" + labelValue(jobName)
	path := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + url.QueryEscape(selector)
	for {
		status, list, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				return nil
			}
			return err
		}
		if len(anySlice(list["items"])) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("installer job %s/%s pods did not terminate within %s", namespace, jobName, timeout)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) waitForInstallerPodsBySlotTerminated(ctx context.Context, namespace, slotName string, timeout time.Duration) error {
	namespace = strings.TrimSpace(namespace)
	slotName = strings.TrimSpace(slotName)
	if namespace == "" || slotName == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	selector := "glimmung.romaine.life/native-slot-name=" + labelValue(slotName)
	path := "/api/v1/namespaces/" + namespace + "/pods?labelSelector=" + url.QueryEscape(selector)
	for {
		status, list, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				return nil
			}
			return err
		}
		if len(anySlice(list["items"])) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("installer pods for slot %s/%s did not terminate within %s", namespace, slotName, timeout)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) CleanupTestSlotInstaller(ctx context.Context, lease Lease, _ Project) error {
	return l.deleteTestSlotInstaller(ctx, lease)
}

func (l *KubernetesNativeLauncher) runTestSlotHelmReconcile(ctx context.Context, lease Lease, project Project, minter NativeGitHubTokenMinter, config testSlotHelmSettings, renderMode string) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	if minter == nil {
		return fmt.Errorf("github token minter is required for test slot helm %s", renderMode)
	}
	sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
	substitutions := testSlotSubstitutions(lease, project, slotName, sessionsNamespace)
	token, err := minter.InstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("mint github token for test slot %s install: %w", renderMode, err)
	}
	secretName := testSlotHelmSecretName(lease, renderMode)
	if err := l.ensureCloneTokenSecret(ctx, secretName, token, lease, slotName); err != nil {
		return err
	}
	jobName := testSlotHelmJobName(lease, renderMode)
	if err := l.createJob(ctx, testSlotInstallJobManifest(l.Settings, config, lease, project, substitutions, renderMode)); err != nil {
		return err
	}
	return l.waitForJobComplete(ctx, l.Settings.NativeRunnerNamespace, jobName, 5*time.Minute)
}

func (l *KubernetesNativeLauncher) runTestSlotHelmUninstall(ctx context.Context, lease Lease, project Project, config testSlotHelmSettings, renderMode string) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	jobName := testSlotHelmUninstallJobName(lease, renderMode)
	if err := l.createJob(ctx, testSlotUninstallJobManifest(l.Settings, config, lease, project, renderMode)); err != nil {
		return err
	}
	return l.waitForJobComplete(ctx, l.Settings.NativeRunnerNamespace, jobName, 5*time.Minute)
}

func (l *KubernetesNativeLauncher) DeprovisionTestSlot(ctx context.Context, lease Lease, project Project) error {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	if strings.TrimSpace(slotName) == "" {
		return nil
	}
	if err := l.deleteTestSlotInstaller(ctx, lease); err != nil {
		return err
	}
	if err := l.deletePlaywrightResources(ctx, lease, slotName); err != nil {
		return err
	}
	if err := l.deleteTestSlotClusterRoleBindings(ctx, slotName); err != nil {
		return err
	}
	sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName)
	if strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		if err := l.deleteNamespaceAndWait(ctx, sessionsNamespace); err != nil {
			return err
		}
	}
	return l.deleteNamespaceAndWait(ctx, slotName)
}

func (l *KubernetesNativeLauncher) deletePlaywrightResources(ctx context.Context, lease Lease, slotName string) error {
	for _, target := range playwrightResourceTargets(lease, slotName) {
		for _, path := range []string{
			"/apis/apps/v1/namespaces/" + target.namespace + "/deployments/" + target.name,
			"/api/v1/namespaces/" + target.namespace + "/services/" + target.name,
		} {
			status, _, err := l.request(ctx, http.MethodDelete, path, deleteOptions("Background"))
			if err != nil && status != http.StatusNotFound {
				return err
			}
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteTestSlotRuntimeResources(ctx context.Context, lease Lease, project Project, slotName string) error {
	namespaces := []string{slotName}
	if sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName); strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		namespaces = append(namespaces, sessionsNamespace)
	}
	for _, namespace := range namespaces {
		for _, collectionPath := range []string{
			"/apis/apps/v1/namespaces/" + namespace + "/deployments",
			"/apis/apps/v1/namespaces/" + namespace + "/statefulsets",
			"/apis/apps/v1/namespaces/" + namespace + "/daemonsets",
			"/apis/batch/v1/namespaces/" + namespace + "/jobs",
			"/api/v1/namespaces/" + namespace + "/pods",
			"/api/v1/namespaces/" + namespace + "/services",
		} {
			if err := l.deleteCollectionItems(ctx, collectionPath); err != nil {
				return err
			}
		}
	}
	if err := l.waitForNoPodsInNamespaces(ctx, namespaces, 5*time.Minute); err != nil {
		return err
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteTestSlotRuntimeWorkloads(ctx context.Context, lease Lease, project Project, slotName string) error {
	namespaces := []string{slotName}
	if sessionsNamespace := testSlotSessionsNamespaceForLease(lease, project, slotName); strings.TrimSpace(sessionsNamespace) != "" && sessionsNamespace != slotName {
		namespaces = append(namespaces, sessionsNamespace)
	}
	for _, namespace := range namespaces {
		for _, collectionPath := range []string{
			"/apis/apps/v1/namespaces/" + namespace + "/deployments",
			"/apis/apps/v1/namespaces/" + namespace + "/statefulsets",
			"/apis/apps/v1/namespaces/" + namespace + "/daemonsets",
			"/apis/batch/v1/namespaces/" + namespace + "/jobs",
			"/api/v1/namespaces/" + namespace + "/pods",
		} {
			if err := l.deleteCollectionItems(ctx, collectionPath); err != nil {
				return err
			}
		}
	}
	return l.waitForNoPodsInNamespaces(ctx, namespaces, 5*time.Minute)
}

func (l *KubernetesNativeLauncher) deleteCollectionItems(ctx context.Context, collectionPath string) error {
	status, list, err := l.request(ctx, http.MethodGet, collectionPath, nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" || name == "kubernetes" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, collectionPath+"/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) waitForNoPodsInNamespaces(ctx context.Context, namespaces []string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		remaining, err := l.remainingPodsInNamespaces(ctx, namespaces)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime pods did not terminate before cleanup timeout: %s", strings.Join(remaining, ", "))
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) remainingPodsInNamespaces(ctx context.Context, namespaces []string) ([]string, error) {
	var remaining []string
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		status, list, err := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/pods", nil)
		if err != nil {
			if status == http.StatusNotFound || status == http.StatusForbidden {
				continue
			}
			return nil, err
		}
		for _, item := range anySlice(list["items"]) {
			name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
			if name == "" {
				continue
			}
			remaining = append(remaining, namespace+"/"+name)
		}
	}
	sort.Strings(remaining)
	return remaining, nil
}

func (l *KubernetesNativeLauncher) ensurePlaywrightForNativePhase(ctx context.Context, req NativeLaunchRequest) error {
	slotName, _ := stringFromMap(req.Lease.Metadata, "native_slot_name")
	slotName = strings.TrimSpace(slotName)
	return l.ensurePlaywrightForSlot(ctx, req.Lease, slotName, false)
}

func (l *KubernetesNativeLauncher) ensurePlaywrightForSlot(ctx context.Context, lease Lease, slotName string, waitForReady bool) error {
	if !l.Settings.NativeRunnerPlaywrightEnabled {
		return nil
	}
	slotName = strings.TrimSpace(slotName)
	if slotName == "" {
		return nil
	}
	name := playwrightResourceName(lease.Project, slotName)
	if name == "" {
		return nil
	}
	labels := playwrightLabels(lease, name)
	if err := l.createDeploymentInNamespace(ctx, slotName, playwrightDeployment(l.Settings, slotName, name, labels)); err != nil {
		return err
	}
	if err := l.createServiceInNamespace(ctx, slotName, playwrightService(l.Settings, slotName, name, labels)); err != nil {
		return err
	}
	if !waitForReady {
		return nil
	}
	return l.waitForDeploymentReady(ctx, slotName, name, 2*time.Minute)
}

func (l *KubernetesNativeLauncher) ensureNamespace(ctx context.Context, name string, labels map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   name,
			"labels": labels,
		},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces", body)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) deleteNamespace(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	status, _, err := l.request(ctx, http.MethodDelete, "/api/v1/namespaces/"+name, deleteOptions("Background"))
	if err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteNamespaceAndWait(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if err := l.deleteNamespace(ctx, name); err != nil {
		return err
	}
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		status, _, err := l.request(ctx, http.MethodGet, "/api/v1/namespaces/"+name, nil)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("namespace %s deletion did not complete before deprovision", name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) ensureTestSlotInstallerAccess(ctx context.Context, lease Lease, namespace string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	body := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata": map[string]any{
			"name":      "glim-test-slot-installer",
			"namespace": namespace,
			"labels":    testSlotLabels(lease, slotName),
		},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     firstNonEmpty(l.Settings.NativeRunnerNamespaceRole, "cluster-admin"),
		},
		"subjects": []any{map[string]any{
			"kind":      "ServiceAccount",
			"name":      l.Settings.NativeRunnerServiceAccount,
			"namespace": l.Settings.NativeRunnerNamespace,
		}},
	}
	status, _, err := l.request(ctx, http.MethodPost, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+namespace+"/rolebindings", body)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) ensureCloneTokenSecret(ctx context.Context, name, token string, lease Lease, slotName string) error {
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": l.Settings.NativeRunnerNamespace,
			"labels":    labels,
		},
		"type":       "Opaque",
		"stringData": map[string]string{"token": token},
	}
	path := "/api/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/secrets"
	status, _, err := l.request(ctx, http.MethodPost, path, body)
	if err == nil {
		return nil
	}
	if status != http.StatusConflict {
		return err
	}
	existingStatus, existing, getErr := l.request(ctx, http.MethodGet, path+"/"+name, nil)
	if getErr != nil || existingStatus >= 400 {
		return fmt.Errorf("read existing test slot clone secret: status=%d err=%w", existingStatus, getErr)
	}
	if rv := mapStringValueOrEmpty(anyMap(existing["metadata"]), "resourceVersion"); rv != "" {
		body["metadata"].(map[string]any)["resourceVersion"] = rv
	}
	_, _, err = l.request(ctx, http.MethodPut, path+"/"+name, body)
	return err
}

func (l *KubernetesNativeLauncher) ensureTestSlotClusterRoleBindings(ctx context.Context, lease Lease, templates []map[string]any, substitutions map[string]string, slotName string) error {
	for _, template := range templates {
		filled := deepFormat(template, substitutions)
		name := strings.TrimSpace(mapStringValueOrEmpty(anyMap(filled["metadata"]), "name"))
		if name == "" {
			name = strings.TrimSpace(mapStringValueOrEmpty(filled, "name"))
		}
		if name == "" {
			continue
		}
		labels := testSlotLabels(lease, slotName)
		labels["glimmung.romaine.life/test-slot"] = "true"
		body := map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRoleBinding",
			"metadata": map[string]any{
				"name":   name,
				"labels": labels,
			},
			"subjects": filled["subjects"],
			"roleRef":  filled["roleRef"],
		}
		path := "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"
		status, _, err := l.request(ctx, http.MethodPost, path, body)
		if err == nil {
			continue
		}
		if status != http.StatusConflict {
			return err
		}
		existingStatus, existing, getErr := l.request(ctx, http.MethodGet, path+"/"+name, nil)
		if getErr != nil || existingStatus >= 400 {
			return fmt.Errorf("read existing test slot clusterrolebinding: status=%d err=%w", existingStatus, getErr)
		}
		if rv := mapStringValueOrEmpty(anyMap(existing["metadata"]), "resourceVersion"); rv != "" {
			body["metadata"].(map[string]any)["resourceVersion"] = rv
		}
		if _, _, err := l.request(ctx, http.MethodPut, path+"/"+name, body); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteTestSlotClusterRoleBindings(ctx context.Context, slotName string) error {
	selector := "glimmung.romaine.life/native-slot-name=" + labelValue(slotName)
	path := "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings?labelSelector=" + url.QueryEscape(selector)
	status, list, err := l.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/"+name, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteTestSlotInstaller(ctx context.Context, lease Lease) error {
	for _, path := range []string{
		"/apis/batch/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/jobs/" + testSlotInstallerJobName(lease),
		"/api/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/secrets/" + testSlotInstallerSecretName(lease),
		"/apis/batch/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/jobs/" + testSlotInstallResourceName("glim-helm-install", lease),
		"/api/v1/namespaces/" + l.Settings.NativeRunnerNamespace + "/secrets/" + testSlotInstallResourceName("glim-helm-clone", lease),
	} {
		status, _, err := l.request(ctx, http.MethodDelete, path, deleteOptions("Background"))
		if err != nil && status != http.StatusNotFound {
			return err
		}
	}
	if slotName, _ := stringFromMap(lease.Metadata, "native_slot_name"); strings.TrimSpace(slotName) != "" {
		if err := l.deleteRunnerResourcesBySlot(ctx, "/apis/batch/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/jobs", slotName); err != nil {
			return err
		}
		if err := l.deleteRunnerResourcesBySlot(ctx, "/api/v1/namespaces/"+l.Settings.NativeRunnerNamespace+"/secrets", slotName); err != nil {
			return err
		}
	}
	return nil
}

func (l *KubernetesNativeLauncher) deleteRunnerResourcesBySlot(ctx context.Context, collectionPath, slotName string) error {
	selector := "glimmung.romaine.life/native-slot-name=" + labelValue(slotName)
	status, list, err := l.request(ctx, http.MethodGet, collectionPath+"?labelSelector="+url.QueryEscape(selector), nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil
		}
		return err
	}
	for _, item := range anySlice(list["items"]) {
		name := mapStringValueOrEmpty(anyMap(anyMap(item)["metadata"]), "name")
		if name == "" {
			continue
		}
		status, _, err := l.request(ctx, http.MethodDelete, collectionPath+"/"+name, deleteOptions("Background"))
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

func (l *KubernetesNativeLauncher) waitForJobComplete(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + name
	for {
		status, job, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound {
				return fmt.Errorf("test slot runtime installer job %s/%s not found", namespace, name)
			}
			return err
		}
		for _, condition := range anySlice(anyMap(job["status"])["conditions"]) {
			typed := anyMap(condition)
			conditionType := strings.TrimSpace(mapStringValueOrEmpty(typed, "type"))
			conditionStatus := strings.TrimSpace(mapStringValueOrEmpty(typed, "status"))
			if conditionType == "Complete" && strings.EqualFold(conditionStatus, "True") {
				return nil
			}
			if conditionType == "Failed" && strings.EqualFold(conditionStatus, "True") {
				return fmt.Errorf("test slot runtime installer job %s/%s failed", namespace, name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime installer job %s/%s did not complete before timeout", namespace, name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) waitForDeploymentReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	path := "/apis/apps/v1/namespaces/" + namespace + "/deployments/" + name
	for {
		status, deployment, err := l.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			if status == http.StatusNotFound {
				return fmt.Errorf("test slot runtime deployment %s/%s not found", namespace, name)
			}
			return err
		}
		statusMap := anyMap(deployment["status"])
		readyReplicas, _ := positiveIntFromMap(statusMap, "readyReplicas")
		availableReplicas, _ := positiveIntFromMap(statusMap, "availableReplicas")
		if readyReplicas > 0 && availableReplicas > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("test slot runtime deployment %s/%s did not become ready before timeout", namespace, name)
		case <-ticker.C:
		}
	}
}

func (l *KubernetesNativeLauncher) createDeploymentInNamespace(ctx context.Context, namespace string, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/apis/apps/v1/namespaces/"+namespace+"/deployments", manifest)
	if err == nil || status == http.StatusConflict {
		return nil
	}
	return err
}

func (l *KubernetesNativeLauncher) createServiceInNamespace(ctx context.Context, namespace string, manifest map[string]any) error {
	status, _, err := l.request(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/services", manifest)
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
		"env":   nativeJobEnv(settings, req, job, secretName),
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

func nativeJobEnv(settings Settings, req NativeLaunchRequest, job NativeJobSpec, secretName string) []map[string]any {
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
		{"name": "GLIMMUNG_JOB_ID", "value": job.ID},
		{"name": "GLIMMUNG_ATTEMPT_INDEX", "value": strconv.Itoa(nativeAttemptIndex(req))},
		{"name": "GLIMMUNG_LEASE_REF", "value": leasePublicRef(req.Lease)},
		{"name": "GLIMMUNG_EVENTS_URL", "value": baseURL + nativePath + "/events"},
		{"name": "GLIMMUNG_STATUS_URL", "value": baseURL + nativePath + "/status"},
		{"name": "GLIMMUNG_COMPLETED_URL", "value": baseURL + nativePath + "/completed"},
		{"name": "GLIMMUNG_GITHUB_TOKEN_URL", "value": baseURL + nativePath + "/github-token"},
		{"name": "GLIMMUNG_ATTEMPT_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "attempt-token"}}},
	}
	seen := envNameSet(env)
	for _, key := range []string{"issue_repo", "issue_number", "issue_title", "issue_body", "native_slot_index", "native_slot_name", "entrypoint_job_id", "entrypoint_step_slug", "work_context_id", "work_context_branch", "work_context_base_ref", "work_context_state"} {
		if value, ok := metadata[key]; ok {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_"+envName(key), fmt.Sprint(value))
		}
	}
	if slotName := mapStringValueOrEmpty(metadata, "native_slot_name"); slotName != "" {
		env = appendLiteralEnv(env, seen, "GLIMMUNG_VALIDATION_NAMESPACE", slotName)
	}
	if phaseInputs := anyMap(metadata["phase_inputs"]); len(phaseInputs) > 0 {
		keys := make([]string, 0, len(phaseInputs))
		for key := range phaseInputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			env = appendLiteralEnv(env, seen, "GLIMMUNG_INPUT_"+envName(key), fmt.Sprint(phaseInputs[key]))
		}
	}
	if settings.NativeRunnerPlaywrightEnabled {
		if slotName, ok := metadata["native_slot_name"].(string); ok && slotName != "" {
			endpoint := fmt.Sprintf("ws://%s.%s.svc.cluster.local:%s", playwrightResourceName(req.Lease.Project, slotName), slotName, settings.NativeRunnerPlaywrightPort)
			env = appendLiteralEnv(env, seen, "GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT", endpoint)
			env = appendLiteralEnv(env, seen, "PLAYWRIGHT_WS_ENDPOINT", endpoint)
			env = appendLiteralEnv(env, seen, "PW_TEST_CONNECT_WS_ENDPOINT", endpoint)
		}
	}
	jobEnvNames := make([]string, 0, len(job.Env))
	for name := range job.Env {
		jobEnvNames = append(jobEnvNames, name)
	}
	sort.Strings(jobEnvNames)
	for _, name := range jobEnvNames {
		env = appendLiteralEnv(env, seen, name, job.Env[name])
	}
	return env
}

func envNameSet(env []map[string]any) map[string]bool {
	seen := map[string]bool{}
	for _, row := range env {
		if name, ok := row["name"].(string); ok && strings.TrimSpace(name) != "" {
			seen[name] = true
		}
	}
	return seen
}

func appendLiteralEnv(env []map[string]any, seen map[string]bool, name, value string) []map[string]any {
	name = strings.TrimSpace(name)
	if name == "" || seen[name] {
		return env
	}
	seen[name] = true
	return append(env, map[string]any{"name": name, "value": value})
}

type testSlotHelmSettings struct {
	InstallerImage      string
	ChartPath           string
	GitRef              string
	Values              map[string]string
	SetStringValues     map[string]string
	ClusterRoleBindings []map[string]any
}

const (
	defaultTestSlotInstallerImage = "alpine/k8s:1.30.0"
	defaultTestSlotChartPath      = "k8s"
)

func testSlotHelmConfig(project Project) (testSlotHelmSettings, bool) {
	raw, ok := mapFromMap(project.Metadata, "test_slot_helm")
	if !ok {
		raw, ok = mapFromMap(project.Metadata, "testSlotHelm")
	}
	if !ok || !boolConfigValue(raw, "enabled") {
		return testSlotHelmSettings{}, false
	}
	values := stringMapFromAnyMap(anyMap(raw["values"]))
	setStringValues := stringMapFromAnyMap(anyMap(firstAny(raw["set_string_values"], raw["setStringValues"])))
	clusterRoleBindings := mapSliceFromAnySlice(anySlice(firstAny(raw["cluster_role_bindings"], raw["clusterRoleBindings"])))
	if len(clusterRoleBindings) == 0 {
		clusterRoleBindings = defaultTestSlotClusterRoleBindings(project)
	}
	return testSlotHelmSettings{
		InstallerImage:      firstNonEmpty(configString(raw, "installer_image", "installerImage"), defaultTestSlotInstallerImage),
		ChartPath:           firstNonEmpty(strings.Trim(configString(raw, "chart_path", "chartPath"), "/"), defaultTestSlotChartPath),
		GitRef:              configString(raw, "git_ref", "gitRef"),
		Values:              values,
		SetStringValues:     setStringValues,
		ClusterRoleBindings: clusterRoleBindings,
	}, true
}

func defaultTestSlotClusterRoleBindings(project Project) []map[string]any {
	if project.Name != "tank-operator" && project.ID != "tank-operator" && !strings.EqualFold(project.GitHubRepo, "nelsong6/tank-operator") {
		return nil
	}
	return []map[string]any{
		{
			"metadata": map[string]any{"name": "{slot_name}-auth-delegator"},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "{slot_name}",
				"namespace": "{slot_name}",
			}},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "system:auth-delegator",
			},
		},
		{
			"metadata": map[string]any{"name": "{slot_name}-session-cluster-admin"},
			"subjects": []any{map[string]any{
				"kind":      "ServiceAccount",
				"name":      "{slot_name}-session",
				"namespace": "{sessions_namespace}",
			}},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "cluster-admin",
			},
		},
	}
}

func testSlotSubstitutions(lease Lease, project Project, slotName, sessionsNamespace string) map[string]string {
	slotIndex := mapStringValueOrEmpty(lease.Metadata, "native_slot_index")
	if slotIndex == "" {
		slotIndex = trailingSlotIndex(slotName)
	}
	host := ""
	if value := testSlotURL(project, &slotName); value != nil {
		host = strings.TrimPrefix(strings.TrimSuffix(*value, "/"), "https://")
	}
	return map[string]string{
		"slot_name":          slotName,
		"slot_index":         slotIndex,
		"sessions_namespace": sessionsNamespace,
		"host":               host,
		"project":            project.Name,
	}
}

func testSlotSessionsNamespace(slotName string, project Project) string {
	for _, key := range []string{"test_slot_helm", "testSlotHelm"} {
		if config, ok := mapFromMap(project.Metadata, key); ok {
			if value := configString(config, "sessions_namespace", "sessionsNamespace"); value != "" {
				return formatSubstitutions(value, map[string]string{"slot_name": slotName})
			}
		}
	}
	return slotName + "-sessions"
}

func testSlotSessionsNamespaceForLease(lease Lease, project Project, slotName string) string {
	if value, ok := stringFromMap(lease.Metadata, "native_sessions_namespace"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return testSlotSessionsNamespace(slotName, project)
}

func testSlotInstallResourceName(prefix string, lease Lease) string {
	ref := LeasePublicRefFromLease(lease)
	if strings.TrimSpace(ref) == "" {
		ref = lease.Project
	}
	attemptIndex := 0
	if lease.LeaseNumber != nil && *lease.LeaseNumber > 0 {
		attemptIndex = *lease.LeaseNumber
	}
	return compactResourceName(prefix, ref, attemptIndex)
}

func testSlotInstallerJobName(lease Lease) string {
	return testSlotHelmJobName(lease, testSlotRenderModeHot)
}

func testSlotInstallerSecretName(lease Lease) string {
	return testSlotHelmSecretName(lease, testSlotRenderModeHot)
}

func testSlotHelmJobName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotInstallerJobPrefix+"-"+renderMode, lease)
}

func testSlotHelmSecretName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotInstallerSecretPrefix+"-"+renderMode, lease)
}

func testSlotHelmUninstallJobName(lease Lease, renderMode string) string {
	return testSlotInstallResourceName(testSlotUninstallJobPrefix+"-"+renderMode, lease)
}

func testSlotHelmReleaseName(slotName, renderMode string) string {
	slotName = strings.TrimSpace(slotName)
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	base := dnsLabel(slotName + "-" + renderMode)
	if len(base) <= 63 {
		return base
	}
	hash := sha256.Sum256([]byte(base))
	return dnsLabel(slotName + "-" + hex.EncodeToString(hash[:])[:16])
}

func testSlotInstallJobManifest(settings Settings, config testSlotHelmSettings, lease Lease, project Project, substitutions map[string]string, renderMode string) map[string]any {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	jobName := testSlotHelmJobName(lease, renderMode)
	secretName := testSlotHelmSecretName(lease, renderMode)
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	labels["glimmung.romaine.life/render-mode"] = labelValue(renderMode)
	releaseName := testSlotHelmReleaseName(slotName, renderMode)
	gitRef := strings.TrimSpace(config.GitRef)
	cloneScript := "set -eu\n" +
		"GIT_REF=" + shellQuote(gitRef) + "\n" +
		"TOKEN=\"$(cat /var/run/glim-clone/token)\"\n" +
		"REPO_URL=\"https://x-access-token:${TOKEN}@github.com/" + project.GitHubRepo + ".git\"\n" +
		"if [ -n \"$GIT_REF\" ]; then\n" +
		"  git clone --depth 1 --branch \"$GIT_REF\" \"$REPO_URL\" /workspace\n" +
		"else\n" +
		"  git clone --depth 1 \"$REPO_URL\" /workspace\n" +
		"fi\n"
	installScript := "set -eu\n" +
		"cd /workspace\n" +
		"if ! helm status " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " >/dev/null 2>&1; then\n" +
		"  " + helmTemplateCommand(config, releaseName, slotName, substitutions, renderMode) + " | " + stripClusterScopedCommand() + " | kubectl delete --ignore-not-found=true -f -\n" +
		"fi\n" +
		helmUpgradeInstallCommand(config, releaseName, slotName, substitutions, renderMode) + "\n"
	podSpec := map[string]any{
		"serviceAccountName": settings.NativeRunnerServiceAccount,
		"restartPolicy":      "Never",
		"volumes": []any{
			map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
			map[string]any{"name": "glim-clone", "secret": map[string]any{"secretName": secretName, "defaultMode": 0400}},
		},
		"initContainers": []any{map[string]any{
			"name":    "clone",
			"image":   "alpine/git:latest",
			"command": []string{"sh", "-c", cloneScript},
			"volumeMounts": []any{
				map[string]any{"name": "workspace", "mountPath": "/workspace"},
				map[string]any{"name": "glim-clone", "mountPath": "/var/run/glim-clone", "readOnly": true},
			},
		}},
		"containers": []any{map[string]any{
			"name":    "install",
			"image":   config.InstallerImage,
			"command": []string{"sh", "-c", installScript},
			"env": []any{
				map[string]any{"name": "GLIM_SLOT_NAME", "value": slotName},
				map[string]any{"name": "GLIM_SLOT_INDEX", "value": substitutions["slot_index"]},
				map[string]any{"name": "GLIM_HOST", "value": substitutions["host"]},
				map[string]any{"name": "GLIM_PROJECT", "value": project.Name},
			},
			"volumeMounts": []any{map[string]any{"name": "workspace", "mountPath": "/workspace"}},
		}},
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"namespace":   settings.NativeRunnerNamespace,
			"labels":      labels,
			"annotations": map[string]string{"glimmung.romaine.life/native-slot-name": slotName},
		},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": settings.NativeRunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func testSlotUninstallJobManifest(settings Settings, config testSlotHelmSettings, lease Lease, project Project, renderMode string) map[string]any {
	slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
	renderMode = firstNonEmpty(strings.TrimSpace(renderMode), testSlotRenderModeHot)
	jobName := testSlotHelmUninstallJobName(lease, renderMode)
	labels := testSlotLabels(lease, slotName)
	labels["glimmung.romaine.life/test-slot-installer"] = "true"
	labels["glimmung.romaine.life/render-mode"] = labelValue(renderMode)
	releaseName := testSlotHelmReleaseName(slotName, renderMode)
	uninstallScript := "set -eu\n" +
		"if helm status " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " >/dev/null 2>&1; then\n" +
		"  helm uninstall " + shellQuote(releaseName) + " --namespace " + shellQuote(slotName) + " --wait --timeout 180s\n" +
		"fi\n"
	podSpec := map[string]any{
		"serviceAccountName": settings.NativeRunnerServiceAccount,
		"restartPolicy":      "Never",
		"containers": []any{map[string]any{
			"name":    "uninstall",
			"image":   firstNonEmpty(config.InstallerImage, defaultTestSlotInstallerImage),
			"command": []string{"sh", "-c", uninstallScript},
			"env": []any{
				map[string]any{"name": "GLIM_SLOT_NAME", "value": slotName},
				map[string]any{"name": "GLIM_PROJECT", "value": project.Name},
				map[string]any{"name": "GLIM_RENDER_MODE", "value": renderMode},
			},
		}},
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        jobName,
			"namespace":   settings.NativeRunnerNamespace,
			"labels":      labels,
			"annotations": map[string]string{"glimmung.romaine.life/native-slot-name": slotName},
		},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": settings.NativeRunnerJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func helmTemplateCommand(config testSlotHelmSettings, releaseName, namespace string, substitutions map[string]string, renderMode string) string {
	parts := []string{
		"helm", "template", shellQuote(releaseName), shellQuote(config.ChartPath),
		"--namespace", shellQuote(namespace),
	}
	for key, value := range testSlotHelmValues(config, renderMode) {
		parts = append(parts, "--set", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	for key, value := range config.SetStringValues {
		parts = append(parts, "--set-string", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	return strings.Join(parts, " ")
}

func helmUpgradeInstallCommand(config testSlotHelmSettings, releaseName, namespace string, substitutions map[string]string, renderMode string) string {
	parts := []string{
		"helm", "upgrade", "--install", shellQuote(releaseName), shellQuote(config.ChartPath),
		"--namespace", shellQuote(namespace),
		"--wait", "--wait-for-jobs", "--timeout", "180s",
	}
	for key, value := range testSlotHelmValues(config, renderMode) {
		parts = append(parts, "--set", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	for key, value := range config.SetStringValues {
		parts = append(parts, "--set-string", shellQuote(key+"="+formatSubstitutions(value, substitutions)))
	}
	return strings.Join(parts, " ")
}

func testSlotHelmValues(config testSlotHelmSettings, renderMode string) map[string]string {
	values := make(map[string]string, len(config.Values)+2)
	for key, value := range config.Values {
		values[key] = value
	}
	values["testEnv.slotName"] = "{slot_name}"
	values["renderMode"] = renderMode
	return values
}

func stripClusterScopedCommand() string {
	awk := `awk 'BEGIN { doc=""; skip=0 } /^---[[:space:]]*$/ { if (doc != "" && skip == 0) printf "%s---\n", doc; doc=""; skip=0; next } { doc = doc $0 "\n"; if ($0 ~ /^kind:[[:space:]]*(ClusterRole|ClusterRoleBinding|Namespace)[[:space:]]*$/) skip=1 } END { if (doc != "" && skip == 0) printf "%s", doc }'`
	return "if command -v yq >/dev/null 2>&1; then yq 'select(.kind != \"ClusterRoleBinding\" and .kind != \"ClusterRole\" and .kind != \"Namespace\")'; else " + awk + "; fi"
}

func testSlotLabels(lease Lease, slotName string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":           "glimmung",
		"glimmung.romaine.life/test-slot":        "true",
		"glimmung.romaine.life/project":          labelValue(lease.Project),
		"glimmung.romaine.life/native-slot-name": labelValue(slotName),
	}
	if value := mapStringValueOrEmpty(lease.Metadata, "native_slot_index"); value != "" {
		labels["glimmung.romaine.life/native-slot-index"] = labelValue(value)
	}
	if ref := LeasePublicRefFromLease(lease); ref != "" {
		labels["glimmung.romaine.life/lease-ref"] = labelValue(ref)
	}
	return labels
}

func deleteOptions(policy string) map[string]any {
	return map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"propagationPolicy": policy,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func formatSubstitutions(value string, substitutions map[string]string) string {
	for key, replacement := range substitutions {
		value = strings.ReplaceAll(value, "{"+key+"}", replacement)
	}
	return value
}

func deepFormat(raw map[string]any, substitutions map[string]string) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = deepFormatValue(value, substitutions)
	}
	return out
}

func deepFormatValue(raw any, substitutions map[string]string) any {
	switch value := raw.(type) {
	case string:
		return formatSubstitutions(value, substitutions)
	case map[string]any:
		return deepFormat(value, substitutions)
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, deepFormatValue(item, substitutions))
		}
		return out
	default:
		return raw
	}
}

func configString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := stringFromMap(values, key); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boolConfigValue(values map[string]any, key string) bool {
	raw, ok := values[key]
	if !ok {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(value)
		return err == nil && parsed
	default:
		return false
	}
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringMapFromAnyMap(values map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func mapSliceFromAnySlice(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if mapped := anyMap(value); len(mapped) > 0 {
			out = append(out, mapped)
		}
	}
	return out
}

func anySlice(raw any) []any {
	if value, ok := raw.([]any); ok {
		return value
	}
	return nil
}

func trailingSlotIndex(slotName string) string {
	suffix := slotName[strings.LastIndex(slotName, "-")+1:]
	if _, err := strconv.Atoi(suffix); err == nil {
		return suffix
	}
	return ""
}

func playwrightResourceName(_ string, slotName string) string {
	if strings.TrimSpace(slotName) == "" {
		return ""
	}
	return "slot-playwright"
}

// PlaywrightWSEndpointFor returns the cluster-internal WebSocket URL of the
// leased slot's `slot-playwright` Service, or nil when the cluster is not
// running playwright-enabled slots or `slotName` is blank. mcp-glimmung and
// other in-cluster callers connect to this URL to drive a Chromium running
// inside the lease, instead of spawning one on a shared host.
func PlaywrightWSEndpointFor(settings Settings, slotName string) *string {
	if !settings.NativeRunnerPlaywrightEnabled {
		return nil
	}
	slot := strings.TrimSpace(slotName)
	if slot == "" {
		return nil
	}
	name := playwrightResourceName("", slot)
	if name == "" {
		return nil
	}
	port := strings.TrimSpace(settings.NativeRunnerPlaywrightPort)
	if port == "" {
		port = "3000"
	}
	endpoint := fmt.Sprintf("ws://%s.%s.svc.cluster.local:%s", name, slot, port)
	return &endpoint
}

func playwrightLabels(lease Lease, name string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/managed-by":           "glimmung",
		"app.kubernetes.io/part-of":              "glimmung-test-slot",
		"app.kubernetes.io/name":                 name,
		"glimmung.romaine.life/slot-playwright":  "true",
		"glimmung.romaine.life/resource-scope":   "tool",
		"glimmung.romaine.life/tool":             "playwright",
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

type playwrightResourceTarget struct {
	namespace string
	name      string
}

func playwrightResourceTargets(lease Lease, slotName string) []playwrightResourceTarget {
	var targets []playwrightResourceTarget
	if name := playwrightResourceName(lease.Project, slotName); name != "" {
		targets = append(targets, playwrightResourceTarget{namespace: slotName, name: name})
	}
	return targets
}

func playwrightDeployment(settings Settings, namespace, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.NativeRunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
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

func playwrightService(settings Settings, namespace, name string, labels map[string]string) map[string]any {
	port, err := strconv.Atoi(settings.NativeRunnerPlaywrightPort)
	if err != nil || port <= 0 {
		port = 3000
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
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
