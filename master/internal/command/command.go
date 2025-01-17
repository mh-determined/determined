package command

import (
	"fmt"
	"math/rand"
	"net/url"
	"time"

	structpb "github.com/golang/protobuf/ptypes/struct"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/actor"
	"github.com/determined-ai/determined/master/pkg/actor/actors"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/container"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/commandv1"
	"github.com/determined-ai/determined/proto/pkg/notebookv1"
	"github.com/determined-ai/determined/proto/pkg/shellv1"
	"github.com/determined-ai/determined/proto/pkg/tensorboardv1"
)

// terminatedDuration defines the amount of time the command stays in a
// terminated state in the master before garbage collecting.
const terminatedDuration = 24 * time.Hour

// TODO: readinessCheck should be defined at the agent level. Temporarily we will use log
// messages as a proxy.
type readinessCheck func(sproto.ContainerLog) bool

// terminateForGC is an internal message indicating that the command actor
// should stop and garbage collect its state.
type terminateForGC struct{}

// commandOwner describes the owner of a command.
type commandOwner struct {
	ID       model.UserID `json:"id"`
	Username string       `json:"username"`
}

// DefaultConfig is the default configuration used by all
// commands (e.g., commands, notebooks, shells) if a request
// does not specify any configuration options.
func DefaultConfig(taskContainerDefaults *model.TaskContainerDefaultsConfig) model.CommandConfig {
	expConf := model.DefaultExperimentConfig(taskContainerDefaults)
	return model.CommandConfig{
		Resources: model.ResourcesConfig{
			Slots:  1,
			Weight: 1,
			// SlotsPerTrial is not used by commands. They prefer Slots instead.
			// It is only defined here to pass check.Validate.
			SlotsPerTrial: 1,
			Devices:       expConf.Resources.Devices,
		},
		Environment: expConf.Environment,
	}
}

// command is executed in a containerized environment on a Determined cluster.
type command struct {
	config model.CommandConfig

	owner          commandOwner
	agentUserGroup *model.AgentUserGroup
	taskSpec       *tasks.TaskSpec

	taskID               sproto.TaskID
	userFiles            archive.Archive
	additionalFiles      archive.Archive
	readinessChecks      map[string]readinessCheck
	readinessMessageSent bool
	metadata             map[string]interface{}
	serviceAddress       *string

	registeredTime time.Time
	task           *sproto.AllocateRequest
	container      *container.Container
	allocation     sproto.Allocation
	proxyNames     []string
	exitStatus     *string
	addresses      []container.Address

	db          *db.PgDB
	proxy       *actor.Ref
	eventStream *actor.Ref

	proxyTCP bool
}

