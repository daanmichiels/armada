package context

import (
	"fmt"
	"time"

	"github.com/openconfig/goyang/pkg/indent"
	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/time/rate"

	"github.com/armadaproject/armada/internal/armada/configuration"
	"github.com/armadaproject/armada/internal/common/armadaerrors"
	armadamaps "github.com/armadaproject/armada/internal/common/maps"
	armadaslices "github.com/armadaproject/armada/internal/common/slices"
	"github.com/armadaproject/armada/internal/common/types"
	"github.com/armadaproject/armada/internal/common/util"
	schedulerconfig "github.com/armadaproject/armada/internal/scheduler/configuration"
	"github.com/armadaproject/armada/internal/scheduler/fairness"
	"github.com/armadaproject/armada/internal/scheduler/interfaces"
	"github.com/armadaproject/armada/internal/scheduler/schedulerobjects"
)

// SchedulingContext contains information necessary for scheduling and records what happened in a scheduling round.
type SchedulingContext struct {
	// Time at which the scheduling cycle started.
	Started time.Time
	// Time at which the scheduling cycle finished.
	Finished time.Time
	// Executor for which we're currently scheduling jobs.
	ExecutorId string
	// Resource pool of this executor.
	Pool string
	// Allowed priority classes.
	PriorityClasses map[string]types.PriorityClass
	// Default priority class.
	DefaultPriorityClass string
	// Determines how fairness is computed.
	FairnessCostProvider fairness.CostProvider
	// Limits job scheduling rate globally across all queues.
	// Use the "Started" time to ensure limiter state remains constant within each scheduling round.
	Limiter *rate.Limiter
	// Sum of queue weights across all queues.
	WeightSum float64
	// Per-queue scheduling contexts.
	QueueSchedulingContexts map[string]*QueueSchedulingContext
	// Total resources across all clusters available at the start of the scheduling cycle.
	TotalResources schedulerobjects.ResourceList
	// Resources assigned across all queues during this scheduling cycle.
	ScheduledResources                schedulerobjects.ResourceList
	ScheduledResourcesByPriorityClass schedulerobjects.QuantityByTAndResourceType[string]
	// Resources evicted across all queues during this scheduling cycle.
	EvictedResources                schedulerobjects.ResourceList
	EvictedResourcesByPriorityClass schedulerobjects.QuantityByTAndResourceType[string]
	// Total number of successfully scheduled jobs.
	NumScheduledJobs int
	// Total number of successfully scheduled gangs.
	NumScheduledGangs int
	// Total number of evicted jobs.
	NumEvictedJobs int
	// TODO(reports): Count the number of evicted gangs.
	// Reason for why the scheduling round finished.
	TerminationReason string
	// Used to efficiently generate scheduling keys.
	SchedulingKeyGenerator *schedulerobjects.SchedulingKeyGenerator
	// Record of job scheduling requirements known to be unfeasible.
	// Used to immediately reject new jobs with identical requirements.
	// Maps to the JobSchedulingContext of a previous job attempted to schedule with the same key.
	UnfeasibleSchedulingKeys map[schedulerobjects.SchedulingKey]*JobSchedulingContext
}

func NewSchedulingContext(
	executorId string,
	pool string,
	priorityClasses map[string]types.PriorityClass,
	defaultPriorityClass string,
	fairnessCostProvider fairness.CostProvider,
	limiter *rate.Limiter,
	totalResources schedulerobjects.ResourceList,
) *SchedulingContext {
	return &SchedulingContext{
		Started:                           time.Now(),
		ExecutorId:                        executorId,
		Pool:                              pool,
		PriorityClasses:                   priorityClasses,
		DefaultPriorityClass:              defaultPriorityClass,
		FairnessCostProvider:              fairnessCostProvider,
		Limiter:                           limiter,
		QueueSchedulingContexts:           make(map[string]*QueueSchedulingContext),
		TotalResources:                    totalResources.DeepCopy(),
		ScheduledResources:                schedulerobjects.NewResourceListWithDefaultSize(),
		ScheduledResourcesByPriorityClass: make(schedulerobjects.QuantityByTAndResourceType[string]),
		EvictedResourcesByPriorityClass:   make(schedulerobjects.QuantityByTAndResourceType[string]),
		SchedulingKeyGenerator:            schedulerobjects.NewSchedulingKeyGenerator(),
		UnfeasibleSchedulingKeys:          make(map[schedulerobjects.SchedulingKey]*JobSchedulingContext),
	}
}

