// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command gabuilder is a bridge between LUCI and GitHub Actions.
//
// It is designed to run as a swarming bot task. When LUCI schedules a build
// for a windows/arm64 builder, this program:
//
//  1. Dispatches a GitHub Actions workflow_dispatch event for the requested
//     commit SHA on a repository containing the windows-arm64-test workflow.
//  2. Polls the resulting workflow run until it completes.
//  3. Streams the workflow logs to stdout so they appear in the LUCI build UI.
//  4. Exits with a status code reflecting the workflow conclusion (0 = success).
//
// Usage:
//
//	gabuilder -repo owner/repo -commit <sha> [-token-file <path>] [-builder-name <name>]
//
// The GitHub token must have Actions write permission on the target repository.
// It can be provided via -token-file (a file containing the token) or the
// GITHUB_TOKEN environment variable.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"sort"
	"strings"
	"time"
)

var (
	flagRepo         = flag.String("repo", "", "GitHub repository as owner/repo where the workflow lives (required)")
	flagCommit       = flag.String("commit", "", "commit SHA to test (required)")
	flagTokenFile    = flag.String("token-file", "", "path to file containing GitHub token")
	flagBuilderName  = flag.String("builder-name", "gotip-windows-arm64-msft", "GO_BUILDER_NAME to pass to the workflow")
	flagBuildID      = flag.String("build-id", "", "LUCI build ID for correlation")
	flagWorkflow     = flag.String("workflow", "windows-arm64-test.yml", "workflow file name to dispatch")
	flagRef          = flag.String("ref", "main", "git ref (branch/tag) for the workflow dispatch")
	flagTestShards   = flag.String("test-shards", "1", "number of test shards")
	flagPollInterval = flag.Duration("poll-interval", 30*time.Second, "interval between status polls")
	flagTimeout      = flag.Duration("timeout", 150*time.Minute, "overall timeout for the workflow run")
	flagGoRepo       = flag.String("go-repo", "https://go.googlesource.com/go", "Go source repository URL to clone inside the workflow")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)

	if *flagRepo == "" || *flagCommit == "" {
		flag.Usage()
		log.Fatal("both -repo and -commit are required")
	}

	token := resolveToken()
	if token == "" {
		log.Fatal("no GitHub token provided; use -token-file or set GITHUB_TOKEN")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := &ghClient{
		token:  token,
		http:   &http.Client{Timeout: 30 * time.Second},
		repo:   *flagRepo,
		apiURL: "https://api.github.com",
	}

	// Step 1: Dispatch the workflow.
	log.Printf("Dispatching workflow %s on %s at commit %s", *flagWorkflow, *flagRepo, *flagCommit)

	runID, err := client.dispatchWorkflow(ctx, *flagWorkflow, *flagRef, map[string]string{
		"commit_sha":   *flagCommit,
		"build_id":     *flagBuildID,
		"builder_name": *flagBuilderName,
		"test_shards":  *flagTestShards,
		"repository":   *flagGoRepo,
	})
	if err != nil {
		log.Fatalf("Failed to dispatch workflow: %v", err)
	}
	log.Printf("Workflow dispatched, run ID: %d", runID)
	log.Printf("View at: https://github.com/%s/actions/runs/%d", *flagRepo, runID)

	// Step 2: Poll until completion.
	conclusion, err := client.pollRun(ctx, runID, *flagPollInterval, *flagTimeout)
	if err != nil {
		log.Fatalf("Error polling workflow run: %v", err)
	}

	// Step 3: Download and stream logs.
	if err := client.streamLogs(ctx, runID, os.Stdout); err != nil {
		log.Printf("Warning: could not download logs: %v", err)
	}

	// Step 4: Map conclusion to exit code.
	log.Printf("Workflow run %d concluded: %s", runID, conclusion)
	switch conclusion {
	case "success":
		os.Exit(0)
	case "failure":
		os.Exit(1)
	case "cancelled":
		os.Exit(2)
	case "timed_out":
		os.Exit(3)
	default:
		log.Printf("Unknown conclusion: %s", conclusion)
		os.Exit(1)
	}
}

