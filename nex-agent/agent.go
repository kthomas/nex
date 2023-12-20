package nexagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	agentapi "github.com/ConnectEverything/nex/agent-api"
	"github.com/ConnectEverything/nex/nex-agent/providers"
	"github.com/cloudevents/sdk-go/pkg/cloudevents"
	"github.com/nats-io/nats.go"
)

const NexAgentSubjectAdvertise = "agentint.advertise"

// Agent facilitates communication between the nex agent running in the firecracker VM
// and the nex node by way of a configured internal NATS server. Agent instances provide
// logging and event emission facilities and execute dispatched workloads
type Agent struct {
	agentLogs chan *agentapi.LogEntry
	eventLogs chan *cloudevents.Event

	cacheBucket nats.ObjectStore
	md          *agentapi.MachineMetadata
	nc          *nats.Conn
	started     time.Time
}

// InitAgent initializes a new agent to facilitate communications with
// the host node and dispatch workloads
func InitAgent() (*Agent, error) {
	metadata, err := GetMachineMetadata()
	if err != nil {
		return nil, err
	}

	nc, err := nats.Connect(fmt.Sprintf("nats://%s:%d", metadata.NodeNatsAddress, metadata.NodePort))
	if err != nil {
		return nil, err
	}

	js, err := nc.JetStream()
	if err != nil {
		return nil, err
	}

	bucket, err := js.ObjectStore(agentapi.WorkloadCacheBucket)
	if err != nil {
		return nil, err
	}

	return &Agent{
		agentLogs:   make(chan *agentapi.LogEntry),
		eventLogs:   make(chan *cloudevents.Event),
		cacheBucket: bucket,
		md:          metadata,
		nc:          nc,
		started:     time.Now().UTC(),
	}, nil
}

// Start the agent
func (a *Agent) Start() error {
	err := a.Advertise()
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("agentint.%s.workdispatch", a.md.VmId)
	_, err = a.nc.Subscribe(subject, a.handleWorkDispatched)
	if err != nil {
		a.LogError(fmt.Sprintf("Failed to subscribe to work dispatch: %s", err))
		return err
	}

	go a.dispatchEvents()
	go a.dispatchLogs()

	return nil
}

// Publish an initial message to the host indicating the agent is "all the way" up
func (a *Agent) Advertise() error {
	msg := agentapi.AdvertiseMessage{
		MachineId: a.md.VmId,
		StartTime: a.started,
		Message:   a.md.Message,
	}
	raw, _ := json.Marshal(msg)

	err := a.nc.Publish(NexAgentSubjectAdvertise, raw)
	if err != nil {
		a.LogError("Agent failed to publish initial advertise message")
		return err
	}

	err = a.nc.FlushTimeout(5 * time.Second)
	if err != nil {
		a.LogError("Agent failed to publish initial advertise message")
		return err
	}

	a.LogInfo("Agent is up")
	return nil
}

// Pull a RunRequest off the wire, get the payload from the shared
// bucket, write it to temp, initialize the execution provider per
// the work request, and then execute it
func (a *Agent) handleWorkDispatched(m *nats.Msg) {
	var request agentapi.WorkRequest
	err := json.Unmarshal(m.Data, &request)
	if err != nil {
		msg := fmt.Sprintf("Failed to unmarshal work request: %s", err)
		a.LogError(msg)
		a.workAck(m, false, msg)
		return
	}

	tmpFile, err := a.cacheExecutableArtifact(&request)
	if err != nil {
		a.workAck(m, false, err.Error())
		return
	}

	provider, err := providers.ExecutionProviderFactory(&agentapi.ExecutionProviderParams{
		WorkRequest: request,
		Stderr:      &logEmitter{stderr: true, name: request.WorkloadName, logs: a.agentLogs},
		Stdout:      &logEmitter{stderr: false, name: request.WorkloadName, logs: a.agentLogs},
		TmpFilename: *tmpFile,
		VmID:        a.md.VmId,
	})
	if err != nil {
		msg := fmt.Sprintf("Failed to initialize workload execution provider; %s", err)
		a.LogError(msg)
		a.workAck(m, false, msg)
		return
	}

	err = provider.Validate()
	if err != nil {
		a.LogError(fmt.Sprintf("Failed to validate workload: %s", err))
	}

	a.workAck(m, true, "Workload accepted")

	err = provider.Execute()
	if err != nil {
		a.LogError(fmt.Sprintf("Failed to execute workload: %s", err))
	}
}

// cacheExecutableArtifact uses the underlying agent configuration to fetch
// the executable workload artifact from the cache bucket, write it to a
// temporary file and make it executable; this method returns the full
// path to the cached artifact if successful
func (a *Agent) cacheExecutableArtifact(req *agentapi.WorkRequest) (*string, error) {
	tempFile := path.Join(os.TempDir(), "workload") // FIXME-- randomly generate a filename

	err := a.cacheBucket.GetFile(req.WorkloadName, tempFile)
	if err != nil {
		msg := fmt.Sprintf("Failed to write workload artifact to temp dir: %s", err)
		a.LogError(msg)
		return nil, errors.New(msg)
	}

	err = os.Chmod(tempFile, 0777)
	if err != nil {
		msg := fmt.Sprintf("Failed to set workload artifact as executable: %s", err)
		a.LogError(msg)
		return nil, errors.New(msg)
	}

	return &tempFile, nil
}

// workAck ACKs the provided NATS message by responding with the
// accepted status of the attempted work request and associated message
func (a *Agent) workAck(m *nats.Msg, accepted bool, msg string) error {
	ack := agentapi.WorkResponse{
		Accepted: accepted,
		Message:  msg,
	}

	bytes, err := json.Marshal(&ack)
	if err != nil {
		return err
	}

	err = m.Respond(bytes)
	if err != nil {
		a.LogError(fmt.Sprintf("Failed to acknowledge work dispatch: %s", err))
		return err
	}

	return nil
}