func (sctx *SchedulingContext) SchedulingKeyFromLegacySchedulerJob(job interfaces.LegacySchedulerJob) schedulerobjects.SchedulingKey {
	var priority int32
	if priorityClass, ok := sctx.PriorityClasses[job.GetPriorityClassName()]; ok {
		priority = priorityClass.Priority
	}
	return sctx.SchedulingKeyGenerator.Key(
		job.GetNodeSelector(),
		job.GetAffinity(),
		job.GetTolerations(),
		job.GetResourceRequirements().Requests,
		priority,
	)
}

func (sctx *SchedulingContext) ClearUnfeasibleSchedulingKeys() {
	sctx.UnfeasibleSchedulingKeys = make(map[schedulerobjects.SchedulingKey]*JobSchedulingContext)
}

func (sctx *SchedulingContext) AddQueueSchedulingContext(
	queue string, weight float64,
	initialAllocatedByPriorityClass schedulerobjects.QuantityByTAndResourceType[string],
	limiter *rate.Limiter,
) error {
	if _, ok := sctx.QueueSchedulingContexts[queue]; ok {
		return errors.WithStack(&armadaerrors.ErrInvalidArgument{
			Name:    "queue",
			Value:   queue,
			Message: fmt.Sprintf("there already exists a context for queue %s", queue),
		})
	}
	if initialAllocatedByPriorityClass == nil {
		initialAllocatedByPriorityClass = make(schedulerobjects.QuantityByTAndResourceType[string])
	} else {
		initialAllocatedByPriorityClass = initialAllocatedByPriorityClass.DeepCopy()
	}
	allocated := schedulerobjects.NewResourceListWithDefaultSize()
	for _, rl := range initialAllocatedByPriorityClass {
		allocated.Add(rl)
	}
	sctx.WeightSum += weight
	qctx := &QueueSchedulingContext{
		SchedulingContext:                 sctx,
		Created:                           time.Now(),
		ExecutorId:                        sctx.ExecutorId,
		Queue:                             queue,
		Weight:                            weight,
		Limiter:                           limiter,
		Allocated:                         allocated,
		AllocatedByPriorityClass:          initialAllocatedByPriorityClass,
		ScheduledResourcesByPriorityClass: make(schedulerobjects.QuantityByTAndResourceType[string]),
		EvictedResourcesByPriorityClass:   make(schedulerobjects.QuantityByTAndResourceType[string]),
		SuccessfulJobSchedulingContexts:   make(map[string]*JobSchedulingContext),
		UnsuccessfulJobSchedulingContexts: make(map[string]*JobSchedulingContext),
		EvictedJobsById:                   make(map[string]bool),
	}
	sctx.QueueSchedulingContexts[queue] = qctx
	return nil
}

func (sctx *SchedulingContext) String() string {
	return sctx.ReportString(0)
}

// GetQueue is necessary to implement the fairness.QueueRepository interface.
func (sctx *SchedulingContext) GetQueue(queue string) (fairness.Queue, bool) {
	qctx, ok := sctx.QueueSchedulingContexts[queue]
	return qctx, ok
}

// TotalCost returns the sum of the costs across all queues.
func (sctx *SchedulingContext) TotalCost() float64 {
	var rv float64
	for _, qctx := range sctx.QueueSchedulingContexts {
		rv += sctx.FairnessCostProvider.CostFromQueue(qctx)
	}
	return rv
}

