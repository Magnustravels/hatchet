package workflows

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/hatchet-dev/hatchet/internal/datautils"
	"github.com/hatchet-dev/hatchet/internal/logger"
	"github.com/hatchet-dev/hatchet/internal/msgqueue"
	"github.com/hatchet-dev/hatchet/internal/repository"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/db"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/dbsqlc"
	"github.com/hatchet-dev/hatchet/internal/repository/prisma/sqlchelpers"
	"github.com/hatchet-dev/hatchet/internal/services/shared/tasktypes"
	"github.com/hatchet-dev/hatchet/internal/telemetry"
)

type WorkflowsController interface {
	Start(ctx context.Context) error
}

type WorkflowsControllerImpl struct {
	mq   msgqueue.MessageQueue
	l    *zerolog.Logger
	repo repository.Repository
	dv   datautils.DataDecoderValidator
	s    gocron.Scheduler
}

type WorkflowsControllerOpt func(*WorkflowsControllerOpts)

type WorkflowsControllerOpts struct {
	mq   msgqueue.MessageQueue
	l    *zerolog.Logger
	repo repository.Repository
	dv   datautils.DataDecoderValidator
}

func defaultWorkflowsControllerOpts() *WorkflowsControllerOpts {
	logger := logger.NewDefaultLogger("workflows-controller")
	return &WorkflowsControllerOpts{
		l:  &logger,
		dv: datautils.NewDataDecoderValidator(),
	}
}

func WithMessageQueue(mq msgqueue.MessageQueue) WorkflowsControllerOpt {
	return func(opts *WorkflowsControllerOpts) {
		opts.mq = mq
	}
}

func WithLogger(l *zerolog.Logger) WorkflowsControllerOpt {
	return func(opts *WorkflowsControllerOpts) {
		opts.l = l
	}
}

func WithRepository(r repository.Repository) WorkflowsControllerOpt {
	return func(opts *WorkflowsControllerOpts) {
		opts.repo = r
	}
}

func WithDataDecoderValidator(dv datautils.DataDecoderValidator) WorkflowsControllerOpt {
	return func(opts *WorkflowsControllerOpts) {
		opts.dv = dv
	}
}

func New(fs ...WorkflowsControllerOpt) (*WorkflowsControllerImpl, error) {
	opts := defaultWorkflowsControllerOpts()

	for _, f := range fs {
		f(opts)
	}

	if opts.mq == nil {
		return nil, fmt.Errorf("task queue is required. use WithMessageQueue")
	}

	if opts.repo == nil {
		return nil, fmt.Errorf("repository is required. use WithRepository")
	}

	s, err := gocron.NewScheduler(gocron.WithLocation(time.UTC))

	if err != nil {
		return nil, fmt.Errorf("could not create scheduler: %w", err)
	}

	newLogger := opts.l.With().Str("service", "workflows-controller").Logger()
	opts.l = &newLogger

	return &WorkflowsControllerImpl{
		mq:   opts.mq,
		l:    opts.l,
		repo: opts.repo,
		dv:   opts.dv,
		s:    s,
	}, nil
}

func (wc *WorkflowsControllerImpl) Start() (func() error, error) {
	wc.l.Debug().Msg("starting workflows controller")

	ctx, cancel := context.WithCancel(context.Background())

	wg := sync.WaitGroup{}

	_, err := wc.s.NewJob(
		gocron.DurationJob(time.Second*5),
		gocron.NewTask(
			wc.runGetGroupKeyRunRequeue(ctx),
		),
	)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not schedule get group key run requeue: %w", err)
	}

	_, err = wc.s.NewJob(
		gocron.DurationJob(time.Second*5),
		gocron.NewTask(
			wc.runGetGroupKeyRunReassign(ctx),
		),
	)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not schedule get group key run reassign: %w", err)
	}

	wc.s.Start()

	f := func(task *msgqueue.Message) error {
		wg.Add(1)
		defer wg.Done()

		err := wc.handleTask(context.Background(), task)
		if err != nil {
			wc.l.Error().Err(err).Msg("could not handle job task")
			return err
		}

		return nil
	}

	cleanupQueue, err := wc.mq.Subscribe(msgqueue.WORKFLOW_PROCESSING_QUEUE, f, msgqueue.NoOpHook)

	if err != nil {
		cancel()
		return nil, err
	}

	cleanup := func() error {
		cancel()

		if err := cleanupQueue(); err != nil {
			return fmt.Errorf("could not cleanup queue: %w", err)
		}

		wg.Wait()

		if err := wc.s.Shutdown(); err != nil {
			return fmt.Errorf("could not shutdown scheduler: %w", err)
		}

		return nil
	}

	return cleanup, nil
}

func (wc *WorkflowsControllerImpl) handleTask(ctx context.Context, task *msgqueue.Message) error {
	switch task.ID {
	case "workflow-run-queued":
		return wc.handleWorkflowRunQueued(ctx, task)
	case "get-group-key-run-started":
		return wc.handleGroupKeyRunStarted(ctx, task)
	case "get-group-key-run-finished":
		return wc.handleGroupKeyRunFinished(ctx, task)
	case "get-group-key-run-failed":
		return wc.handleGroupKeyRunFailed(ctx, task)
	case "workflow-run-finished":
		return wc.handleWorkflowRunFinished(ctx, task)
	}

	return fmt.Errorf("unknown task: %s", task.ID)
}

