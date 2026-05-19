package server

import (
	"context"
	"time"
)

const committedNativeLaunchTimeout = 2 * time.Minute

func launchCommittedNativePhase(ctx context.Context, nativeLauncher NativeLauncher, req NativeLaunchRequest) ([]string, error) {
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedNativeLaunchTimeout)
	defer cancel()
	return nativeLauncher.LaunchNativePhase(launchCtx, req)
}
