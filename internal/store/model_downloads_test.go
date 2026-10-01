package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func nilModelDownloadContext() context.Context { return nil }

func TestStore_ModelDownloadPersistsAndRequeuesStaleLease(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateModelDownload(ctx, ModelDownloadTask{
		ID:       "download-1",
		Provider: "modelscope",
		RepoID:   "acme/demo",
		Revision: "main",
		SourceID: "models",
		Include:  []string{"*.gguf"},
		Status:   ModelDownloadQueued,
	}); err != nil {
		t.Fatal(err)
	}
	task, found, err := st.ClaimNextModelDownload(ctx, "worker-1", time.Now())
	if err != nil || !found {
		t.Fatalf("claim = %+v, found=%v, err=%v", task, found, err)
	}
	if task.Status != ModelDownloadDownloading || task.Attempts != 1 || task.Provider != "modelscope" {
		t.Fatalf("claimed task = %+v", task)
	}
	if err := st.UpdateModelDownloadManifest(ctx, task.ID, "worker-1", 2, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelDownloadProgress(ctx, task.ID, "worker-1", "model.gguf", 0, 2, 40, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.RequeueStaleModelDownloads(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	requeued, found, err := st.GetModelDownload(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("get requeued task = %+v, found=%v, err=%v", requeued, found, err)
	}
	if requeued.Status != ModelDownloadQueued || requeued.DownloadedBytes != 40 || requeued.TotalFiles != 2 {
		t.Fatalf("requeued task = %+v", requeued)
	}
	if len(requeued.Include) != 1 || requeued.Include[0] != "*.gguf" {
		t.Fatalf("requeued include = %v", requeued.Include)
	}
}

func TestStore_ModelDownloadRetryingWaitsUntilScheduled(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateModelDownload(ctx, ModelDownloadTask{ID: "scheduled-download", RepoID: "acme/demo", SourceID: "models"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	task, found, err := st.ClaimNextModelDownload(ctx, "worker-1", now)
	if err != nil || !found {
		t.Fatalf("initial claim = %+v, found=%v, err=%v", task, found, err)
	}
	retryAt := now.Add(time.Minute)
	if err := st.RetryOwnedModelDownload(ctx, task.ID, "worker-1", "temporary outage", retryAt); err != nil {
		t.Fatal(err)
	}
	scheduled, found, err := st.GetModelDownload(ctx, task.ID)
	if err != nil || !found || scheduled.Status != ModelDownloadRetrying || !scheduled.NextRetryAt.Equal(retryAt) || scheduled.Error != "temporary outage" {
		t.Fatalf("scheduled retry = %+v, found=%v, err=%v", scheduled, found, err)
	}
	if _, found, err := st.ClaimNextModelDownload(ctx, "worker-2", now.Add(30*time.Second)); err != nil || found {
		t.Fatalf("early claim found=%v, err=%v", found, err)
	}
	claimed, found, err := st.ClaimNextModelDownload(ctx, "worker-2", retryAt)
	if err != nil || !found || claimed.Status != ModelDownloadDownloading || claimed.Attempts != 2 || !claimed.NextRetryAt.IsZero() {
		t.Fatalf("due claim = %+v, found=%v, err=%v", claimed, found, err)
	}
}

func TestStore_ModelDownloadMethodsAcceptNilContext(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.CreateModelDownload(nilModelDownloadContext(), ModelDownloadTask{ID: "nil-download", RepoID: "acme/demo", SourceID: "models"}); err != nil {
		t.Fatalf("CreateModelDownload: %v", err)
	}
	if _, found, err := st.GetModelDownload(nilModelDownloadContext(), "nil-download"); err != nil || !found {
		t.Fatalf("GetModelDownload: found=%v err=%v", found, err)
	}
	if _, err := st.ListModelDownloads(nilModelDownloadContext(), 10, 0); err != nil {
		t.Fatalf("ListModelDownloads: %v", err)
	}
	if _, found, err := st.FindActiveModelDownload(nilModelDownloadContext(), "huggingface", "acme/demo", "main", "models", nil, nil); err != nil || !found {
		t.Fatalf("FindActiveModelDownload: found=%v err=%v", found, err)
	}
	if err := st.RequeueStaleModelDownloads(nilModelDownloadContext(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("RequeueStaleModelDownloads: %v", err)
	}
	if err := st.CancelModelDownload(nilModelDownloadContext(), "nil-download"); err != nil {
		t.Fatalf("CancelModelDownload: %v", err)
	}
	if err := st.RetryModelDownload(nilModelDownloadContext(), "nil-download"); err != nil {
		t.Fatalf("RetryModelDownload: %v", err)
	}
	if err := st.CancelModelDownload(nilModelDownloadContext(), "nil-download"); err != nil {
		t.Fatalf("CancelModelDownload after retry: %v", err)
	}
	if err := st.DeleteModelDownload(nilModelDownloadContext(), "nil-download"); err != nil {
		t.Fatalf("DeleteModelDownload: %v", err)
	}
}

func TestStore_ModelDownloadDeleteOnlyTerminal(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, status := range []string{ModelDownloadQueued, ModelDownloadDownloading} {
		id := "active-" + status
		if err := st.CreateModelDownload(ctx, ModelDownloadTask{ID: id, RepoID: "acme/demo", SourceID: "models", Status: status}); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteModelDownload(ctx, id); !errors.Is(err, ErrModelDownloadNotTerminal) {
			t.Fatalf("DeleteModelDownload(%s) = %v, want ErrModelDownloadNotTerminal", status, err)
		}
		if _, found, err := st.GetModelDownload(ctx, id); err != nil || !found {
			t.Fatalf("active task after failed delete = found %v, err %v", found, err)
		}
	}

	for _, status := range []string{ModelDownloadCompleted, ModelDownloadFailed, ModelDownloadCanceled} {
		id := "terminal-" + status
		if err := st.CreateModelDownload(ctx, ModelDownloadTask{ID: id, RepoID: "acme/demo", SourceID: "models", Status: status}); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteModelDownload(ctx, id); err != nil {
			t.Fatalf("DeleteModelDownload(%s): %v", status, err)
		}
		if _, found, err := st.GetModelDownload(ctx, id); err != nil || found {
			t.Fatalf("terminal task after delete = found %v, err %v", found, err)
		}
	}
}

func TestStore_ModelDownloadMethodsOnNilStoreReturnErrors(t *testing.T) {
	var st *Store
	ctx := context.Background()
	now := time.Now()
	if err := st.CreateModelDownload(ctx, ModelDownloadTask{ID: "download", RepoID: "owner/repo", SourceID: "source"}); err == nil {
		t.Fatal("CreateModelDownload on nil store unexpectedly succeeded")
	}
	if _, _, err := st.GetModelDownload(ctx, "download"); err == nil {
		t.Fatal("GetModelDownload on nil store unexpectedly succeeded")
	}
	if _, err := st.ListModelDownloads(ctx, 1, 0); err == nil {
		t.Fatal("ListModelDownloads on nil store unexpectedly succeeded")
	}
	if _, _, err := st.FindActiveModelDownload(ctx, "huggingface", "owner/repo", "main", "source", nil, nil); err == nil {
		t.Fatal("FindActiveModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.RequeueStaleModelDownloads(ctx, now); err == nil {
		t.Fatal("RequeueStaleModelDownloads on nil store unexpectedly succeeded")
	}
	if _, _, err := st.ClaimNextModelDownload(ctx, "worker", now); err == nil {
		t.Fatal("ClaimNextModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.UpdateModelDownloadManifest(ctx, "download", "worker", 1, 1); err == nil {
		t.Fatal("UpdateModelDownloadManifest on nil store unexpectedly succeeded")
	}
	if err := st.UpdateModelDownloadProgress(ctx, "download", "worker", "file", 1, 1, 1, 1); err == nil {
		t.Fatal("UpdateModelDownloadProgress on nil store unexpectedly succeeded")
	}
	if err := st.FinishModelDownload(ctx, "download", "worker", ModelDownloadCompleted, "", 1, 1); err == nil {
		t.Fatal("FinishModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.RequeueOwnedModelDownload(ctx, "download", "worker"); err == nil {
		t.Fatal("RequeueOwnedModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.RetryOwnedModelDownload(ctx, "download", "worker", "temporary", now); err == nil {
		t.Fatal("RetryOwnedModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.CancelModelDownload(ctx, "download"); err == nil {
		t.Fatal("CancelModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.RetryModelDownload(ctx, "download"); err == nil {
		t.Fatal("RetryModelDownload on nil store unexpectedly succeeded")
	}
	if err := st.DeleteModelDownload(ctx, "download"); err == nil {
		t.Fatal("DeleteModelDownload on nil store unexpectedly succeeded")
	}
}
