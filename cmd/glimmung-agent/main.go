package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nelsong6/glimmung/internal/ops/agentops"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	command := args[0]
	if command == "--help" || command == "-h" || command == "help" {
		printUsage(stdout)
		return 0
	}
	ops := agentops.New(agentops.ExecRunner{})
	ops.Stdout = stdout
	ops.Stderr = stderr
	ctx := context.Background()

	var result any
	var err error

	switch command {
	case "build-preview-image":
		fs := newFlagSet(command, stderr)
		imageTag := fs.String("image-tag", "", "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"image-tag": *imageTag}); err == nil {
			result, err = ops.BuildPreviewImage(ctx, *imageTag)
		}
	case "deploy-validation-preview":
		fs := newFlagSet(command, stderr)
		release := fs.String("release", "", "")
		namespace := fs.String("namespace", agentops.ProdNamespace, "")
		imageTag := fs.String("image-tag", "", "")
		publicHost := fs.String("public-host", "", "")
		prNumber := fs.String("pr-number", "", "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"release": *release, "image-tag": *imageTag, "public-host": *publicHost}); err == nil {
			result, err = ops.DeployPreview(ctx, agentops.DeployPreviewOptions{
				Release:    *release,
				Namespace:  *namespace,
				ImageTag:   *imageTag,
				PublicHost: *publicHost,
				PRNumber:   *prNumber,
			})
		}
	case "label-release-pr":
		fs := newFlagSet(command, stderr)
		release := fs.String("release", "", "")
		namespace := fs.String("namespace", agentops.ProdNamespace, "")
		prNumber := fs.String("pr-number", "", "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"release": *release, "pr-number": *prNumber}); err == nil {
			result, err = ops.LabelReleasePR(ctx, *release, *namespace, *prNumber)
		}
	case "label-release-branch":
		fs := newFlagSet(command, stderr)
		release := fs.String("release", "", "")
		namespace := fs.String("namespace", agentops.ProdNamespace, "")
		branchSlug := fs.String("branch-slug", "", "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"release": *release, "branch-slug": *branchSlug}); err == nil {
			result, err = ops.LabelReleaseBranch(ctx, *release, *namespace, *branchSlug)
		}
	case "rebuild-validation-image":
		fs := newFlagSet(command, stderr)
		release := fs.String("release", "", "")
		namespace := fs.String("namespace", agentops.ProdNamespace, "")
		branch := fs.String("branch", "", "")
		imageTag := fs.String("image-tag", "", "")
		repoSlug := fs.String("repo-slug", agentops.RepoSlugDefault, "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"release": *release, "branch": *branch, "image-tag": *imageTag}); err == nil {
			result, err = ops.RebuildValidationImage(ctx, agentops.RebuildValidationImageOptions{
				Release:   *release,
				Namespace: *namespace,
				Branch:    *branch,
				ImageTag:  *imageTag,
				RepoSlug:  *repoSlug,
			})
		}
	case "wait-public-preview":
		fs := newFlagSet(command, stderr)
		targetURL := fs.String("url", "", "")
		timeoutSeconds := fs.Int("timeout-seconds", 900, "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"url": *targetURL}); err == nil {
			result, err = ops.WaitPublicPreview(ctx, *targetURL, *timeoutSeconds)
		}
	case "destroy-validation-preview":
		fs := newFlagSet(command, stderr)
		release := fs.String("release", "", "")
		namespace := fs.String("namespace", agentops.ProdNamespace, "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"release": *release}); err == nil {
			result, err = ops.DestroyPreview(ctx, *release, *namespace)
		}
	case "apply-agent-job":
		fs := newFlagSet(command, stderr)
		namespace := fs.String("namespace", "", "")
		jobName := fs.String("job-name", "", "")
		issueNumber := fs.String("issue-number", "", "")
		issueID := fs.String("issue-id", "", "")
		issueTitle := fs.String("issue-title", "", "")
		issueURL := fs.String("issue-url", "", "")
		validationURL := fs.String("validation-url", "", "")
		branchName := fs.String("branch-name", "", "")
		proxyIP := fs.String("proxy-ip", "", "")
		agentContainerTag := fs.String("agent-container-tag", "", "")
		repoSlug := fs.String("repo-slug", agentops.RepoSlugDefault, "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{
			"namespace":           *namespace,
			"job-name":            *jobName,
			"issue-title":         *issueTitle,
			"issue-url":           *issueURL,
			"validation-url":      *validationURL,
			"branch-name":         *branchName,
			"proxy-ip":            *proxyIP,
			"agent-container-tag": *agentContainerTag,
		}); err == nil {
			result, err = ops.ApplyAgentJob(ctx, agentops.ApplyAgentJobOptions{
				Namespace:         *namespace,
				JobName:           *jobName,
				IssueNumber:       *issueNumber,
				IssueID:           *issueID,
				IssueTitle:        *issueTitle,
				IssueURL:          *issueURL,
				ValidationURL:     *validationURL,
				BranchName:        *branchName,
				ProxyIP:           *proxyIP,
				AgentContainerTag: *agentContainerTag,
				RepoSlug:          *repoSlug,
			})
		}
	case "wait-agent-job":
		fs := newFlagSet(command, stderr)
		namespace := fs.String("namespace", "", "")
		jobName := fs.String("job-name", "", "")
		timeoutSeconds := fs.Int("timeout-seconds", 1800, "")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return 2
		}
		if err = requireFlags(map[string]string{"namespace": *namespace, "job-name": *jobName}); err == nil {
			result, err = ops.WaitAgentJob(ctx, *namespace, *jobName, *timeoutSeconds)
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		printUsage(stderr)
		return 2
	}

	if err != nil {
		_ = writeJSON(stderr, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("%T: %v", err, err),
			"command": command,
		})
		return 1
	}
	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: glimmung-agent <command> [options]

commands:
  build-preview-image
  deploy-validation-preview
  label-release-pr
  label-release-branch
  rebuild-validation-image
  wait-public-preview
  destroy-validation-preview
  apply-agent-job
  wait-agent-job`)
}

func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	return fs
}

func requireFlags(values map[string]string) error {
	for name, value := range values {
		if value == "" {
			return errors.New("missing required --" + name)
		}
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
}