// Receive implements the actor.Actor interface.
func (c *command) Receive(ctx *actor.Context) error {
	switch msg := ctx.Message().(type) {
	case actor.PreStart:
		c.registeredTime = ctx.Self().RegisteredTime()
		// Initialize an event stream manager.
		c.eventStream, _ = ctx.ActorOf("events", newEventManager())
		// Schedule the command with the cluster.
		c.proxy = ctx.Self().System().Get(actor.Addr("proxy"))

		c.task = &sproto.AllocateRequest{
			ID:             c.taskID,
			Name:           c.config.Description,
			SlotsNeeded:    c.config.Resources.Slots,
			Label:          c.config.Resources.AgentLabel,
			ResourcePool:   c.config.Resources.ResourcePool,
			NonPreemptible: true,
			FittingRequirements: sproto.FittingRequirements{
				SingleAgent: true,
			},
			TaskActor: ctx.Self(),
		}
		if err := ctx.Ask(sproto.GetRM(ctx.Self().System()), *c.task).Error(); err != nil {
			return err
		}
		ctx.Tell(sproto.GetRM(ctx.Self().System()), sproto.SetGroupPriority{
			Priority: c.config.Resources.Priority,
			Handler:  ctx.Self(),
		})
		ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), ScheduledEvent: &c.taskID})

	case actor.PostStop:
		c.terminate(ctx)

	case sproto.ResourcesAllocated:
		return c.receiveSchedulerMsg(ctx)

	case getSummary:
		if msg.userFilter == "" || c.owner.Username == msg.userFilter {
			ctx.Respond(newSummary(c))
		}

	case *notebookv1.Notebook:
		notebook, err := c.toNotebook(ctx)
		switch {
		case err != nil:
			ctx.Log().Error(err)
		default:
			ctx.Respond(notebook)
		}

	case *apiv1.GetNotebookRequest:
		notebook, err := c.toNotebook(ctx)
		switch {
		case err != nil:
			ctx.Log().Error(err)
		default:
			ctx.Respond(&apiv1.GetNotebookResponse{
				Notebook: notebook,
				Config:   protoutils.ToStruct(c.config),
			})
		}

	case *apiv1.KillNotebookRequest:
		notebook, err := c.toNotebook(ctx)
		switch {
		case err != nil:
			ctx.Log().Error(err)
		default:
			c.terminate(ctx)
			ctx.Respond(&apiv1.KillNotebookResponse{Notebook: notebook})
		}

	case *commandv1.Command:
		ctx.Respond(c.toCommand(ctx))

	case *apiv1.GetCommandRequest:
		ctx.Respond(&apiv1.GetCommandResponse{
			Command: c.toCommand(ctx),
			Config:  protoutils.ToStruct(c.config),
		})

	case *apiv1.KillCommandRequest:
		c.terminate(ctx)
		ctx.Respond(&apiv1.KillCommandResponse{Command: c.toCommand(ctx)})

	case *shellv1.Shell:
		ctx.Respond(c.toShell(ctx))

	case *apiv1.GetShellRequest:
		ctx.Respond(&apiv1.GetShellResponse{
			Shell:  c.toShell(ctx),
			Config: protoutils.ToStruct(c.config),
		})

	case *apiv1.KillShellRequest:
		c.terminate(ctx)
		ctx.Respond(&apiv1.KillShellResponse{Shell: c.toShell(ctx)})

	case *tensorboardv1.Tensorboard:
		ctx.Respond(c.toTensorboard(ctx))

	case *apiv1.GetTensorboardRequest:
		ctx.Respond(&apiv1.GetTensorboardResponse{Tensorboard: c.toTensorboard(ctx)})

	case *apiv1.KillTensorboardRequest:
		c.terminate(ctx)
		ctx.Respond(&apiv1.KillTensorboardResponse{Tensorboard: c.toTensorboard(ctx)})

	case sproto.TaskContainerStateChanged:
		c.container = &msg.Container

		switch {
		case msg.Container.State == container.Running:
			c.addresses = msg.ContainerStarted.Addresses

			names := make([]string, 0, len(c.addresses))
			for _, address := range c.addresses {
				// We are keying on task ID instead of container ID. Revisit this when we need to
				// proxy multi-container tasks or when containers are created prior to being
				// assigned to an agent.
				ctx.Ask(c.proxy, proxy.Register{
					ServiceID: string(c.taskID),
					URL: &url.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s:%d", address.HostIP, address.HostPort),
					},
					ProxyTCP: c.proxyTCP,
				})
				names = append(names, string(c.taskID))
			}
			c.proxyNames = names
			ctx.Tell(c.eventStream, event{
				Snapshot: newSummary(c), ContainerStartedEvent: msg.ContainerStarted,
			})

		case msg.Container.State == container.Terminated:
			for _, name := range c.proxyNames {
				ctx.Tell(c.proxy, proxy.Unregister{ServiceID: name})
			}
			c.proxyNames = make([]string, 0)

			exitStatus := "command exited successfully"
			if msg.ContainerStopped.Failure != nil {
				exitStatus = msg.ContainerStopped.Failure.Error()
			}

			c.exit(ctx, exitStatus)
		}

	case sproto.ContainerLog:
		if !c.readinessMessageSent && c.readinessChecksPass(ctx, msg) {
			c.readinessMessageSent = true
			ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), ServiceReadyEvent: &msg})
		}
		log := msg.String()
		ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), LogEvent: &log})

	case terminateForGC:
		ctx.Self().Stop()

	default:
		return actor.ErrUnexpectedMessage(ctx)
	}
	return nil
}

