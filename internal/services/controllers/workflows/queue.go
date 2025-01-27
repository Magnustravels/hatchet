package workflows

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/go-multierror"
	"golang.org/x/sync/errgroup"

	"github.com/hatchet-dev/hatchet/internal/datautils"
	"github.com/hatchet-dev/hatchet/internal/msgqueue"
	"github.com/hatchet-dev/hatchet/internal/repository"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/db"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/dbsqlc"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/sqlchelpers"
	"github.com/hatchet-dev/hatchet/internal/services/shared/defaults"
	"github.com/hatchet-dev/hatchet/internal/services/shared/tasktypes"
	"github.com/hatchet-dev/hatchet/internal/telemetry"
	"github.com/hatchet-dev/hatchet/internal/telemetry/servertel"
)

func (wc *WorkflowsControllerImpl) handleWorkflowRunQueued(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-workflow-run-queued")
	defer span.End()

	payload := tasktypes.WorkflowRunQueuedTaskPayload{}
	metadata := tasktypes.WorkflowRunQueuedTaskMetadata{}

	err := wc.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = wc.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	// get the workflow run in the database
	workflowRun, err := wc.repo.WorkflowRun().GetWorkflowRunById(metadata.TenantId, payload.WorkflowRunId)

	if err != nil {
		return fmt.Errorf("could not get job run: %w", err)
	}

	servertel.WithWorkflowRunModel(span, workflowRun)

	wc.l.Info().Msgf("starting workflow run %s", workflowRun.ID)

	// determine if we should start this workflow run or we need to limit its concurrency
	// if the workflow has concurrency settings, then we need to check if we can start it
	if _, hasConcurrency := workflowRun.WorkflowVersion().Concurrency(); hasConcurrency {
		wc.l.Info().Msgf("workflow %s has concurrency settings", workflowRun.ID)

		groupKeyRun, ok := workflowRun.GetGroupKeyRun()

		if !ok {
			return fmt.Errorf("could not get group key run")
		}

		sqlcGroupKeyRun, err := wc.repo.GetGroupKeyRun().GetGroupKeyRunForEngine(metadata.TenantId, groupKeyRun.ID)

		if err != nil {
			return fmt.Errorf("could not get group key run for engine: %w", err)
		}

		err = wc.scheduleGetGroupAction(ctx, sqlcGroupKeyRun)

		if err != nil {
			return fmt.Errorf("could not trigger get group action: %w", err)
		}

		return nil
	}

	err = wc.queueWorkflowRunJobs(ctx, workflowRun)

	if err != nil {
		return fmt.Errorf("could not start workflow run: %w", err)
	}

	return nil
}

func (wc *WorkflowsControllerImpl) handleWorkflowRunFinished(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-workflow-run-finished")
	defer span.End()

	payload := tasktypes.WorkflowRunFinishedTask{}
	metadata := tasktypes.WorkflowRunFinishedTaskMetadata{}

	err := wc.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = wc.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	// get the workflow run in the database
	workflowRun, err := wc.repo.WorkflowRun().GetWorkflowRunById(metadata.TenantId, payload.WorkflowRunId)

	if err != nil {
		return fmt.Errorf("could not get job run: %w", err)
	}

	servertel.WithWorkflowRunModel(span, workflowRun)

	wc.l.Info().Msgf("finishing workflow run %s", workflowRun.ID)

	// if the workflow run has a concurrency group, then we need to queue any queued workflow runs
	if concurrency, hasConcurrency := workflowRun.WorkflowVersion().Concurrency(); hasConcurrency {
		wc.l.Info().Msgf("workflow %s has concurrency settings", workflowRun.ID)

		switch concurrency.LimitStrategy {
		case db.ConcurrencyLimitStrategyGroupRoundRobin:
			err = wc.queueByGroupRoundRobin(ctx, metadata.TenantId, workflowRun.WorkflowVersion())
		default:
			return nil
		}

		if err != nil {
			return fmt.Errorf("could not queue workflow runs: %w", err)
		}
	}

	return nil
}

