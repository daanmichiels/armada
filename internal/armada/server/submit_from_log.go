package server

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/hashicorp/go-multierror"
	pool "github.com/jolestar/go-commons-pool"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/armadaproject/armada/internal/armada/repository"
	"github.com/armadaproject/armada/internal/common/armadacontext"
	"github.com/armadaproject/armada/internal/common/armadaerrors"
	"github.com/armadaproject/armada/internal/common/compress"
	"github.com/armadaproject/armada/internal/common/eventutil"
	"github.com/armadaproject/armada/internal/common/logging"
	"github.com/armadaproject/armada/internal/common/schedulers"
	"github.com/armadaproject/armada/internal/common/util"
	"github.com/armadaproject/armada/pkg/api"
	"github.com/armadaproject/armada/pkg/armadaevents"
)

// SubmitFromLog is a service that reads messages from Pulsar and updates the state of the Armada server accordingly
// (in particular, it writes to Redis).
// Calls into an embedded Armada submit server object.
type SubmitFromLog struct {
	SubmitServer *SubmitServer
	Consumer     pulsar.Consumer
	// Logger from which the loggers used by this service are derived
	// (e.g., using srv.Logger.WithField), or nil, in which case the global logrus logger is used.
	Logger *logrus.Entry
}

// Run the service that reads from Pulsar and updates Armada until the provided context is cancelled.
func (srv *SubmitFromLog) Run(ctx *armadacontext.Context) error {
	// Get the configured logger, or the standard logger if none is provided.
	log := srv.getLogger()
	log.Info("service started")

	// Recover from panics by restarting the service.
	defer func() {
		if err := recover(); err != nil {
			log.WithField("error", err).Error("unexpected panic; restarting")
			time.Sleep(time.Second)
			go func() {
				if err := srv.Run(ctx); err != nil {
					logging.WithStacktrace(log, err).Error("service failure")
				}
			}()
		} else {
			// An expected shutdown.
			log.Info("service stopped")
		}
	}()

	// Periodically log the number of processed messages.
	logInterval := 10 * time.Second
	lastLogged := time.Now()
	numReceived := 0
	numErrored := 0
	var lastMessageId pulsar.MessageID
	lastMessageId = nil
	lastPublishTime := time.Now()

	// Run until ctx is cancelled.
	for {

		// Periodic logging.
		if time.Since(lastLogged) > logInterval {
			log.WithFields(
				logrus.Fields{
					"received":      numReceived,
					"succeeded":     numReceived - numErrored,
					"errored":       numErrored,
					"interval":      logInterval,
					"lastMessageId": lastMessageId,
					"timeLag":       time.Since(lastPublishTime),
				},
			).Info("message statistics")
			numReceived = 0
			numErrored = 0
			lastLogged = time.Now()
		}

		// Exit if the context has been cancelled. Otherwise, get a message from Pulsar.
		select {
		case <-ctx.Done():
			return nil
		default:

			// Get a message from Pulsar, which consists of a sequence of events (i.e., state transitions).
			ctxWithTimeout, cancel := armadacontext.WithTimeout(ctx, 10*time.Second)
			msg, err := srv.Consumer.Receive(ctxWithTimeout)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				break // expected
			}

			// If receiving fails, try again in the hope that the problem is transient.
			// We don't need to distinguish between errors here, since any error means this function can't proceed.
			if err != nil {
				logging.WithStacktrace(log, err).WithField("lastMessageId", lastMessageId).Warnf("Pulsar receive failed; backing off")
				time.Sleep(100 * time.Millisecond)
				break
			}

			// If this message isn't for us we can simply ack it
			// and go to the next message
			if !schedulers.ForLegacyScheduler(msg) {
				srv.ack(ctx, msg)
				break
			}

			lastMessageId = msg.ID()
			lastPublishTime = msg.PublishTime()
			numReceived++

			ctxWithLogger := armadacontext.WithLogField(ctx, "messageId", msg.ID())

			// Unmarshal and validate the message.
			sequence, err := eventutil.UnmarshalEventSequence(ctxWithLogger, msg.Payload())
			if err != nil {
				srv.ack(ctx, msg)
				logging.WithStacktrace(ctxWithLogger, err).Warnf("processing message failed; ignoring")
				numErrored++
				break
			}

			ctxWithLogger.WithField("numEvents", len(sequence.Events)).Info("processing sequence")
			// TODO: Improve retry logic.
			srv.ProcessSequence(ctxWithLogger, sequence)
			srv.ack(ctx, msg)
		}
	}
}

