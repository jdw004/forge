package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"forge/internal/model"
)

type remote struct {
	base, token string
	client      *http.Client
}

func (r remote) request(ctx context.Context, method, path string, body []byte, key string, out any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, r.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		request.Header.Set("Authorization", "Bearer "+r.token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("%s %s returned HTTP %d", method, path, response.StatusCode)
	}
	if out != nil {
		decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<20))
		if err := decoder.Decode(out); err != nil {
			return response.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return response.StatusCode, nil
}

// RunHTTP measures an existing Forge API. Running-state polling gives a lower
// bound on concurrency; it can miss short tasks between requests. Worker/slot
// options do not change the remote deployment.
func RunHTTP(parent context.Context, options Options, baseURL, token string, client *http.Client) (report Report, runError error) {
	spec, err := Spec(options)
	if err != nil {
		return report, err
	}
	report = newReport(options, "http", spec)
	report.ConcurrencySource = "polled_running_task_states_lower_bound"
	report.LatencySource = "persisted_start_to_finish_including_acknowledgement"
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return report, errors.New("API URL must be HTTP(S), without credentials, query, or fragment")
	}
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	if copy.Timeout == 0 {
		copy.Timeout = 10 * time.Second
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	api := remote{base: strings.TrimRight(baseURL, "/"), token: token, client: &copy}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	var system struct {
		Mode string `json:"mode"`
	}
	if _, err := api.request(ctx, "GET", "/v1/system", nil, "", &system); err != nil {
		return report, err
	}
	switch system.Mode {
	case "demo":
		report.ExecutionModel = "remote_api_single_process_demo"
	case "api":
		report.ExecutionModel = "remote_api_postgresql_workers"
	default:
		return report, errors.New("API reported an unsupported execution mode")
	}
	var nodes struct {
		Nodes []model.Node `json:"nodes"`
	}
	if _, err := api.request(ctx, "GET", "/v1/nodes", nil, "", &nodes); err != nil {
		return report, err
	}
	for _, node := range nodes.Nodes {
		report.ConfiguredCapacity += node.Capacity
	}
	report.WorkerPools = len(nodes.Nodes)
	body, err := json.Marshal(spec)
	if err != nil {
		return report, err
	}
	report.SubmissionKey = model.ID()
	report.StartedAt = time.Now().UTC()
	started := time.Now()
	var workflow model.Workflow
	status, err := api.request(ctx, "POST", "/v1/workflows", body, report.SubmissionKey, &workflow)
	if err != nil {
		report.Error = err.Error()
		report.Cleanup = "submission_outcome_unknown"
		return report, err
	}
	report.WorkflowID = workflow.ID
	if status != http.StatusCreated {
		return report, errors.New("submission replayed an existing workflow; no new benchmark was started")
	}
	// On a timeout or interruption, attempt to cancel only the workflow created
	// by this run. Report cleanup failure so abandoned work is visible.
	defer func() {
		if runError == nil {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := api.request(cleanup, "POST", "/v1/workflows/"+report.WorkflowID+"/cancel", nil, "", nil); err != nil {
			report.Cleanup = "cancellation_failed"
		} else {
			report.Cleanup = "workflow_cancelled"
		}
		report.Passed = false
		report.Error = runError.Error()
	}()
	poll := time.NewTicker(options.PollInterval)
	defer poll.Stop()
wait:
	for {
		running := 0
		for _, task := range workflow.Tasks {
			if task.State == model.Running {
				running++
			}
		}
		if running > report.PeakActive {
			report.PeakActive = running
		}
		if workflow.State.Terminal() {
			break
		}
		select {
		case <-ctx.Done():
			runError = ctx.Err()
			break wait
		case <-poll.C:
			var latest model.Workflow
			if _, err := api.request(ctx, "GET", "/v1/workflows/"+report.WorkflowID, nil, "", &latest); err != nil {
				runError = err
				break wait
			}
			workflow = latest
		}
	}
	report.summarize(workflow, time.Since(started))
	var durations []time.Duration
	for _, task := range workflow.Tasks {
		if task.StartedAt != nil && task.FinishedAt != nil && !task.FinishedAt.Before(*task.StartedAt) {
			durations = append(durations, task.FinishedAt.Sub(*task.StartedAt))
		}
	}
	report.Latency = latency(durations)
	if runError == nil && !report.Passed {
		runError = errors.New("one or more task outcomes did not match the scenario")
	}
	return report, runError
}