func (wc *WorkflowsControllerImpl) scheduleGetGroupAction(
	ctx context.Context,
	getGroupKeyRun *dbsqlc.GetGroupKeyRunForEngineRow,
) error {
	ctx, span := telemetry.NewSpan(ctx, "trigger-get-group-action")
	defer span.End()

	tenantId := sqlchelpers.UUIDToStr(getGroupKeyRun.GetGroupKeyRun.TenantId)
	getGroupKeyRunId := sqlchelpers.UUIDToStr(getGroupKeyRun.GetGroupKeyRun.ID)
	workflowRunId := sqlchelpers.UUIDToStr(getGroupKeyRun.WorkflowRunId)

	getGroupKeyRun, err := wc.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(tenantId, getGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
		Status: repository.StepRunStatusPtr(db.StepRunStatusPendingAssignment),
	})

	if err != nil {
		return fmt.Errorf("could not update get group key run: %w", err)
	}

	selectedWorkerId, dispatcherId, err := wc.repo.GetGroupKeyRun().AssignGetGroupKeyRunToWorker(
		tenantId,
		getGroupKeyRunId,
	)

	if err != nil {
		if errors.Is(err, repository.ErrNoWorkerAvailable) {
			wc.l.Debug().Msgf("no worker available for get group key run %s, requeueing", getGroupKeyRunId)
			return nil
		}

		return fmt.Errorf("could not assign get group key run to worker: %w", err)
	}

	telemetry.WithAttributes(span, servertel.WorkerId(selectedWorkerId))

	tickerId, err := wc.repo.GetGroupKeyRun().AssignGetGroupKeyRunToTicker(tenantId, getGroupKeyRunId)

	if err != nil {
		return fmt.Errorf("could not assign get group key run to ticker: %w", err)
	}

	scheduleTimeoutTask, err := scheduleGetGroupKeyRunTimeoutTask(tenantId, workflowRunId, getGroupKeyRunId)

	if err != nil {
		return fmt.Errorf("could not schedule get group key run timeout task: %w", err)
	}

	// send a task to the dispatcher
	err = wc.mq.AddMessage(
		ctx,
		msgqueue.QueueTypeFromDispatcherID(dispatcherId),
		getGroupActionTask(
			tenantId,
			workflowRunId,
			selectedWorkerId,
			dispatcherId,
		),
	)

	if err != nil {
		return fmt.Errorf("could not add job assigned task to task queue: %w", err)
	}

	// send a task to the ticker
	err = wc.mq.AddMessage(
		ctx,
		msgqueue.QueueTypeFromTickerID(tickerId),
		scheduleTimeoutTask,
	)

	if err != nil {
		return fmt.Errorf("could not add schedule get group key run timeout task to task queue: %w", err)
	}

	return nil
}

func (wc *WorkflowsControllerImpl) queueWorkflowRunJobs(ctx context.Context, workflowRun *db.WorkflowRunModel) error {
	ctx, span := telemetry.NewSpan(ctx, "process-event")
	defer span.End()

	jobRuns := workflowRun.JobRuns()

	var err error

	for i := range jobRuns {
		err := wc.mq.AddMessage(
			context.Background(),
			msgqueue.JOB_PROCESSING_QUEUE,
			tasktypes.JobRunQueuedToTask(jobRuns[i].Job(), &jobRuns[i]),
		)

		if err != nil {
			err = multierror.Append(err, fmt.Errorf("could not add job run to task queue: %w", err))
		}
	}

	return err
}

func (wc *WorkflowsControllerImpl) runGetGroupKeyRunRequeue(ctx context.Context) func() {
	return func() {
		wc.l.Debug().Msgf("workflows controller: checking get group key run requeue")

		// list all tenants
		tenants, err := wc.repo.Tenant().ListTenants()

		if err != nil {
			wc.l.Err(err).Msg("could not list tenants")
			return
		}

		g := new(errgroup.Group)

		for i := range tenants {
			tenantId := tenants[i].ID

			g.Go(func() error {
				return wc.runGetGroupKeyRunRequeueTenant(ctx, tenantId)
			})
		}

		err = g.Wait()

		if err != nil {
			wc.l.Err(err).Msg("could not run get group key run requeue")
		}
	}
}

