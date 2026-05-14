//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultChildCommand = "/app/glimmung"
	defaultStopTimeout  = 10 * time.Second
)

type config struct {
	childCommand   string
	hotArtifact    string
	restartEnabled bool
	stopTimeout    time.Duration
}

func main() {
	os.Exit(run(loadConfig()))
}

func loadConfig() config {
	return config{
		childCommand:   firstNonEmpty(os.Getenv("GLIMMUNG_SUPERVISOR_CHILD"), defaultChildCommand),
		hotArtifact:    strings.TrimSpace(os.Getenv("GLIMMUNG_SUPERVISOR_HOT_ARTIFACT")),
		restartEnabled: envBoolDefault("GLIMMUNG_SUPERVISOR_RESTART_ENABLED", true),
		stopTimeout:    envDurationDefault("GLIMMUNG_SUPERVISOR_STOP_TIMEOUT_SECONDS", defaultStopTimeout),
	}
}

func run(cfg config) int {
	child, err := startChild(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start child: %v\n", err)
		return 1
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	exited := make(chan error, 1)
	go func() {
		exited <- child.Wait()
	}()

	for {
		select {
		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				if !cfg.restartEnabled {
					continue
				}
				if err := stopChild(child, cfg.stopTimeout); err != nil {
					fmt.Fprintf(os.Stderr, "stop child for restart: %v\n", err)
				}
				select {
				case <-exited:
				default:
				}
				child, err = startChild(cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "restart child: %v\n", err)
					return 1
				}
				exited = make(chan error, 1)
				go func() {
					exited <- child.Wait()
				}()
			case syscall.SIGTERM, syscall.SIGINT:
				if err := stopChild(child, cfg.stopTimeout); err != nil {
					fmt.Fprintf(os.Stderr, "stop child: %v\n", err)
				}
				return exitCode(<-exited)
			}
		case err := <-exited:
			return exitCode(err)
		}
	}
}

func startChild(cfg config) (*exec.Cmd, error) {
	command := cfg.childCommand
	if cfg.hotArtifact != "" {
		if info, err := os.Stat(cfg.hotArtifact); err == nil && !info.IsDir() {
			command = cfg.hotArtifact
		}
	}
	cmd := exec.Command(command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "glimmung-supervisor started pid=%d command=%s\n", cmd.Process.Pid, command)
	return cmd, nil
}

func stopChild(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return err
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for {
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return ctx.Err()
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "child wait failed: %v\n", err)
	return 1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envBoolDefault(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationDefault(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