func (sctx *SchedulingContext) ReportString(verbosity int32) string {
	w := util.NewTabbedStringBuilder(1, 1, 1, ' ', 0)
	w.Writef("Started:\t%s\n", sctx.Started)
	w.Writef("Finished:\t%s\n", sctx.Finished)
	w.Writef("Duration:\t%s\n", sctx.Finished.Sub(sctx.Started))
	w.Writef("Termination reason:\t%s\n", sctx.TerminationReason)
	w.Writef("Total capacity:\t%s\n", sctx.TotalResources.CompactString())
	w.Writef("Scheduled resources:\t%s\n", sctx.ScheduledResources.CompactString())
	w.Writef("Preempted resources:\t%s\n", sctx.EvictedResources.CompactString())
	w.Writef("Number of gangs scheduled:\t%d\n", sctx.NumScheduledGangs)
	w.Writef("Number of jobs scheduled:\t%d\n", sctx.NumScheduledJobs)
	w.Writef("Number of jobs preempted:\t%d\n", sctx.NumEvictedJobs)
	scheduled := armadamaps.Filter(
		sctx.QueueSchedulingContexts,
		func(_ string, qctx *QueueSchedulingContext) bool {
			return len(qctx.SuccessfulJobSchedulingContexts) > 0
		},
	)
	if verbosity <= 0 {
		w.Writef("Scheduled queues:\t%v\n", maps.Keys(scheduled))
	} else {
		w.Write("Scheduled queues:\n")
		for queueName, qctx := range scheduled {
			w.Writef("\t%s:\n", queueName)
			w.Write(indent.String("\t\t", qctx.ReportString(verbosity-2)))
		}
	}
	preempted := armadamaps.Filter(
		sctx.QueueSchedulingContexts,
		func(_ string, qctx *QueueSchedulingContext) bool {
			return len(qctx.EvictedJobsById) > 0
		},
	)
	if verbosity <= 0 {
		w.Writef("Preempted queues:\t%v\n", maps.Keys(preempted))
	} else {
		w.Write("Preempted queues:\n")
		for queueName, qctx := range preempted {
			w.Writef("\t%s:\n", queueName)
			w.Writef(indent.String("\t\t", qctx.ReportString(verbosity-2)))
		}
	}
	return w.String()
}

func (sctx *SchedulingContext) AddGangSchedulingContext(gctx *GangSchedulingContext) (bool, error) {
	allJobsEvictedInThisRound := true
	allJobsSuccessful := true
	for _, jctx := range gctx.JobSchedulingContexts {
		evictedInThisRound, err := sctx.AddJobSchedulingContext(jctx)
		if err != nil {
			return false, err
		}
		allJobsEvictedInThisRound = allJobsEvictedInThisRound && evictedInThisRound
		allJobsSuccessful = allJobsSuccessful && jctx.IsSuccessful()
	}
	if allJobsSuccessful && !allJobsEvictedInThisRound {
		sctx.NumScheduledGangs++
	}
	return allJobsEvictedInThisRound, nil
}

// AddJobSchedulingContext adds a job scheduling context.
// Automatically updates scheduled resources.
func (sctx *SchedulingContext) AddJobSchedulingContext(jctx *JobSchedulingContext) (bool, error) {
	qctx, ok := sctx.QueueSchedulingContexts[jctx.Job.GetQueue()]
	if !ok {
		return false, errors.Errorf("failed adding job %s to scheduling context: no context for queue %s", jctx.JobId, jctx.Job.GetQueue())
	}
	evictedInThisRound, err := qctx.AddJobSchedulingContext(jctx)
	if err != nil {
		return false, err
	}
	if jctx.IsSuccessful() {
		if evictedInThisRound {
			sctx.EvictedResources.SubV1ResourceList(jctx.PodRequirements.ResourceRequirements.Requests)
			sctx.EvictedResourcesByPriorityClass.SubV1ResourceList(jctx.Job.GetPriorityClassName(), jctx.PodRequirements.ResourceRequirements.Requests)
			sctx.NumEvictedJobs--
		} else {
			sctx.ScheduledResources.AddV1ResourceList(jctx.PodRequirements.ResourceRequirements.Requests)
			sctx.ScheduledResourcesByPriorityClass.AddV1ResourceList(jctx.Job.GetPriorityClassName(), jctx.PodRequirements.ResourceRequirements.Requests)
			sctx.NumScheduledJobs++
		}
	}
	return evictedInThisRound, nil
}