// ProcessSequence processes all events in a particular sequence.
// For efficiency, we may process several events at a time.
// To maintain ordering, we only do so for subsequences of consecutive events of equal type.
// The returned bool indicates if the corresponding Pulsar message should be ack'd or not.
func (srv *SubmitFromLog) ProcessSequence(ctx *armadacontext.Context, sequence *armadaevents.EventSequence) bool {
	// Sub-functions should always increment the events index unless they experience a transient error.
	// However, if a permanent error is mis-categorised as transient, we may get stuck forever.
	// To avoid that issue, we return immediately if timeout time has passed
	// (i.e., if processing a sequence takes more than timeout time, some events may be ignored).
	// If no events were processed, the corresponding Pulsar message won't be ack'd.
	timeout := 5 * time.Minute
	lastProgress := time.Now()

	i := 0
	for i < len(sequence.Events) && time.Since(lastProgress) < timeout {
		j, err := srv.ProcessSubSequence(ctx, i, sequence)
		if err != nil {
			logging.WithStacktrace(ctx, err).WithFields(logrus.Fields{"lowerIndex": i, "upperIndex": j}).Warnf("processing subsequence failed; ignoring")
		}

		if j == i {
			ctx.WithFields(logrus.Fields{"lowerIndex": i, "upperIndex": j}).Info("made no progress")

			// We should only get here if a transient error occurs.
			// Sleep for a bit before retrying.
			time.Sleep(time.Second)
		} else {
			lastProgress = time.Now()
		}
		i = j
	}

	// To avoid applying the same event more than once, ack messages if at least 1 event was applied.
	// Or if the sequence contained no events.
	return i > 0 || len(sequence.Events) == 0
}