// runGetGroupKeyRunRequeueTenant looks for any get group key runs that haven't been assigned that are past their
// requeue time
func (ec *WorkflowsControllerImpl) runGetGroupKeyRunRequeueTenant(ctx context.Context, tenantId string) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-get-group-key-run-requeue")
	defer span.End()

	getGroupKeyRuns, err := ec.repo.GetGroupKeyRun().ListGetGroupKeyRunsToRequeue(tenantId)

	if err != nil {
		return fmt.Errorf("could not list group key runs: %w", err)
	}

	g := new(errgroup.Group)

	for i := range getGroupKeyRuns {
		getGroupKeyRunCp := getGroupKeyRuns[i]

		// wrap in func to get defer on the span to avoid leaking spans
		g.Go(func() (err error) {
			var innerGetGroupKeyRun *dbsqlc.GetGroupKeyRunForEngineRow

			ctx, span := telemetry.NewSpan(ctx, "handle-get-group-key-run-requeue-tenant")
			defer span.End()

			getGroupKeyRunId := sqlchelpers.UUIDToStr(getGroupKeyRunCp.ID)

			ec.l.Debug().Msgf("requeueing group key run %s", getGroupKeyRunId)

			now := time.Now().UTC().UTC()

			// if the current time is after the scheduleTimeoutAt, then mark this as timed out
			scheduleTimeoutAt := getGroupKeyRunCp.ScheduleTimeoutAt.Time

			// timed out if there was no scheduleTimeoutAt set and the current time is after the get group key run created at time plus the default schedule timeout,
			// or if the scheduleTimeoutAt is set and the current time is after the scheduleTimeoutAt
			isTimedOut := !scheduleTimeoutAt.IsZero() && scheduleTimeoutAt.Before(now)

			if isTimedOut {
				innerGetGroupKeyRun, err = ec.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(tenantId, getGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
					CancelledAt:     &now,
					CancelledReason: repository.StringPtr("SCHEDULING_TIMED_OUT"),
					Status:          repository.StepRunStatusPtr(db.StepRunStatusCancelled),
				})

				if err != nil {
					return fmt.Errorf("could not update get group key run %s: %w", getGroupKeyRunId, err)
				}

				return nil
			}

			requeueAfter := time.Now().UTC().Add(time.Second * 5)

			innerGetGroupKeyRun, err = ec.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(tenantId, getGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
				RequeueAfter: &requeueAfter,
			})

			if err != nil {
				return fmt.Errorf("could not update get group key run %s: %w", getGroupKeyRunId, err)
			}

			return ec.scheduleGetGroupAction(ctx, innerGetGroupKeyRun)
		})
	}

	return g.Wait()
}

func (wc *WorkflowsControllerImpl) runGetGroupKeyRunReassign(ctx context.Context) func() {
	return func() {
		wc.l.Debug().Msgf("workflows controller: checking get group key run reassign")

		// list all tenants
		tenants, err := wc.repo.Tenant().ListTenants()

		if err != nil {
			wc.l.Err(err).Msg("could not list tenants")
			return
		}

		g := new(errgroup.Group)

		for i := range tenants {
			tenantId := tenants[i].ID

			g.Go(func() error {
				return wc.runGetGroupKeyRunReassignTenant(ctx, tenantId)
			})
		}

		err = g.Wait()

		if err != nil {
			wc.l.Err(err).Msg("could not run get group key run reassign")
		}
	}
}

// runGetGroupKeyRunReassignTenant looks for any get group key runs that have been assigned to an inactive worker
func (ec *WorkflowsControllerImpl) runGetGroupKeyRunReassignTenant(ctx context.Context, tenantId string) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-get-group-key-run-reassign")
	defer span.End()

	getGroupKeyRuns, err := ec.repo.GetGroupKeyRun().ListGetGroupKeyRunsToReassign(tenantId)

	if err != nil {
		return fmt.Errorf("could not list get group key runs: %w", err)
	}

	g := new(errgroup.Group)

	for i := range getGroupKeyRuns {
		getGroupKeyRunCp := getGroupKeyRuns[i]

		// wrap in func to get defer on the span to avoid leaking spans
		g.Go(func() (err error) {
			var innerGetGroupKeyRun *dbsqlc.GetGroupKeyRunForEngineRow

			ctx, span := telemetry.NewSpan(ctx, "handle-get-group-key-run-reassign-tenant")
			defer span.End()

			getGroupKeyRunId := sqlchelpers.UUIDToStr(getGroupKeyRunCp.ID)

			ec.l.Debug().Msgf("reassigning group key run %s", getGroupKeyRunId)

			requeueAfter := time.Now().UTC().Add(time.Second * 5)

			innerGetGroupKeyRun, err = ec.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(tenantId, getGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
				RequeueAfter: &requeueAfter,
				Status:       repository.StepRunStatusPtr(db.StepRunStatusPendingAssignment),
			})

			if err != nil {
				return fmt.Errorf("could not update get group key run %s: %w", getGroupKeyRunId, err)
			}

			return ec.scheduleGetGroupAction(ctx, innerGetGroupKeyRun)
		})
	}

	return g.Wait()
}