func (sctx *SchedulingContext) EvictGang(jobs []interfaces.LegacySchedulerJob) (bool, error) {
	allJobsScheduledInThisRound := true
	for _, job := range jobs {
		scheduledInThisRound, err := sctx.EvictJob(job)
		if err != nil {
			return false, err
		}
		allJobsScheduledInThisRound = allJobsScheduledInThisRound && scheduledInThisRound
	}
	if allJobsScheduledInThisRound {
		sctx.NumScheduledGangs--
	}
	return allJobsScheduledInThisRound, nil
}

func (sctx *SchedulingContext) EvictJob(job interfaces.LegacySchedulerJob) (bool, error) {
	qctx, ok := sctx.QueueSchedulingContexts[job.GetQueue()]
	if !ok {
		return false, errors.Errorf("failed evicting job %s from scheduling context: no context for queue %s", job.GetId(), job.GetQueue())
	}
	scheduledInThisRound, err := qctx.EvictJob(job)
	if err != nil {
		return false, err
	}
	rl := job.GetResourceRequirements().Requests
	if scheduledInThisRound {
		sctx.ScheduledResources.SubV1ResourceList(rl)
		sctx.ScheduledResourcesByPriorityClass.SubV1ResourceList(job.GetPriorityClassName(), rl)
		sctx.NumScheduledJobs--
	} else {
		sctx.EvictedResources.AddV1ResourceList(rl)
		sctx.EvictedResourcesByPriorityClass.AddV1ResourceList(job.GetPriorityClassName(), rl)
		sctx.NumEvictedJobs++
	}
	return scheduledInThisRound, nil
}

// ClearJobSpecs zeroes out job specs to reduce memory usage.
func (sctx *SchedulingContext) ClearJobSpecs() {
	for _, qctx := range sctx.QueueSchedulingContexts {
		qctx.ClearJobSpecs()
	}
}

func (sctx *SchedulingContext) SuccessfulJobSchedulingContexts() []*JobSchedulingContext {
	jctxs := make([]*JobSchedulingContext, 0)
	for _, qctx := range sctx.QueueSchedulingContexts {
		for _, jctx := range qctx.SuccessfulJobSchedulingContexts {
			jctxs = append(jctxs, jctx)
		}
	}
	return jctxs
}

// AllocatedByQueueAndPriority returns map from queue name and priority to resources allocated.
func (sctx *SchedulingContext) AllocatedByQueueAndPriority() map[string]schedulerobjects.QuantityByTAndResourceType[string] {
	rv := make(
		map[string]schedulerobjects.QuantityByTAndResourceType[string],
		len(sctx.QueueSchedulingContexts),
	)
	for queue, qctx := range sctx.QueueSchedulingContexts {
		if !qctx.AllocatedByPriorityClass.IsZero() {
			rv[queue] = qctx.AllocatedByPriorityClass.DeepCopy()
		}
	}
	return rv
}