func resolveToken() string {
	if *flagTokenFile != "" {
		data, err := os.ReadFile(*flagTokenFile)
		if err != nil {
			log.Fatalf("Failed to read token file: %v", err)
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv("GITHUB_TOKEN")
}

// ghClient is a minimal GitHub REST API client for Actions workflows.
type ghClient struct {
	token  string
	http   *http.Client
	repo   string // owner/repo
	apiURL string
}

// dispatchWorkflow triggers a workflow_dispatch event and returns the run ID.
func (c *ghClient) dispatchWorkflow(ctx context.Context, workflowFile, ref string, inputs map[string]string) (int64, error) {
	body := map[string]any{
		"ref":                ref,
		"inputs":             inputs,
		"return_run_details": true,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal dispatch body: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", c.apiURL, c.repo, workflowFile)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonBody)))
	if err != nil {
		return 0, err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("dispatch request: %w", err)
	}
	defer resp.Body.Close()

	// With return_run_details=true, GitHub returns 200 with a JSON body
	// containing the run ID. If the API doesn't support that parameter
	// (older API versions), it returns 204 and we fall back to search.
	if resp.StatusCode == http.StatusOK {
		var result struct {
			WorkflowRunID int64  `json:"workflow_run_id"`
			RunURL        string `json:"run_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return 0, fmt.Errorf("decode dispatch response: %w", err)
		}
		if result.WorkflowRunID > 0 {
			return result.WorkflowRunID, nil
		}
	}

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		// Fallback: search for the run by head_sha.
		return c.findRunBySHA(ctx, workflowFile, *flagCommit)
	}

	respBody, _ := io.ReadAll(resp.Body)
	return 0, fmt.Errorf("dispatch returned status %d: %s", resp.StatusCode, respBody)
}

// findRunBySHA polls the workflow runs list to find a run matching the commit SHA.
// This is a fallback for when the dispatch API doesn't return the run ID directly.
func (c *ghClient) findRunBySHA(ctx context.Context, workflowFile, sha string) (int64, error) {
	// Give GitHub a moment to create the run.
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	for attempt := 0; attempt < 10; attempt++ {
		url := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?head_sha=%s&per_page=5",
			c.apiURL, c.repo, workflowFile, sha)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return 0, err
		}
		c.setHeaders(req)

		resp, err := c.http.Do(req)
		if err != nil {
			return 0, fmt.Errorf("list runs: %w", err)
		}
		var result struct {
			TotalCount int `json:"total_count"`
			Runs       []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return 0, fmt.Errorf("decode runs list: %w", err)
		}
		if len(result.Runs) > 0 {
			return result.Runs[0].ID, nil
		}

		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, fmt.Errorf("could not find workflow run for SHA %s after retries", sha)
}

// pollRun polls the workflow run until it reaches a terminal state.
func (c *ghClient) pollRun(ctx context.Context, runID int64, interval, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		status, conclusion, err := c.getRunStatus(ctx, runID)
		if err != nil {
			return "", err
		}

		log.Printf("Run %d: status=%s conclusion=%s", runID, status, conclusion)

		if status == "completed" {
			return conclusion, nil
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for run %d after %v", runID, timeout)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// getRunStatus fetches the current status and conclusion of a workflow run.
func (c *ghClient) getRunStatus(ctx context.Context, runID int64) (status, conclusion string, err error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d", c.apiURL, c.repo, runID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("get run status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("get run returned status %d: %s", resp.StatusCode, body)
	}

	var run struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		return "", "", fmt.Errorf("decode run: %w", err)
	}
	return run.Status, run.Conclusion, nil
}

// streamLogs downloads the workflow run logs and writes them to w.
// GitHub returns logs as a zip archive with one text file per step.
// We download the zip into memory, then extract and print each file
// in sorted order so the output is readable in the LUCI build UI.
func (c *ghClient) streamLogs(ctx context.Context, runID int64, w io.Writer) error {
	zipData, err := c.downloadLogsZip(ctx, runID)
	if err != nil {
		return err
	}
	return extractAndPrintLogs(zipData, w)
}

// downloadLogsZip fetches the log archive for a workflow run into memory.
func (c *ghClient) downloadLogsZip(ctx context.Context, runID int64) ([]byte, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d/logs", c.apiURL, c.repo, runID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	// Use a client with longer timeout for log downloads and that follows redirects.
	dlClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := dlClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("logs returned status %d: %s", resp.StatusCode, body)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read log archive: %w", err)
	}
	log.Printf("Downloaded %d bytes of logs", len(data))
	return data, nil
}

// extractAndPrintLogs unzips a GitHub Actions log archive and writes each
// step's log to w, sorted by filename so steps appear in order.
func extractAndPrintLogs(zipData []byte, w io.Writer) error {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	// Sort files by name so steps appear in execution order.
	// GitHub names them like "test/1_Set up job.txt", "test/2_Checkout.txt", etc.
	files := make([]*zip.File, len(r.File))
	copy(files, r.File)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})

	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Base(f.Name)
		fmt.Fprintf(w, "\n=== %s ===\n", name)

		rc, err := f.Open()
		if err != nil {
			fmt.Fprintf(w, "[error opening %s: %v]\n", f.Name, err)
			continue
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			fmt.Fprintf(w, "[error reading %s: %v]\n", f.Name, err)
			continue
		}
		rc.Close()
	}
	return nil
}

func (c *ghClient) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