func (ec *WorkflowsControllerImpl) handleGroupKeyRunStarted(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpan(ctx, "get-group-key-run-started")
	defer span.End()

	payload := tasktypes.GetGroupKeyRunStartedTaskPayload{}
	metadata := tasktypes.GetGroupKeyRunStartedTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode group key run started task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode group key run started task metadata: %w", err)
	}

	// update the get group key run in the database
	startedAt, err := time.Parse(time.RFC3339, payload.StartedAt)

	if err != nil {
		return fmt.Errorf("could not parse started at: %w", err)
	}

	_, err = ec.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(metadata.TenantId, payload.GetGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
		StartedAt: &startedAt,
		Status:    repository.StepRunStatusPtr(db.StepRunStatusRunning),
	})

	return err
}

func (wc *WorkflowsControllerImpl) handleGroupKeyRunFinished(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-group-key-run-finished")
	defer span.End()

	payload := tasktypes.GetGroupKeyRunFinishedTaskPayload{}
	metadata := tasktypes.GetGroupKeyRunFinishedTaskMetadata{}

	err := wc.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode group key run finished task payload: %w", err)
	}

	err = wc.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode group key run finished task metadata: %w", err)
	}

	// update the group key run in the database
	finishedAt, err := time.Parse(time.RFC3339, payload.FinishedAt)

	if err != nil {
		return fmt.Errorf("could not parse started at: %w", err)
	}

	groupKeyRun, err := wc.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(metadata.TenantId, payload.GetGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
		FinishedAt: &finishedAt,
		Status:     repository.StepRunStatusPtr(db.StepRunStatusSucceeded),
		Output:     &payload.GroupKey,
	})

	if err != nil {
		return fmt.Errorf("could not update group key run: %w", err)
	}

	errGroup := new(errgroup.Group)

	errGroup.Go(func() error {
		workflowVersionId := sqlchelpers.UUIDToStr(groupKeyRun.WorkflowVersionId)
		workflowVersion, err := wc.repo.Workflow().GetWorkflowVersionById(metadata.TenantId, workflowVersionId)

		if err != nil {
			return fmt.Errorf("could not get workflow version: %w", err)
		}

		concurrency, _ := workflowVersion.Concurrency()

		switch concurrency.LimitStrategy {
		case db.ConcurrencyLimitStrategyCancelInProgress:
			err = wc.queueByCancelInProgress(ctx, metadata.TenantId, payload.GroupKey, workflowVersion)
		case db.ConcurrencyLimitStrategyGroupRoundRobin:
			err = wc.queueByGroupRoundRobin(ctx, metadata.TenantId, workflowVersion)
		default:
			return fmt.Errorf("unimplemented concurrency limit strategy: %s", concurrency.LimitStrategy)
		}

		return err
	})

	// cancel the timeout task
	errGroup.Go(func() error {
		err = wc.mq.AddMessage(
			ctx,
			msgqueue.QueueTypeFromTickerID(sqlchelpers.UUIDToStr(groupKeyRun.GetGroupKeyRun.TickerId)),
			cancelGetGroupKeyRunTimeoutTask(groupKeyRun),
		)

		if err != nil {
			return fmt.Errorf("could not add cancel group key run timeout task to task queue: %w", err)
		}

		return nil
	})

	return errGroup.Wait()
}

func (wc *WorkflowsControllerImpl) handleGroupKeyRunFailed(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpan(ctx, "handle-group-key-run-failed")
	defer span.End()

	payload := tasktypes.GetGroupKeyRunFailedTaskPayload{}
	metadata := tasktypes.GetGroupKeyRunFailedTaskMetadata{}

	err := wc.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode group key run failed task payload: %w", err)
	}

	err = wc.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode group key run failed task metadata: %w", err)
	}

	// update the group key run in the database
	failedAt, err := time.Parse(time.RFC3339, payload.FailedAt)
	if err != nil {
		return fmt.Errorf("could not parse started at: %w", err)
	}

	groupKeyRun, err := wc.repo.GetGroupKeyRun().UpdateGetGroupKeyRun(metadata.TenantId, payload.GetGroupKeyRunId, &repository.UpdateGetGroupKeyRunOpts{
		FinishedAt: &failedAt,
		Error:      &payload.Error,
		Status:     repository.StepRunStatusPtr(db.StepRunStatusFailed),
	})

	if err != nil {
		return fmt.Errorf("could not update get group key run: %w", err)
	}

	// cancel the ticker for the group key run
	err = wc.mq.AddMessage(
		ctx,
		msgqueue.QueueTypeFromTickerID(sqlchelpers.UUIDToStr(groupKeyRun.GetGroupKeyRun.TickerId)),
		cancelGetGroupKeyRunTimeoutTask(groupKeyRun),
	)

	if err != nil {
		return fmt.Errorf("could not add cancel group key run timeout task to task queue: %w", err)
	}

	return nil
}

func cancelGetGroupKeyRunTimeoutTask(getGroupKeyRun *dbsqlc.GetGroupKeyRunForEngineRow) *msgqueue.Message {
	payload, _ := datautils.ToJSONMap(tasktypes.CancelGetGroupKeyRunTimeoutTaskPayload{
		GetGroupKeyRunId: sqlchelpers.UUIDToStr(getGroupKeyRun.GetGroupKeyRun.ID),
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.CancelGetGroupKeyRunTimeoutTaskMetadata{
		TenantId: sqlchelpers.UUIDToStr(getGroupKeyRun.GetGroupKeyRun.TenantId),
	})

	return &msgqueue.Message{
		ID:       "cancel-get-group-key-run-timeout",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}
}