// QueueSchedulingContext captures the decisions made by the scheduler during one invocation
// for a particular queue.
type QueueSchedulingContext struct {
	// The scheduling context to which this QueueSchedulingContext belongs.
	SchedulingContext *SchedulingContext
	// Time at which this context was created.
	Created time.Time
	// Executor this job was attempted to be assigned to.
	ExecutorId string
	// Queue name.
	Queue string
	// Determines the fair share of this queue relative to other queues.
	Weight float64
	// Limits job scheduling rate for this queue.
	// Use the "Started" time to ensure limiter state remains constant within each scheduling round.
	Limiter *rate.Limiter
	// Total resources assigned to the queue across all clusters by priority class priority.
	// Includes jobs scheduled during this invocation of the scheduler.
	Allocated schedulerobjects.ResourceList
	// Total resources assigned to the queue across all clusters by priority class.
	// Includes jobs scheduled during this invocation of the scheduler.
	AllocatedByPriorityClass schedulerobjects.QuantityByTAndResourceType[string]
	// Resources assigned to this queue during this scheduling cycle.
	ScheduledResourcesByPriorityClass schedulerobjects.QuantityByTAndResourceType[string]
	// Resources evicted from this queue during this scheduling cycle.
	EvictedResourcesByPriorityClass schedulerobjects.QuantityByTAndResourceType[string]
	// Job scheduling contexts associated with successful scheduling attempts.
	SuccessfulJobSchedulingContexts map[string]*JobSchedulingContext
	// Job scheduling contexts associated with unsuccessful scheduling attempts.
	UnsuccessfulJobSchedulingContexts map[string]*JobSchedulingContext
	// Jobs evicted in this round.
	EvictedJobsById map[string]bool
}

func (qctx *QueueSchedulingContext) String() string {
	return qctx.ReportString(0)
}

// GetAllocation is necessary to implement the fairness.Queue interface.
func (qctx *QueueSchedulingContext) GetAllocation() schedulerobjects.ResourceList {
	return qctx.Allocated
}

// GetWeight is necessary to implement the fairness.Queue interface.
func (qctx *QueueSchedulingContext) GetWeight() float64 {
	return qctx.Weight
}

const maxJobIdsToPrint = 1

func (qctx *QueueSchedulingContext) ReportString(verbosity int32) string {
	w := util.NewTabbedStringBuilder(1, 1, 1, ' ', 0)
	if verbosity >= 0 {
		w.Writef("Time:\t%s\n", qctx.Created)
		w.Writef("Queue:\t%s\n", qctx.Queue)
	}
	w.Writef("Scheduled resources:\t%s\n", qctx.ScheduledResourcesByPriorityClass.AggregateByResource().CompactString())
	w.Writef("Scheduled resources (by priority):\t%s\n", qctx.ScheduledResourcesByPriorityClass.String())
	w.Writef("Preempted resources:\t%s\n", qctx.EvictedResourcesByPriorityClass.AggregateByResource().CompactString())
	w.Writef("Preempted resources (by priority):\t%s\n", qctx.EvictedResourcesByPriorityClass.String())
	if verbosity >= 0 {
		w.Writef("Total allocated resources after scheduling:\t%s\n", qctx.Allocated.CompactString())
		w.Writef("Total allocated resources after scheduling by priority class:\t%s\n", qctx.AllocatedByPriorityClass)
		w.Writef("Number of jobs scheduled:\t%d\n", len(qctx.SuccessfulJobSchedulingContexts))
		w.Writef("Number of jobs preempted:\t%d\n", len(qctx.EvictedJobsById))
		w.Writef("Number of jobs that could not be scheduled:\t%d\n", len(qctx.UnsuccessfulJobSchedulingContexts))
		if len(qctx.SuccessfulJobSchedulingContexts) > 0 {
			jobIdsToPrint := maps.Keys(qctx.SuccessfulJobSchedulingContexts)
			if len(jobIdsToPrint) > maxJobIdsToPrint {
				jobIdsToPrint = jobIdsToPrint[0:maxJobIdsToPrint]
			}
			w.Writef("Scheduled jobs:\t%v", jobIdsToPrint)
			if len(jobIdsToPrint) != len(qctx.SuccessfulJobSchedulingContexts) {
				w.Writef(" (and %d others not shown)\n", len(qctx.SuccessfulJobSchedulingContexts)-len(jobIdsToPrint))
			} else {
				w.Write("\n")
			}
		}
		if len(qctx.EvictedJobsById) > 0 {
			jobIdsToPrint := maps.Keys(qctx.EvictedJobsById)
			if len(jobIdsToPrint) > maxJobIdsToPrint {
				jobIdsToPrint = jobIdsToPrint[0:maxJobIdsToPrint]
			}
			w.Writef("Preempted jobs:\t%v", jobIdsToPrint)
			if len(jobIdsToPrint) != len(qctx.EvictedJobsById) {
				w.Writef(" (and %d others not shown)\n", len(qctx.EvictedJobsById)-len(jobIdsToPrint))
			} else {
				w.Write("\n")
			}
		}
		if len(qctx.UnsuccessfulJobSchedulingContexts) > 0 {
			w.Write("Unschedulable jobs:\n")
			jobIdsByReason := armadaslices.MapAndGroupByFuncs(
				maps.Values(qctx.UnsuccessfulJobSchedulingContexts),
				func(jctx *JobSchedulingContext) string {
					return jctx.UnschedulableReason
				},
				func(jctx *JobSchedulingContext) string {
					return jctx.JobId
				},
			)
			reasons := maps.Keys(jobIdsByReason)
			slices.SortFunc(reasons, func(a, b string) bool { return len(jobIdsByReason[a]) < len(jobIdsByReason[b]) })
			for i := len(reasons) - 1; i >= 0; i-- {
				reason := reasons[i]
				jobIds := jobIdsByReason[reason]
				if len(jobIds) <= 0 {
					continue
				}
				w.Writef("\t%d:\t%s (e.g., %s)\n", len(jobIds), reason, jobIds[0])
			}
		}
	}
	return w.String()
}