// ProcessSubSequence processes sequence.Events[i:j-1], where j is the index of the first event in the sequence
// of a type different from that of sequence.Events[i], or len(sequence.Events) if no such event exists in the sequence,
// and returns j.
//
// Processing one such subsequence at a time preserves ordering between events of different types.
// For example, SubmitJob events are processed before CancelJob events that occur later in the sequence.
//
// Events are processed by calling into the embedded srv.SubmitServer.
//
// Not all events are handled by this processor since the legacy scheduler writes some transitions directly to the db.
func (srv *SubmitFromLog) ProcessSubSequence(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) (j int, err error) {
	j = i // Initially, the next event to be processed is i.
	if i < 0 || i >= len(sequence.Events) {
		err = &armadaerrors.ErrInvalidArgument{
			Name:    "i",
			Value:   i,
			Message: fmt.Sprintf("tried to index into a list composed of %d elements", len(sequence.Events)),
		}
		err = errors.WithStack(err)
		return
	}

	// Process the subsequence starting at the i-th event consisting of all consecutive events of the same type.
	var ok bool
	switch sequence.Events[i].Event.(type) {
	case *armadaevents.EventSequence_Event_SubmitJob:
		es := collectJobSubmitEvents(ctx, i, sequence)
		ok, err = srv.SubmitJobs(ctx, sequence.UserId, sequence.Groups, sequence.Queue, sequence.JobSetName, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_CancelJob:
		es := collectCancelJobEvents(ctx, i, sequence)
		ok, err = srv.CancelJobs(ctx, sequence.UserId, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_CancelJobSet:
		es := collectCancelJobSetEvents(ctx, i, sequence)
		ok, err = srv.CancelJobSets(ctx, sequence.UserId, sequence.Queue, sequence.JobSetName, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_ReprioritiseJob:
		es := collectReprioritiseJobEvents(ctx, i, sequence)
		ok, err = srv.ReprioritizeJobs(ctx, sequence.UserId, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_ReprioritiseJobSet:
		es := collectReprioritiseJobSetEvents(ctx, i, sequence)
		ok, err = srv.ReprioritizeJobSets(ctx, sequence.UserId, sequence.Queue, sequence.JobSetName, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_JobRunRunning:
		es := collectEvents[*armadaevents.EventSequence_Event_JobRunRunning](ctx, i, sequence)
		ok, err = srv.UpdateJobStartTimes(ctx, es)
		if ok {
			j = i + len(es)
		}
	case *armadaevents.EventSequence_Event_JobErrors:
		es := collectEvents[*armadaevents.EventSequence_Event_JobErrors](ctx, i, sequence)
		ok, err = srv.DeleteFailedJobs(ctx, es)
		if ok {
			j = i + len(es)
		}
	default:
		// Assign to j the index of the next event in the sequence with type different from sequence.Events[i],
		// or len(sequence.Events) if no such element exists, so that processing won't be attempted for this type again.
		j = i
		t := reflect.TypeOf(sequence.Events[i].Event)
		for j < len(sequence.Events) && reflect.TypeOf(sequence.Events[j].Event) == t {
			j++
		}
		err = nil
	}
	return
}

// collectJobSubmitEvents (and the corresponding functions for other types below)
// return a slice of events starting at index i in the sequence with equal type.
func collectJobSubmitEvents(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.SubmitJob {
	result := make([]*armadaevents.SubmitJob, 0)
	for j := i; j < len(sequence.Events); j++ {
		if e, ok := sequence.Events[j].Event.(*armadaevents.EventSequence_Event_SubmitJob); ok {
			result = append(result, e.SubmitJob)
		} else {
			break
		}
	}
	return result
}

func collectCancelJobEvents(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.CancelJob {
	result := make([]*armadaevents.CancelJob, 0)
	for j := i; j < len(sequence.Events); j++ {
		if e, ok := sequence.Events[j].Event.(*armadaevents.EventSequence_Event_CancelJob); ok {
			result = append(result, e.CancelJob)
		} else {
			break
		}
	}
	return result
}

func collectCancelJobSetEvents(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.CancelJobSet {
	result := make([]*armadaevents.CancelJobSet, 0)
	for j := i; j < len(sequence.Events); j++ {
		if e, ok := sequence.Events[j].Event.(*armadaevents.EventSequence_Event_CancelJobSet); ok {
			result = append(result, e.CancelJobSet)
		} else {
			break
		}
	}
	return result
}

func collectReprioritiseJobEvents(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.ReprioritiseJob {
	result := make([]*armadaevents.ReprioritiseJob, 0)
	for j := i; j < len(sequence.Events); j++ {
		if e, ok := sequence.Events[j].Event.(*armadaevents.EventSequence_Event_ReprioritiseJob); ok {
			result = append(result, e.ReprioritiseJob)
		} else {
			break
		}
	}
	return result
}

func collectReprioritiseJobSetEvents(ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.ReprioritiseJobSet {
	result := make([]*armadaevents.ReprioritiseJobSet, 0)
	for j := i; j < len(sequence.Events); j++ {
		if e, ok := sequence.Events[j].Event.(*armadaevents.EventSequence_Event_ReprioritiseJobSet); ok {
			result = append(result, e.ReprioritiseJobSet)
		} else {
			break
		}
	}
	return result
}

func collectEvents[T any](ctx *armadacontext.Context, i int, sequence *armadaevents.EventSequence) []*armadaevents.EventSequence_Event {
	events := make([]*armadaevents.EventSequence_Event, 0)
	for j := i; j < len(sequence.Events); j++ {
		if _, ok := sequence.Events[j].Event.(T); ok {
			events = append(events, sequence.Events[j])
		} else {
			break
		}
	}
	return events
}

func (srv *SubmitFromLog) getLogger() *logrus.Entry {
	var log *logrus.Entry
	if srv.Logger != nil {
		log = srv.Logger.WithField("service", "SubmitFromLog")
	} else {
		log = logrus.StandardLogger().WithField("service", "SubmitFromLog")
	}
	return log
}

// SubmitJobs processes several job submit events in bulk.
// It returns a boolean indicating if the events were processed and any error that occurred during processing.
// Specifically, events are not processed if writing to the database results in a network-related error.
// For any other error, the jobs are marked as failed and the events are considered to have been processed.
func (srv *SubmitFromLog) SubmitJobs(
	ctx *armadacontext.Context,
	userId string,
	groups []string,
	queueName string,
	jobSetName string,
	es []*armadaevents.SubmitJob,
) (bool, error) {
	// Convert Pulsar jobs to legacy api jobs.
	// We can't report job failure on error here, since the job failure message bundles the job struct.
	// Hence, if an error occurs here, the job disappears from the point of view of the user.
	// However, this code path is exercised when jobs are submitted to the log so errors should be rare.
	jobs, err := eventutil.ApiJobsFromLogSubmitJobs(userId, groups, queueName, jobSetName, time.Now(), es)
	if err != nil {
		return true, err
	}

	log := srv.getLogger()
	compressor, err := srv.SubmitServer.compressorPool.BorrowObject(armadacontext.Background())
	if err != nil {
		return false, err
	}
	defer func(compressorPool *pool.ObjectPool, ctx *armadacontext.Context, object interface{}) {
		err := compressorPool.ReturnObject(ctx, object)
		if err != nil {
			log.WithError(err).Errorf("Error returning compressor to pool")
		}
	}(srv.SubmitServer.compressorPool, armadacontext.Background(), compressor)

	compressedOwnershipGroups, err := compress.CompressStringArray(groups, compressor.(compress.Compressor))
	if err != nil {
		return true, err
	}
	for _, job := range jobs {
		job.QueueOwnershipUserGroups = nil
		job.CompressedQueueOwnershipUserGroups = compressedOwnershipGroups
	}

	// Submit the jobs by writing them to the database.
	// If an error occurs here, there was a problem writing to the database and we mark all jobs as failed.
	// Unless the error is network-related, in which case we return an error so that the caller can try again later.
	submissionResults, err := srv.SubmitServer.jobRepository.AddJobs(jobs)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		jobFailures := createJobFailuresWithReason(jobs, fmt.Sprintf("Failed to save job in Armada: %s", err))
		reportErr := reportFailed(srv.SubmitServer.eventStore, "", jobFailures)
		if reportErr != nil {
			err = errors.WithMessage(err, reportErr.Error())
		}
		return true, err
	}

	// Create events that report what happened.
	var createdJobs []*api.Job
	var jobFailures []*jobFailure
	var doubleSubmits []*repository.SubmitJobResult
	for i, submissionResult := range submissionResults {
		jobResponse := &api.JobSubmitResponseItem{JobId: submissionResult.JobId}
		if submissionResult.Error != nil {
			jobResponse.Error = submissionResult.Error.Error()
			jobFailures = append(jobFailures, &jobFailure{
				job:    jobs[i],
				reason: fmt.Sprintf("Failed to save job in Armada: %s", submissionResult.Error.Error()),
			})
		} else if submissionResult.AlreadyProcessed {
			log.Warnf("Already Processed job id %s, this job submission will be discarded", submissionResult.JobId)
		} else if submissionResult.DuplicateDetected {
			doubleSubmits = append(doubleSubmits, submissionResult)
		} else {
			createdJobs = append(createdJobs, jobs[i])
		}
	}

	// Write events to the database.
	// We consider the events to have been processed even if there are failures at this point.
	// If that happens, some messages may have gone missing.
	// The alternative would be to re-process the job submit events, which could result in duplicated jobs.
	var result *multierror.Error
	err = reportFailed(srv.SubmitServer.eventStore, "", jobFailures)
	result = multierror.Append(result, err)

	err = reportDuplicateDetected(srv.SubmitServer.eventStore, doubleSubmits)
	result = multierror.Append(result, err)

	err = reportQueued(srv.SubmitServer.eventStore, createdJobs)
	result = multierror.Append(result, err)

	return true, result.ErrorOrNil()
}

type CancelJobPayload struct {
	JobId  string
	Reason string
}

// CancelJobs cancels all jobs specified by the provided events in a single operation.
func (srv *SubmitFromLog) CancelJobs(ctx *armadacontext.Context, userId string, es []*armadaevents.CancelJob) (bool, error) {
	cancelJobPayloads := make([]*CancelJobPayload, len(es))
	for i, e := range es {
		id, err := armadaevents.UlidStringFromProtoUuid(e.JobId)
		if err != nil {
			// TODO: should we instead cancel the jobs we can here?
			return false, err
		}
		cancelJobPayloads[i] = &CancelJobPayload{
			JobId:  id,
			Reason: e.Reason,
		}
	}
	return srv.BatchedCancelJobsById(ctx, userId, cancelJobPayloads)
}

// CancelJobSets processes several CancelJobSet events.
// Because event sequences are specific to queue and job set, all CancelJobSet events in a sequence are equivalent,
// and we only need to call CancelJobSet once.
func (srv *SubmitFromLog) CancelJobSets(
	ctx *armadacontext.Context,
	userId string,
	queueName string,
	jobSetName string,
	events []*armadaevents.CancelJobSet,
) (bool, error) {
	// Get reason from first event
	reason := ""
	if len(events) > 0 {
		reason = events[0].Reason
	}
	return srv.CancelJobSet(ctx, userId, queueName, jobSetName, reason)
}

func (srv *SubmitFromLog) CancelJobSet(ctx *armadacontext.Context, userId string, queueName string, jobSetName string, reason string) (bool, error) {
	jobIds, err := srv.SubmitServer.jobRepository.GetActiveJobIds(queueName, jobSetName)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}
	cancelJobPayloads := util.Map(jobIds, func(jobId string) *CancelJobPayload {
		return &CancelJobPayload{
			JobId:  jobId,
			Reason: reason,
		}
	})
	return srv.BatchedCancelJobsById(ctx, userId, cancelJobPayloads)
}

func (srv *SubmitFromLog) BatchedCancelJobsById(ctx *armadacontext.Context, userId string, cancelJobPayloads []*CancelJobPayload) (bool, error) {
	// Split IDs into batches and process one batch at a time.
	// To reduce the number of jobs stored in memory.
	//
	// In case of network error, we indicate the events were not processed.
	// Because some batches may have already been processed, retrying may cause jobs to be cancelled multiple times.
	// However, that should be fine.
	batches := util.Batch(cancelJobPayloads, srv.SubmitServer.cancelJobsBatchSize)
	for _, batch := range batches {
		_, err := srv.CancelJobsById(ctx, userId, batch)
		if armadaerrors.IsNetworkError(err) {
			return false, err
		} else if err != nil {
			return true, err
		}

		// TODO I think the right way to do this is to include a timeout with the call to Redis
		// Then, we can check for a deadline exceeded error here
		if util.CloseToDeadline(ctx, time.Second*1) {
			err = errors.Errorf("deadline exceeded")
			return false, errors.WithStack(err)
		}
	}

	return true, nil
}

type CancelledJobPayload struct {
	job    *api.Job
	reason string
}

// CancelJobsById cancels all jobs with the specified ids.
func (srv *SubmitFromLog) CancelJobsById(ctx *armadacontext.Context, userId string, cancelJobPayloads []*CancelJobPayload) ([]string, error) {
	jobIdReasonMap := make(map[string]string)
	jobIds := util.Map(cancelJobPayloads, func(payload *CancelJobPayload) string {
		jobIdReasonMap[payload.JobId] = payload.Reason
		return payload.JobId
	})
	jobs, err := srv.SubmitServer.jobRepository.GetExistingJobsByIds(jobIds)
	if err != nil {
		return nil, err
	}

	err = reportJobsCancelling(srv.SubmitServer.eventStore, userId, jobs, "")
	if err != nil {
		return nil, err
	}

	deletionResult, err := srv.SubmitServer.jobRepository.DeleteJobs(jobs)
	if err != nil {
		return nil, err
	}

	// Check which jobs cancelled successfully.
	// Collect any errors into a multierror.
	var result *multierror.Error
	var cancelled []*CancelledJobPayload
	var cancelledIds []string
	for job, err := range deletionResult {
		if err != nil {
			result = multierror.Append(result, err)
		} else {
			reason := ""
			if r, ok := jobIdReasonMap[job.Id]; ok {
				reason = r
			}
			cancelled = append(cancelled, &CancelledJobPayload{
				job:    job,
				reason: reason,
			})
			cancelledIds = append(cancelledIds, job.Id)
		}
	}

	// Report the jobs that cancelled successfully.
	// Any error in doing so is a sibling to the errors with cancelling individual jobs.
	result = multierror.Append(result, reportJobsCancelled(srv.SubmitServer.eventStore, userId, cancelled))

	return cancelledIds, result.ErrorOrNil()
}

// ReprioritizeJobs updates the priority of one of more jobs.
func (srv *SubmitFromLog) ReprioritizeJobs(ctx *armadacontext.Context, userId string, es []*armadaevents.ReprioritiseJob) (bool, error) {
	if len(es) == 0 {
		return true, nil
	}

	jobIds := make([]string, len(es))
	for i, e := range es {
		id, err := armadaevents.UlidStringFromProtoUuid(e.JobId)
		if err != nil {
			// TODO: should we instead reprioritize the jobs we can here?
			return true, err
		}
		jobIds[i] = id
	}
	jobs, err := srv.SubmitServer.jobRepository.GetExistingJobsByIds(jobIds)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	// The submit API guarantees that all events specify the same priority.
	newPriority := es[0].Priority
	for _, e := range es {
		if e.Priority != newPriority {
			err = errors.Errorf("all ReprioritiseJob events must have the same priority")
			return true, errors.WithStack(err)
		}
	}

	err = reportJobsReprioritizing(srv.SubmitServer.eventStore, userId, jobs, float64(newPriority))
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	_, err = srv.SubmitServer.reprioritizeJobs(jobIds, float64(newPriority), userId)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	return true, nil
}

func (srv *SubmitFromLog) DeleteFailedJobs(ctx *armadacontext.Context, es []*armadaevents.EventSequence_Event) (bool, error) {
	jobIdsToDelete := make([]string, 0, len(es))
	for _, event := range es {
		jobErrors := event.GetJobErrors()
		if jobErrors == nil {
			continue
		}
		for _, err := range jobErrors.Errors {
			if err.Terminal {
				jobId, err := armadaevents.UlidStringFromProtoUuid(jobErrors.JobId)
				if err != nil {
					return false, err
				}
				jobIdsToDelete = append(jobIdsToDelete, jobId)
			}
		}
	}

	jobsToDelete, err := srv.SubmitServer.jobRepository.GetExistingJobsByIds(jobIdsToDelete)
	if err != nil {
		return false, err
	}
	if _, err := srv.SubmitServer.jobRepository.DeleteJobs(jobsToDelete); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateJobStartTimes records the start time (in Redis) of one of more jobs.
func (srv *SubmitFromLog) UpdateJobStartTimes(ctx *armadacontext.Context, es []*armadaevents.EventSequence_Event) (bool, error) {
	jobStartsInfos := make([]*repository.JobStartInfo, 0, len(es))
	for _, event := range es {
		jobRun := event.GetJobRunRunning()
		if jobRun == nil {
			continue
		}
		jobId, err := armadaevents.UlidStringFromProtoUuid(jobRun.GetJobId())
		if err != nil {
			logrus.WithError(err).Warnf("Invalid job id received when processing jobRunRunning Message")
			continue
		}

		if event.Created == nil {
			logrus.WithError(err).Warnf("Job run event for job %s has a missing timestamp.  Ignoring.", jobId)
			continue
		}
		clusterId := ""
		if len(jobRun.ResourceInfos) > 0 {
			clusterId = jobRun.ResourceInfos[0].GetObjectMeta().GetExecutorId()
		}
		jobStartsInfos = append(jobStartsInfos, &repository.JobStartInfo{
			JobId:     jobId,
			ClusterId: clusterId,
			StartTime: *event.Created,
		})
	}
	jobErrors, err := srv.SubmitServer.jobRepository.UpdateStartTime(jobStartsInfos)
	if err != nil {
		return false, err
	}

	var jobNotFoundError *repository.ErrJobNotFound
	allOk := true
	for _, jobErr := range jobErrors {
		if jobErr != nil && !errors.As(jobErr, &jobNotFoundError) {
			allOk = false
			err = jobErr
			break
		}
	}
	return allOk, err
}

// ReprioritizeJobSets updates the priority of several job sets.
// Returns a multierror containing all errors that occurred.
// Since repeating this operation is safe (setting the priority is idempotent),
// the bool indicating if events were processed is set to false if any job set failed.
func (srv *SubmitFromLog) ReprioritizeJobSets(
	ctx *armadacontext.Context,
	userId string,
	queueName string,
	jobSetName string,
	es []*armadaevents.ReprioritiseJobSet,
) (bool, error) {
	okResult := true
	var result *multierror.Error
	for _, e := range es {
		ok, err := srv.ReprioritizeJobSet(ctx, userId, queueName, jobSetName, e)
		okResult = ok && okResult
		result = multierror.Append(result, err)
	}
	return okResult, result.ErrorOrNil()
}

func (srv *SubmitFromLog) ReprioritizeJobSet(
	ctx *armadacontext.Context,
	userId string,
	queueName string,
	jobSetName string,
	e *armadaevents.ReprioritiseJobSet,
) (bool, error) {
	jobIds, err := srv.SubmitServer.jobRepository.GetActiveJobIds(queueName, jobSetName)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	jobs, err := srv.SubmitServer.jobRepository.GetExistingJobsByIds(jobIds)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	err = reportJobsReprioritizing(srv.SubmitServer.eventStore, userId, jobs, float64(e.Priority))
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	_, err = srv.SubmitServer.reprioritizeJobs(jobIds, float64(e.Priority), userId)
	if armadaerrors.IsNetworkError(err) {
		return false, err
	} else if err != nil {
		return true, err
	}

	return true, nil
}

func (srv *SubmitFromLog) ack(ctx *armadacontext.Context, msg pulsar.Message) {
	util.RetryUntilSuccess(
		ctx,
		func() error {
			return srv.Consumer.Ack(msg)
		},
		func(err error) {
			logrus.WithError(err).Warnf("Error acking pulsar message")
			time.Sleep(time.Second)
		},
	)
}
