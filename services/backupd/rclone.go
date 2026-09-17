package main

import (
	"context"
	"fmt"
	"os/exec"
	"path"
)

func copyRcloneBackup(ctx context.Context, source, target, runID string) (int64, error) {
	if source == "" || target == "" {
		return 0, fmt.Errorf("rclone source and target are required")
	}
	command := exec.CommandContext(ctx, "rclone", "copy", source, target+"/"+path.Base(runID), "--config", "/etc/rclone/rclone.conf", "--create-empty-src-dirs")
	if output, err := command.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("rclone copy: %w: %s", err, string(output))
	}
	return 0, nil
}