func (qctx *QueueSchedulingContext) AddGangSchedulingContext(gctx *GangSchedulingContext) error {
	for _, jctx := range gctx.JobSchedulingContexts {
		if _, err := qctx.AddJobSchedulingContext(jctx); err != nil {
			return err
		}
	}
	return nil
}

// AddJobSchedulingContext adds a job scheduling context.
// Automatically updates scheduled resources.
func (qctx *QueueSchedulingContext) AddJobSchedulingContext(jctx *JobSchedulingContext) (bool, error) {
	if _, ok := qctx.SuccessfulJobSchedulingContexts[jctx.JobId]; ok {
		return false, errors.Errorf("failed adding job %s to queue: job already marked successful", jctx.JobId)
	}
	if _, ok := qctx.UnsuccessfulJobSchedulingContexts[jctx.JobId]; ok {
		return false, errors.Errorf("failed adding job %s to queue: job already marked unsuccessful", jctx.JobId)
	}
	_, evictedInThisRound := qctx.EvictedJobsById[jctx.JobId]
	if jctx.IsSuccessful() {
		if jctx.PodRequirements == nil {
			return false, errors.Errorf("failed adding job %s to queue: job requirements are missing", jctx.JobId)
		}

		// Always update ResourcesByPriority.
		// Since ResourcesByPriority is used to order queues by fraction of fair share.
		qctx.Allocated.AddV1ResourceList(jctx.PodRequirements.ResourceRequirements.Requests)
		qctx.AllocatedByPriorityClass.AddV1ResourceList(jctx.Job.GetPriorityClassName(), jctx.PodRequirements.ResourceRequirements.Requests)

		// Only if the job is not evicted, update ScheduledResourcesByPriority.
		// Since ScheduledResourcesByPriority is used to control per-round scheduling constraints.
		if evictedInThisRound {
			delete(qctx.EvictedJobsById, jctx.JobId)
			qctx.EvictedResourcesByPriorityClass.SubV1ResourceList(jctx.Job.GetPriorityClassName(), jctx.PodRequirements.ResourceRequirements.Requests)
		} else {
			qctx.SuccessfulJobSchedulingContexts[jctx.JobId] = jctx
			qctx.ScheduledResourcesByPriorityClass.AddV1ResourceList(jctx.Job.GetPriorityClassName(), jctx.PodRequirements.ResourceRequirements.Requests)
		}
	} else {
		qctx.UnsuccessfulJobSchedulingContexts[jctx.JobId] = jctx
	}
	return evictedInThisRound, nil
}

