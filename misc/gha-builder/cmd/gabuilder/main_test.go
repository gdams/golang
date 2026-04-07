// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDispatchWorkflow(t *testing.T) {
	dispatched := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/actions/workflows/test.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		dispatched = true

		var body struct {
			Ref    string            `json:"ref"`
			Inputs map[string]string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode dispatch body: %v", err)
		}
		if body.Ref != "main" {
			t.Errorf("ref = %q, want %q", body.Ref, "main")
		}
		if body.Inputs["commit_sha"] != "abc123" {
			t.Errorf("commit_sha = %q, want %q", body.Inputs["commit_sha"], "abc123")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"workflow_run_id": 42,
			"run_url":         "https://api.github.com/repos/test/repo/actions/runs/42",
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &ghClient{
		token:  "test-token",
		http:   &http.Client{Timeout: 5 * time.Second},
		repo:   "test/repo",
		apiURL: srv.URL,
	}

	runID, err := client.dispatchWorkflow(context.Background(), "test.yml", "main", map[string]string{
		"commit_sha": "abc123",
	})
	if err != nil {
		t.Fatalf("dispatchWorkflow: %v", err)
	}
	if !dispatched {
		t.Fatal("dispatch endpoint was not called")
	}
	if runID != 42 {
		t.Errorf("runID = %d, want 42", runID)
	}
}

func TestPollRun(t *testing.T) {
	pollCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/actions/runs/42", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		status := "in_progress"
		conclusion := ""
		if pollCount >= 3 {
			status = "completed"
			conclusion = "success"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":     status,
			"conclusion": conclusion,
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &ghClient{
		token:  "test-token",
		http:   &http.Client{Timeout: 5 * time.Second},
		repo:   "test/repo",
		apiURL: srv.URL,
	}

	conclusion, err := client.pollRun(context.Background(), 42, 10*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("pollRun: %v", err)
	}
	if conclusion != "success" {
		t.Errorf("conclusion = %q, want %q", conclusion, "success")
	}
	if pollCount < 3 {
		t.Errorf("pollCount = %d, want >= 3", pollCount)
	}
}

func TestPollRunTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/actions/runs/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":     "in_progress",
			"conclusion": "",
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &ghClient{
		token:  "test-token",
		http:   &http.Client{Timeout: 5 * time.Second},
		repo:   "test/repo",
		apiURL: srv.URL,
	}

	_, err := client.pollRun(context.Background(), 99, 10*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want timeout", err)
	}
}

func TestStreamLogs(t *testing.T) {
	logContent := "=== test log output ==="
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/test/repo/actions/runs/42/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		fmt.Fprint(w, logContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &ghClient{
		token:  "test-token",
		http:   &http.Client{Timeout: 5 * time.Second},
		repo:   "test/repo",
		apiURL: srv.URL,
	}

	var buf strings.Builder
	err := client.streamLogs(context.Background(), 42, &buf)
	if err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	if !strings.Contains(buf.String(), logContent) {
		t.Errorf("output = %q, want to contain %q", buf.String(), logContent)
	}
}
