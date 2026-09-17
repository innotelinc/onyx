package main

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
)

func (s *server) handleRcloneRemotes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5e9)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rclone", "--config", "/etc/rclone/rclone.conf", "listremotes")
	output, err := cmd.Output()
	if err != nil {
		// Missing config or rclone is a valid unconfigured state for a new dev host.
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "remotes": []string{}})
		return
	}
	remotes := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ":") {
			remotes = append(remotes, strings.TrimSuffix(line, ":"))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": len(remotes) > 0, "remotes": remotes})
}