func (qctx *QueueSchedulingContext) EvictJob(job interfaces.LegacySchedulerJob) (bool, error) {
	jobId := job.GetId()
	if _, ok := qctx.UnsuccessfulJobSchedulingContexts[jobId]; ok {
		return false, errors.Errorf("failed evicting job %s from queue: job already marked unsuccessful", jobId)
	}
	if _, ok := qctx.EvictedJobsById[jobId]; ok {
		return false, errors.Errorf("failed evicting job %s from queue: job already marked evicted", jobId)
	}
	rl := job.GetResourceRequirements().Requests
	_, scheduledInThisRound := qctx.SuccessfulJobSchedulingContexts[jobId]
	if scheduledInThisRound {
		qctx.ScheduledResourcesByPriorityClass.SubV1ResourceList(job.GetPriorityClassName(), rl)
		delete(qctx.SuccessfulJobSchedulingContexts, jobId)
	} else {
		qctx.EvictedResourcesByPriorityClass.AddV1ResourceList(job.GetPriorityClassName(), rl)
		qctx.EvictedJobsById[jobId] = true
	}
	qctx.Allocated.SubV1ResourceList(rl)
	qctx.AllocatedByPriorityClass.SubV1ResourceList(job.GetPriorityClassName(), rl)
	return scheduledInThisRound, nil
}

// ClearJobSpecs zeroes out job specs to reduce memory usage.
func (qctx *QueueSchedulingContext) ClearJobSpecs() {
	for _, jctx := range qctx.SuccessfulJobSchedulingContexts {
		jctx.Job = nil
	}
	for _, jctx := range qctx.UnsuccessfulJobSchedulingContexts {
		jctx.Job = nil
	}
}

type GangSchedulingContext struct {
	Created               time.Time
	Queue                 string
	PriorityClassName     string
	JobSchedulingContexts []*JobSchedulingContext
	TotalResourceRequests schedulerobjects.ResourceList
	AllJobsEvicted        bool
	NodeUniformityLabel   string
}

func NewGangSchedulingContext(jctxs []*JobSchedulingContext) *GangSchedulingContext {
	// We assume that all jobs in a gang are in the same queue and have the same priority class
	// (which we enforce at job submission).
	queue := ""
	priorityClassName := ""
	nodeUniformityLabel := ""
	if len(jctxs) > 0 {
		queue = jctxs[0].Job.GetQueue()
		priorityClassName = jctxs[0].Job.GetPriorityClassName()
		if jctxs[0].PodRequirements != nil {
			nodeUniformityLabel = jctxs[0].PodRequirements.Annotations[configuration.GangNodeUniformityLabelAnnotation]
		}
	}
	allJobsEvicted := true
	totalResourceRequests := schedulerobjects.NewResourceList(4)
	for _, jctx := range jctxs {
		allJobsEvicted = allJobsEvicted && isEvictedJob(jctx.Job)
		totalResourceRequests.AddV1ResourceList(jctx.PodRequirements.ResourceRequirements.Requests)
	}
	return &GangSchedulingContext{
		Created:               time.Now(),
		Queue:                 queue,
		PriorityClassName:     priorityClassName,
		JobSchedulingContexts: jctxs,
		TotalResourceRequests: totalResourceRequests,
		AllJobsEvicted:        allJobsEvicted,
		NodeUniformityLabel:   nodeUniformityLabel,
	}
}

