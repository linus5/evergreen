package scheduler

import (
	"context"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/cloud"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mitchellh/mapstructure"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/mongodb/grip/sometimes"
	"github.com/pkg/errors"
)

const (
	// maximum turnaround we want to maintain for all hosts for a given distro
	MaxDurationPerDistroHost               = 30 * time.Minute
	MaxDurationPerDistroHostWithContainers = 2 * time.Minute
	dynamicDistroRuntimeAlertThreshold     = 24 * time.Hour
)

type Configuration struct {
	DistroID         string
	TaskFinder       string
	FreeHostFraction float64
}

func PlanDistro(ctx context.Context, conf Configuration, s *evergreen.Settings) error {
	schedulerInstanceID := util.RandomString()
	distro, err := distro.FindOne(distro.ById(conf.DistroID))
	if err != nil {
		return errors.Wrap(err, "problem finding distro")
	}

	if err = underwaterUnschedule(distro.Id); err != nil {
		return errors.Wrap(err, "problem unscheduling underwater tasks")
	}

	if distro.Disabled {
		grip.InfoWhen(sometimes.Quarter(), message.Fields{
			"message": "scheduling for distro is disabled",
			"runner":  RunnerName,
			"distro":  distro.Id,
		})
		return nil
	}

	////////////////////
	// task-finder phase
	////////////////////

	taskFindingBegins := time.Now()
	finder := GetTaskFinder(conf.TaskFinder)
	tasks, err := finder(distro.Id)
	if err != nil {
		return errors.Wrap(err, "problem calculating task finder")
	}
	grip.Info(message.Fields{
		"runner":        RunnerName,
		"distro":        distro.Id,
		"operation":     "runtime-stats",
		"phase":         "task-finder",
		"instance":      schedulerInstanceID,
		"duration_secs": time.Since(taskFindingBegins).Seconds(),
	})

	runnableTasks, versions, err := filterTasksWithVersionCache(tasks)
	if err != nil {
		return errors.Wrap(err, "error while filtering tasks against the versions' cache")
	}

	ds := &distroScheduler{
		TaskPrioritizer: &CmpBasedTaskPrioritizer{
			runtimeID: schedulerInstanceID,
		},
		TaskQueuePersister: &DBTaskQueuePersister{},
		runtimeID:          schedulerInstanceID,
	}

	// If the distro supports containers, get its container pool information.
	maxDurationThreshold := MaxDurationPerDistroHost
	var containerPool *evergreen.ContainerPool
	if distro.ContainerPool != "" {
		containerPool = s.ContainerPools.GetContainerPool(distro.ContainerPool)
		if containerPool == nil {
			return errors.Wrap(err, "problem retrieving container pool")
		}
		maxDurationThreshold = MaxDurationPerDistroHostWithContainers
	}

	////////////////////////
	// planning-distro phase
	////////////////////////

	planningPhaseBegins := time.Now()
	prioritizedTasks, err := ds.scheduleDistro(distro.Id, runnableTasks, versions, maxDurationThreshold)
	if err != nil {
		return errors.Wrap(err, "problem calculating distro plan")
	}

	grip.Info(message.Fields{
		"runner":        RunnerName,
		"distro":        distro.Id,
		"operation":     "runtime-stats",
		"phase":         "planning-distro",
		"instance":      schedulerInstanceID,
		"duration_secs": time.Since(planningPhaseBegins).Seconds(),
		"stat":          "distro-queue-size",
		"size":          len(prioritizedTasks),
	})

	return nil
}

func UpdateStaticDistro(d distro.Distro) error {
	if d.Provider != evergreen.ProviderNameStatic {
		return nil
	}

	hosts, err := doStaticHostUpdate(d)
	if err != nil {
		return errors.WithStack(err)
	}

	if d.Id == "" {
		return nil
	}

	return host.MarkInactiveStaticHosts(hosts, d.Id)
}

func doStaticHostUpdate(d distro.Distro) ([]string, error) {
	settings := &cloud.StaticSettings{}
	err := mapstructure.Decode(d.ProviderSettings, settings)
	if err != nil {
		return nil, errors.Errorf("invalid static settings for '%v'", d.Id)
	}

	staticHosts := []string{}
	for _, h := range settings.Hosts {
		hostInfo, err := util.ParseSSHInfo(h.Name)
		if err != nil {
			return nil, err
		}
		user := hostInfo.User
		if user == "" {
			user = d.User
		}
		staticHost := host.Host{
			Id:           h.Name,
			User:         user,
			Host:         h.Name,
			Distro:       d,
			CreationTime: time.Now(),
			StartedBy:    evergreen.User,
			Status:       evergreen.HostRunning,
			Provisioned:  true,
		}

		if d.Provider == evergreen.ProviderNameStatic {
			staticHost.Provider = evergreen.HostTypeStatic
		}

		// upsert the host
		_, err = staticHost.Upsert()
		if err != nil {
			return nil, err
		}
		staticHosts = append(staticHosts, h.Name)
	}

	return staticHosts, nil
}
