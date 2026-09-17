package main

import (
	"encoding/json"
	"net/http"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

type createSnapshotBody struct {
	Pool      string `json:"pool"`
	Subvolume string `json:"subvolume"`
	Name      string `json:"name"`
	Writable  bool   `json:"writable"`
}

func (s *server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	resp, err := s.snapd.ListSnapshots(r.Context(), &onyxv1.ListSnapshotsRequest{
		Pool:      r.URL.Query().Get("pool"),
		Subvolume: r.URL.Query().Get("subvolume"),
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var body createSnapshotBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	resp, err := s.snapd.CreateSnapshot(r.Context(), &onyxv1.CreateSnapshotRequest{
		Pool: body.Pool, Subvolume: body.Subvolume, Name: body.Name, Writable: body.Writable,
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(resp))
}

func (s *server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	resp, err := s.snapd.DeleteSnapshot(r.Context(), &onyxv1.DeleteSnapshotRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleRollbackSnapshot(w http.ResponseWriter, r *http.Request) {
	resp, err := s.snapd.RollbackSnapshot(r.Context(), &onyxv1.RollbackSnapshotRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

type createBackupJobBody struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	TargetKind     string `json:"target_kind"`
	Target         string `json:"target"`
	Schedule       string `json:"schedule"`
	Retention      int32  `json:"retention"`
	SnapshotBefore bool   `json:"snapshot_before"`
}

func (s *server) handleBackupJobs(w http.ResponseWriter, r *http.Request) {
	resp, err := s.backupd.ListBackupJobs(r.Context(), &onyxv1.ListBackupJobsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleCreateBackupJob(w http.ResponseWriter, r *http.Request) {
	var body createBackupJobBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	resp, err := s.backupd.CreateBackupJob(r.Context(), &onyxv1.CreateBackupJobRequest{
		Name: body.Name, Source: body.Source, TargetKind: body.TargetKind, Target: body.Target,
		Schedule: body.Schedule, Retention: body.Retention, SnapshotBefore: body.SnapshotBefore,
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(resp))
}

func (s *server) handleDeleteBackupJob(w http.ResponseWriter, r *http.Request) {
	resp, err := s.backupd.DeleteBackupJob(r.Context(), &onyxv1.DeleteBackupJobRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	resp, err := s.backupd.RunBackup(r.Context(), &onyxv1.RunBackupRequest{JobId: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, protoMessage(resp))
}

func (s *server) handleBackupHistory(w http.ResponseWriter, r *http.Request) {
	resp, err := s.backupd.ListBackups(r.Context(), &onyxv1.ListBackupsRequest{JobId: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

type restoreBackupBody struct {
	Destination string `json:"destination"`
}

func (s *server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	var body restoreBackupBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	resp, err := s.backupd.RestoreBackup(r.Context(), &onyxv1.RestoreBackupRequest{RunId: r.PathValue("id"), Destination: body.Destination})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, protoMessage(resp))
}

func (s *server) handleBackupReport(w http.ResponseWriter, r *http.Request) {
	resp, err := s.backupd.GetBackupReport(r.Context(), &onyxv1.GetBackupReportRequest{JobId: r.URL.Query().Get("job_id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}