// Cardinality returns the number of jobs in the gang.
func (gctx *GangSchedulingContext) Cardinality() int {
	return len(gctx.JobSchedulingContexts)
}

func isEvictedJob(job interfaces.LegacySchedulerJob) bool {
	return job.GetAnnotations()[schedulerconfig.IsEvictedAnnotation] == "true"
}

// JobSchedulingContext is created by the scheduler and contains information
// about the decision made by the scheduler for a particular job.
type JobSchedulingContext struct {
	// Time at which this context was created.
	Created time.Time
	// Id of the job this pod corresponds to.
	JobId string
	// Job spec.
	Job interfaces.LegacySchedulerJob
	// Scheduling requirements of this job.
	// We currently require that each job contains exactly one pod spec.
	PodRequirements *schedulerobjects.PodRequirements
	// Reason for why the job could not be scheduled.
	// Empty if the job was scheduled successfully.
	UnschedulableReason string
	// Pod scheduling contexts for the individual pods that make up the job.
	PodSchedulingContext *PodSchedulingContext
}

func (jctx *JobSchedulingContext) String() string {
	w := util.NewTabbedStringBuilder(1, 1, 1, ' ', 0)
	w.Writef("Time:\t%s\n", jctx.Created)
	w.Writef("Job ID:\t%s\n", jctx.JobId)
	if jctx.UnschedulableReason != "" {
		w.Writef("UnschedulableReason:\t%s\n", jctx.UnschedulableReason)
	} else {
		w.Write("UnschedulableReason:\tnone\n")
	}
	if jctx.PodSchedulingContext != nil {
		w.Write(jctx.PodSchedulingContext.String())
	}
	return w.String()
}

func (jctx *JobSchedulingContext) IsSuccessful() bool {
	return jctx.UnschedulableReason == ""
}

func JobSchedulingContextsFromJobs[J interfaces.LegacySchedulerJob](priorityClasses map[string]types.PriorityClass, jobs []J) []*JobSchedulingContext {
	jctxs := make([]*JobSchedulingContext, len(jobs))
	timestamp := time.Now()
	for i, job := range jobs {
		jctxs[i] = &JobSchedulingContext{
			Created:         timestamp,
			JobId:           job.GetId(),
			Job:             job,
			PodRequirements: job.GetPodRequirements(priorityClasses),
		}
	}
	return jctxs
}

// PodSchedulingContext is returned by SelectAndBindNodeToPod and
// contains detailed information on the scheduling decision made for this pod.
type PodSchedulingContext struct {
	// Time at which this context was created.
	Created time.Time
	// ID of the node that the pod was assigned to, or empty.
	NodeId string
	// Score indicates how well the pod fits on the selected node.
	Score int
	// Priority class priority at which this pod was scheduled.
	// Only set if NodeId is.
	ScheduledAtPriority int32
	// Node types on which this pod could be scheduled.
	MatchingNodeTypes []*schedulerobjects.NodeType
	// Total number of nodes in the cluster when trying to schedule.
	NumNodes int
	// Number of nodes excluded by reason.
	NumExcludedNodesByReason map[string]int
}

func (pctx *PodSchedulingContext) String() string {
	w := util.NewTabbedStringBuilder(1, 1, 1, ' ', 0)
	if pctx.NodeId != "" {
		w.Writef("Node:\t%s\n", pctx.NodeId)
	} else {
		w.Write("Node:\tnone\n")
	}
	w.Writef("Number of nodes in cluster:\t%d\n", pctx.NumNodes)
	if len(pctx.NumExcludedNodesByReason) == 0 {
		w.Write("Excluded nodes:\tnone\n")
	} else {
		w.Write("Excluded nodes:\n")
		for reason, count := range pctx.NumExcludedNodesByReason {
			w.Writef("\t%d:\t%s\n", count, reason)
		}
	}
	return w.String()
}