func (c *command) receiveSchedulerMsg(ctx *actor.Context) error {
	switch msg := ctx.Message().(type) {
	case sproto.ResourcesAllocated:
		// Ignore this message if the command has exited.
		if c.task == nil || msg.ID != c.task.ID {
			ctx.Log().Info("ignoring resource allocation since the command has exited.")
			return nil
		}

		check.Panic(check.Equal(len(msg.Allocations), 1,
			"Command should only receive an allocation of one container"))

		taskToken, err := c.db.StartTaskSession(string(c.task.ID))
		if err != nil {
			return errors.Wrap(err, "cannot start a new task session")
		}

		c.allocation = msg.Allocations[0]

		taskSpec := *c.taskSpec
		taskSpec.AgentUserGroup = c.agentUserGroup
		taskSpec.TaskToken = taskToken
		taskSpec.SetInner(&tasks.StartCommand{
			Config:          c.config,
			UserFiles:       c.userFiles,
			AdditionalFiles: c.additionalFiles,
		})
		msg.Allocations[0].Start(ctx, taskSpec)

		ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), AssignedEvent: &msg})

		// Evict the context from memory after starting the command as it is no longer needed. We
		// evict as soon as possible to prevent the master from hitting an OOM.
		// TODO: Consider not storing the userFiles in memory at all.
		c.userFiles = nil
		c.additionalFiles = nil

	default:
		return actor.ErrUnexpectedMessage(ctx)
	}
	return nil
}

// terminate handles the following cases of command termination:
// 1. Command is aborted before being allocated.
// 2. Forcible terminating a command by killing containers.
func (c *command) terminate(ctx *actor.Context) {
	if msg, ok := ctx.Message().(sproto.ReleaseResources); ok {
		ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), TerminateRequestEvent: &msg})
	}

	if c.allocation == nil {
		c.exit(ctx, "task is aborted without being scheduled")
	} else {
		ctx.Log().Info("task forcible terminating")
		c.allocation.Kill(ctx)
	}
}

// exit handles the following cases of command exiting:
// 1. Command is aborted before being allocated.
// 2. Forcible terminating a command by killing containers.
// 3. The command container exits itself.
func (c *command) exit(ctx *actor.Context, exitStatus string) {
	c.exitStatus = &exitStatus
	ctx.Tell(c.eventStream, event{Snapshot: newSummary(c), ExitedEvent: c.exitStatus})

	ctx.Tell(
		sproto.GetRM(ctx.Self().System()),
		sproto.ResourcesReleased{TaskActor: ctx.Self()},
	)
	actors.NotifyAfter(ctx, terminatedDuration, terminateForGC{})

	if c.task != nil {
		if err := c.db.DeleteTaskSessionByTaskID(string(c.task.ID)); err != nil {
			ctx.Log().WithError(err).Error("cannot delete task session for a command")
		}
	}
}

func (c *command) readinessChecksPass(ctx *actor.Context, log sproto.ContainerLog) bool {
	for name, check := range c.readinessChecks {
		if check(log) {
			delete(c.readinessChecks, name)
			ctx.Log().Infof("readiness check passed: %s", name)
		}
	}
	return len(c.readinessChecks) == 0
}

// State returns the command's state. This mirros the associated container's state
// if available.
func (c *command) State() State {
	state := Pending
	switch {
	case c.container != nil:
		switch c.container.State {
		case container.Assigned:
			state = Assigned
		case container.Pulling:
			state = Pulling
		case container.Starting:
			state = Starting
		case container.Running:
			state = Running
		case container.Terminated:
			state = Terminated
		}
	case c.exitStatus != nil:
		state = Terminated
	}
	return state
}