func (wc *WorkflowsControllerImpl) queueByCancelInProgress(ctx context.Context, tenantId, groupKey string, workflowVersion *db.WorkflowVersionModel) error {
	ctx, span := telemetry.NewSpan(ctx, "queue-by-cancel-in-progress")
	defer span.End()

	wc.l.Info().Msgf("handling queue with strategy CANCEL_IN_PROGRESS for %s", groupKey)

	concurrency, hasConcurrency := workflowVersion.Concurrency()

	if !hasConcurrency {
		return nil
	}

	// list all workflow runs that are running for this group key
	running := db.WorkflowRunStatusRunning

	runningWorkflowRuns, err := wc.repo.WorkflowRun().ListWorkflowRuns(tenantId, &repository.ListWorkflowRunsOpts{
		WorkflowVersionId: &concurrency.WorkflowVersionID,
		GroupKey:          &groupKey,
		Status:            &running,
		// order from oldest to newest
		OrderBy:        repository.StringPtr("createdAt"),
		OrderDirection: repository.StringPtr("ASC"),
	})

	if err != nil {
		return fmt.Errorf("could not list running workflow runs: %w", err)
	}

	// get workflow runs which are queued for this group key
	queued := db.WorkflowRunStatusQueued

	queuedWorkflowRuns, err := wc.repo.WorkflowRun().ListWorkflowRuns(tenantId, &repository.ListWorkflowRunsOpts{
		WorkflowVersionId: &concurrency.WorkflowVersionID,
		GroupKey:          &groupKey,
		Status:            &queued,
		// order from oldest to newest
		OrderBy:        repository.StringPtr("createdAt"),
		OrderDirection: repository.StringPtr("ASC"),
		Limit:          &concurrency.MaxRuns,
	})

	if err != nil {
		return fmt.Errorf("could not list queued workflow runs: %w", err)
	}

	// cancel up to maxRuns - queued runs
	maxRuns := concurrency.MaxRuns
	maxToQueue := min(maxRuns, len(queuedWorkflowRuns.Rows))
	errGroup := new(errgroup.Group)

	for i := range runningWorkflowRuns.Rows {
		// in this strategy we need to make room for all of the queued runs
		if i >= len(queuedWorkflowRuns.Rows) {
			break
		}

		row := runningWorkflowRuns.Rows[i]

		errGroup.Go(func() error {
			workflowRunId := sqlchelpers.UUIDToStr(row.WorkflowRun.ID)
			return wc.cancelWorkflowRun(tenantId, workflowRunId)
		})
	}

	if err := errGroup.Wait(); err != nil {
		return fmt.Errorf("could not cancel workflow runs: %w", err)
	}

	errGroup = new(errgroup.Group)

	for i := range queuedWorkflowRuns.Rows {
		if i >= maxToQueue {
			break
		}

		row := queuedWorkflowRuns.Rows[i]

		errGroup.Go(func() error {
			workflowRunId := sqlchelpers.UUIDToStr(row.WorkflowRun.ID)
			workflowRun, err := wc.repo.WorkflowRun().GetWorkflowRunById(tenantId, workflowRunId)

			if err != nil {
				return fmt.Errorf("could not get workflow run: %w", err)
			}

			return wc.queueWorkflowRunJobs(ctx, workflowRun)
		})
	}

	if err := errGroup.Wait(); err != nil {
		return fmt.Errorf("could not queue workflow runs: %w", err)
	}

	return nil
}

