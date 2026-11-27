package service

import (
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// workerStopTimeout is how long a stopping worker lets in-flight activities finish
// before they are abandoned (and retried after the process comes back).
const workerStopTimeout = 30 * time.Second

// StartWorkers runs the workflow worker (queue anz-projects: both workflows and
// every activity) and the generate worker (queue anz-generate: only
// GenerateAndOpenPR). maxGenerate caps parallel generation jobs, which replaces
// the old in-process semaphore (each job is a heavy process tree). The returned
// stop function drains both workers.
func StartWorkers(c client.Client, acts *Activities, maxGenerate int) (stop func(), err error) {
	if maxGenerate <= 0 {
		maxGenerate = defaultMaxGenerations
	}
	if acts.Heartbeat == nil {
		acts.Heartbeat = activity.RecordHeartbeat
	}

	main := worker.New(c, TaskQueueProjects, worker.Options{WorkerStopTimeout: workerStopTimeout})
	main.RegisterWorkflow(ProjectWorkflow)
	main.RegisterWorkflow(RedeployWorkflow)
	main.RegisterActivity(acts)

	gen := worker.New(c, TaskQueueGenerate, worker.Options{MaxConcurrentActivityExecutionSize: maxGenerate, WorkerStopTimeout: workerStopTimeout})
	gen.RegisterActivityWithOptions(acts.GenerateAndOpenPR, activity.RegisterOptions{Name: "GenerateAndOpenPR"})

	if err := main.Start(); err != nil {
		return nil, err
	}
	if err := gen.Start(); err != nil {
		main.Stop()
		return nil, err
	}
	return func() { gen.Stop(); main.Stop() }, nil
}
