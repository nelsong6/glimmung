package agentops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Command struct {
	Name         string
	Args         []string
	Cwd          string
	Input        string
	Stdout       io.Writer
	Stderr       io.Writer
	AllowFailure bool
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Runner interface {
	Run(ctx context.Context, command Command) (Result, error)
}

type ExecRunner struct{}

type CommandError struct {
	Command  []string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *CommandError) Error() string {
	parts := []string{
		fmt.Sprintf("Command failed: %s", strings.Join(e.Command, " ")),
		fmt.Sprintf("exit_code=%d", e.ExitCode),
		strings.TrimSpace(e.Stdout),
		strings.TrimSpace(e.Stderr),
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n")
}

func (ExecRunner) Run(ctx context.Context, command Command) (Result, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	if command.Cwd != "" {
		cmd.Dir = command.Cwd
	}
	if command.Input != "" {
		cmd.Stdin = strings.NewReader(command.Input)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if command.Stdout != nil {
		cmd.Stdout = io.MultiWriter(command.Stdout, &stdout)
	} else {
		cmd.Stdout = &stdout
	}
	if command.Stderr != nil {
		cmd.Stderr = io.MultiWriter(command.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	result := Result{
		Stdout: strings.TrimSpace(stdout.String()),
		Stderr: strings.TrimSpace(stderr.String()),
	}
	if err == nil {
		return result, nil
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		if command.AllowFailure {
			return result, nil
		}
		return result, &CommandError{
			Command:  append([]string{command.Name}, command.Args...),
			ExitCode: result.ExitCode,
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
		}
	}
	if command.AllowFailure {
		return result, nil
	}
	return result, err
}