func (wc *WorkflowsControllerImpl) queueByGroupRoundRobin(ctx context.Context, tenantId string, workflowVersion *db.WorkflowVersionModel) error {
	ctx, span := telemetry.NewSpan(ctx, "queue-by-group-round-robin")
	defer span.End()

	wc.l.Info().Msgf("handling queue with strategy GROUP_ROUND_ROBIN for workflow version %s", workflowVersion.ID)

	concurrency, hasConcurrency := workflowVersion.Concurrency()

	if !hasConcurrency {
		return nil
	}

	// get workflow runs which are queued for this group key
	poppedWorkflowRuns, err := wc.repo.WorkflowRun().PopWorkflowRunsRoundRobin(tenantId, concurrency.WorkflowVersionID, concurrency.MaxRuns)

	if err != nil {
		return fmt.Errorf("could not list queued workflow runs: %w", err)
	}

	errGroup := new(errgroup.Group)

	for i := range poppedWorkflowRuns {
		row := poppedWorkflowRuns[i]

		errGroup.Go(func() error {
			workflowRunId := sqlchelpers.UUIDToStr(row.ID)

			wc.l.Info().Msgf("popped workflow run %s", workflowRunId)
			workflowRun, err := wc.repo.WorkflowRun().GetWorkflowRunById(tenantId, workflowRunId)

			if err != nil {
				return fmt.Errorf("could not get workflow run: %w", err)
			}

			return wc.queueWorkflowRunJobs(ctx, workflowRun)
		})
	}

	if err := errGroup.Wait(); err != nil {
		return fmt.Errorf("could not queue workflow runs: %w", err)
	}

	return nil
}

func (wc *WorkflowsControllerImpl) cancelWorkflowRun(tenantId, workflowRunId string) error {
	// get the workflow run in the database
	workflowRun, err := wc.repo.WorkflowRun().GetWorkflowRunById(tenantId, workflowRunId)

	if err != nil {
		return fmt.Errorf("could not get workflow run: %w", err)
	}

	// cancel all running step runs
	stepRuns, err := wc.repo.StepRun().ListStepRuns(tenantId, &repository.ListStepRunsOpts{
		WorkflowRunId: &workflowRun.ID,
		Status:        repository.StepRunStatusPtr(db.StepRunStatusRunning),
	})

	if err != nil {
		return fmt.Errorf("could not list step runs: %w", err)
	}

	errGroup := new(errgroup.Group)

	for i := range stepRuns {
		stepRunCp := stepRuns[i]
		errGroup.Go(func() error {
			return wc.mq.AddMessage(
				context.Background(),
				msgqueue.JOB_PROCESSING_QUEUE,
				getStepRunNotifyCancelTask(tenantId, stepRunCp.ID, "CANCELLED_BY_CONCURRENCY_LIMIT"),
			)
		})
	}

	return errGroup.Wait()
}

func getGroupActionTask(tenantId, workflowRunId, workerId, dispatcherId string) *msgqueue.Message {
	payload, _ := datautils.ToJSONMap(tasktypes.GroupKeyActionAssignedTaskPayload{
		WorkflowRunId: workflowRunId,
		WorkerId:      workerId,
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.GroupKeyActionAssignedTaskMetadata{
		TenantId:     tenantId,
		DispatcherId: dispatcherId,
	})

	return &msgqueue.Message{
		ID:       "group-key-action-assigned",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}
}

func getStepRunNotifyCancelTask(tenantId, stepRunId, reason string) *msgqueue.Message {
	payload, _ := datautils.ToJSONMap(tasktypes.StepRunNotifyCancelTaskPayload{
		StepRunId:       stepRunId,
		CancelledReason: reason,
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.StepRunNotifyCancelTaskMetadata{
		TenantId: tenantId,
	})

	return &msgqueue.Message{
		ID:       "step-run-cancelled",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}
}

func scheduleGetGroupKeyRunTimeoutTask(tenantId, workflowRunId, getGroupKeyRunId string) (*msgqueue.Message, error) {
	durationStr := defaults.DefaultStepRunTimeout

	// get a duration
	duration, err := time.ParseDuration(durationStr)

	if err != nil {
		return nil, fmt.Errorf("could not parse duration: %w", err)
	}

	timeoutAt := time.Now().UTC().Add(duration)

	payload, _ := datautils.ToJSONMap(tasktypes.ScheduleGetGroupKeyRunTimeoutTaskPayload{
		GetGroupKeyRunId: getGroupKeyRunId,
		WorkflowRunId:    workflowRunId,
		TimeoutAt:        timeoutAt.Format(time.RFC3339),
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.ScheduleGetGroupKeyRunTimeoutTaskMetadata{
		TenantId: tenantId,
	})

	return &msgqueue.Message{
		ID:       "schedule-get-group-key-run-timeout",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}, nil
}