func (c *command) toNotebook(ctx *actor.Context) (*notebookv1.Notebook, error) {
	serviceAddress, err := generateServiceAddress(string(c.taskID))
	if err != nil {
		return nil, errors.Wrapf(err, "generating service address for %s", c.taskID)
	}

	exitStatus := protoutils.DefaultStringValue
	if c.exitStatus != nil {
		exitStatus = *c.exitStatus
	}

	return &notebookv1.Notebook{
		Id:             ctx.Self().Address().Local(),
		State:          c.State().Proto(),
		Description:    c.config.Description,
		Container:      c.container.Proto(),
		ServiceAddress: serviceAddress,
		StartTime:      protoutils.ToTimestamp(ctx.Self().RegisteredTime()),
		Username:       c.owner.Username,
		ResourcePool:   c.config.Resources.ResourcePool,
		ExitStatus:     exitStatus,
	}, nil
}

func (c *command) toCommand(ctx *actor.Context) *commandv1.Command {
	exitStatus := protoutils.DefaultStringValue
	if c.exitStatus != nil {
		exitStatus = *c.exitStatus
	}

	return &commandv1.Command{
		Id:           ctx.Self().Address().Local(),
		State:        c.State().Proto(),
		Description:  c.config.Description,
		Container:    c.container.Proto(),
		StartTime:    protoutils.ToTimestamp(ctx.Self().RegisteredTime()),
		Username:     c.owner.Username,
		ResourcePool: c.config.Resources.ResourcePool,
		ExitStatus:   exitStatus,
	}
}

func (c *command) toShell(ctx *actor.Context) *shellv1.Shell {
	exitStatus := protoutils.DefaultStringValue
	if c.exitStatus != nil {
		exitStatus = *c.exitStatus
	}

	addresses := make([]*structpb.Struct, 0)
	for _, addr := range c.addresses {
		addresses = append(addresses, protoutils.ToStruct(addr))
	}

	return &shellv1.Shell{
		Id:             ctx.Self().Address().Local(),
		State:          c.State().Proto(),
		Description:    c.config.Description,
		StartTime:      protoutils.ToTimestamp(ctx.Self().RegisteredTime()),
		Container:      c.container.Proto(),
		PrivateKey:     c.metadata["privateKey"].(string),
		PublicKey:      c.metadata["publicKey"].(string),
		Username:       c.owner.Username,
		ResourcePool:   c.config.Resources.ResourcePool,
		ExitStatus:     exitStatus,
		Addresses:      addresses,
		AgentUserGroup: protoutils.ToStruct(c.agentUserGroup),
	}
}

func (c *command) toTensorboard(ctx *actor.Context) *tensorboardv1.Tensorboard {
	exitStatus := protoutils.DefaultStringValue
	if c.exitStatus != nil {
		exitStatus = *c.exitStatus
	}

	var eids []int32
	for _, id := range c.metadata["experiment_ids"].([]int) {
		eids = append(eids, int32(id))
	}
	var tids []int32
	for _, id := range c.metadata["trial_ids"].([]int) {
		tids = append(tids, int32(id))
	}
	return &tensorboardv1.Tensorboard{
		Id:             ctx.Self().Address().Local(),
		State:          c.State().Proto(),
		Description:    c.config.Description,
		StartTime:      protoutils.ToTimestamp(ctx.Self().RegisteredTime()),
		Container:      c.container.Proto(),
		ServiceAddress: fmt.Sprintf(tensorboardServiceAddress, c.taskID),
		ExperimentIds:  eids,
		TrialIds:       tids,
		Username:       c.owner.Username,
		ResourcePool:   c.config.Resources.ResourcePool,
		ExitStatus:     exitStatus,
	}
}

func getPort(min, max int) int {
	return rand.Intn(max-min) + min
}

func setPodSpec(
	config *model.CommandConfig,
	taskContainerDefaults model.TaskContainerDefaultsConfig,
) {
	if config.Environment.PodSpec != nil {
		return
	}

	if config.Resources.Slots == 0 {
		config.Environment.PodSpec = taskContainerDefaults.CPUPodSpec
	} else {
		config.Environment.PodSpec = taskContainerDefaults.GPUPodSpec
	}
}
