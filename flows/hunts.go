// Manage in memory hunt replication.  For performance, the hunts
// table is mirrored in memory and refreshed periodically. The clients
// are then compared against it on each poll and hunts are dispatched
// as needed.
package flows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	errors "github.com/pkg/errors"
	actions_proto "www.velocidex.com/golang/velociraptor/actions/proto"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	"www.velocidex.com/golang/velociraptor/constants"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/datastore"
	flows_proto "www.velocidex.com/golang/velociraptor/flows/proto"
	"www.velocidex.com/golang/velociraptor/grpc_client"
	"www.velocidex.com/golang/velociraptor/logging"
	urns "www.velocidex.com/golang/velociraptor/urns"
)

var (
	dispatch_container = &HuntDispatcherContainer{}
)

type HuntDispatcher struct {
	config_obj     *api_proto.Config
	last_timestamp uint64
	hunts          []*api_proto.Hunt
}

func (self *HuntDispatcher) GetApplicableHunts(last_timestamp uint64) []*api_proto.Hunt {
	var result []*api_proto.Hunt

	for _, hunt := range self.hunts {
		if hunt.CreateTime > last_timestamp {
			result = append(result, hunt)
		}
	}
	return result
}

// Check all the hunts in our hunt list for pending clients that
// should have been added.

// Clients are added to hunts in a pre-determined rate (e.g. 20
// clients/min), therefore we need to manage how many clients to be
// added to each hunt. The foreman adds clients to the pending queue
// and the HuntManager takes clients from the pending queue and adds
// them to the running queue at the pre-determined rate.
func (self *HuntDispatcher) Update() error {
	logger := logging.GetLogger(self.config_obj, &logging.FrontendComponent)
	db, err := datastore.GetDB(self.config_obj)
	if err != nil {
		return err
	}
	for _, hunt := range self.hunts {
		// If the hunt is not in the running state we do not
		// schedule new clients for it.
		if hunt.State != api_proto.Hunt_RUNNING {
			continue
		}
		modified, err := self._ScheduleClientsForHunt(hunt)
		if err != nil {
			logger.Error("_ScheduleClientsForHunt:", err)
		}

		// Spin here until all the results are processed for this hunt.
		for {
			modified2, result_count, err := self._SortResultsForHunt(hunt)
			if err != nil {
				logger.Error("_SortResultsForHunt:", err)
			}

			if result_count == 0 {
				break
			}

			if modified2 {
				modified = true
			}
		}

		if modified {
			err = db.SetSubject(self.config_obj, hunt.HuntId, hunt)
			if err != nil {
				logger.Error("", err)
			}
		}

	}
	return nil
}

func (self *HuntDispatcher) _SortResultsForHunt(hunt *api_proto.Hunt) (
	modified bool, result_count int, err error) {
	db, err := datastore.GetDB(self.config_obj)
	if err != nil {
		return false, 0, err
	}

	completed_urn := hunt.HuntId + "/completed"
	// Take the first 100 urns off the list. They will be
	// removed below.
	urns, err := db.ListChildren(
		self.config_obj, completed_urn, 0, 100)
	if err != nil {
		return false, 0, err
	}

	// Nothing to do here.
	if len(urns) == 0 {
		return false, 0, nil
	}

	// Whatever happens we remove these ones.
	defer func() {
		for _, urn := range urns {
			derr := db.DeleteSubject(self.config_obj, urn)
			if derr != nil {
				err = derr
			}
		}
	}()

	for _, urn := range urns {
		summary := &api_proto.HuntInfo{}
		derr := db.GetSubject(self.config_obj, urn, summary)
		var destination string
		if derr != nil || summary.Result == nil ||
			summary.Result.State == flows_proto.FlowContext_ERROR {
			destination = hunt.HuntId + "/errors/" +
				summary.ClientId
			hunt.TotalClientsWithErrors += 1
			err = derr
		} else if summary.Result.TotalResults > 0 {
			destination = hunt.HuntId + "/results/" +
				summary.ClientId
			hunt.TotalClientsWithResults += 1
		} else if summary.Result.TotalResults == 0 {
			destination = hunt.HuntId + "/no_results/" +
				summary.ClientId
			hunt.TotalClientsWithoutResults += 1
		} else {
			continue
		}

		derr = db.SetSubject(self.config_obj, destination, summary)
		if derr != nil {
			err = derr
		}

		modified = true
		result_count += 1
	}

	return
}

// Move the required clients from the pending queue to the running
// queue. We only move clients which are due to be scheduled.
func (self *HuntDispatcher) _ScheduleClientsForHunt(hunt *api_proto.Hunt) (
	modified bool, err error) {
	db, err := datastore.GetDB(self.config_obj)
	if err != nil {
		return false, err
	}

	logger := logging.GetLogger(self.config_obj, &logging.FrontendComponent)

	client_rate := hunt.ClientRate

	// Default client rate is 20 per minute.
	if client_rate == 0 {
		client_rate = 20
	}

	last_unpause_time := hunt.LastUnpauseTime
	// Default LastUnpauseTime is hunt creation time.
	if last_unpause_time == 0 {
		last_unpause_time = hunt.CreateTime
	}
	now := uint64(time.Now().UTC().UnixNano() / 1000)
	seconds_since_unpause := (now - last_unpause_time) / 1000000
	expected_clients := (client_rate*seconds_since_unpause/60 +
		hunt.TotalClientsWhenUnpaused)

	// We should be adding some more clients to the
	// hunt. Read HuntInfo AFF4 objects from the
	// pending queue, launch their flows and put them in
	// the running queue.
	if hunt.TotalClientsScheduled < expected_clients {

		// Only get as many clients as we need from the
		// pending queue and not more.
		clients_to_get := expected_clients - hunt.TotalClientsScheduled
		pending_urn := hunt.HuntId + "/pending"
		urns, err := db.ListChildren(
			self.config_obj, pending_urn, 0, clients_to_get)
		if err != nil {
			return false, err
		}

		// No clients in the pending queue - nothing to do.
		if len(urns) == 0 {
			return false, nil
		}

		// Regardless what happens below we really need to
		// remove the urns from the pending queue.
		defer func() {
			for _, urn := range urns {
				derr := db.DeleteSubject(self.config_obj, urn)
				if derr != nil {
					err = derr
				}
			}
		}()

		// We need to launch the flow by calling our gRPC
		// endpoint API.
		channel := grpc_client.GetChannel(self.config_obj)
		defer channel.Close()

		for _, urn := range urns {
			// Get the summary and launch the flow.
			summary := &api_proto.HuntInfo{}
			err := db.GetSubject(self.config_obj, urn, summary)
			if err != nil {
				logger.Error("", err)
				continue
			}
			flow_runner_args := &flows_proto.FlowRunnerArgs{
				ClientId: summary.ClientId,
				FlowName: "HuntRunnerFlow",
			}
			flow_args, err := ptypes.MarshalAny(summary)
			if err != nil {
				logger.Error("", err)
				continue
			}
			flow_runner_args.Args = flow_args

			client := api_proto.NewAPIClient(channel)
			response, err := client.LaunchFlow(
				context.Background(), flow_runner_args)
			if err != nil {
				// If we can not launch the flow we
				// need to store the summary in the
				// error queue.
				logger.Error("Cant launch hunt flow", err)
				summary.State = api_proto.HuntInfo_ERROR
				summary.Result = &flows_proto.FlowContext{
					CreateTime: uint64(time.Now().UnixNano() / 1000),
					Backtrace:  fmt.Sprintf("HuntDispatcher: %v", err),
				}

				hunt.TotalClientsWithErrors += 1
				modified = true

				error_urn := hunt.HuntId + "/errors/" + summary.ClientId
				err = db.SetSubject(self.config_obj, error_urn, summary)

				continue
			}

			// Store the summary in the running queue.
			summary.FlowId = response.FlowId
			running_urn := hunt.HuntId + "/running/" + summary.ClientId
			err = db.SetSubject(self.config_obj, running_urn, summary)

			hunt.TotalClientsScheduled += 1
			modified = true
		}
	}
	return
}

type HuntDispatcherContainer struct {
	refresh_mu sync.Mutex
	mu         sync.Mutex
	dispatcher *HuntDispatcher
}

func (self *HuntDispatcherContainer) Refresh(config_obj *api_proto.Config) {
	// Serialize access to Refresh() calls. While the
	// NewHuntDispatcher() is being built, readers may access the
	// old one freely, but new Refresh calls are blocked.
	self.refresh_mu.Lock()
	defer self.refresh_mu.Unlock()
	dispatcher, err := NewHuntDispatcher(config_obj)
	if err != nil {
		dispatcher = &HuntDispatcher{}
	}

	// Swap the pointers under lock between the old and new hunt
	// list. This should be very fast minimizing reader
	// contention.
	self.mu.Lock()
	defer self.mu.Unlock()

	self.dispatcher = dispatcher
}

func NewHuntDispatcher(config_obj *api_proto.Config) (*HuntDispatcher, error) {
	result := &HuntDispatcher{config_obj: config_obj}
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	hunts, err := db.ListChildren(config_obj, constants.HUNTS_URN, 0, 100)
	if err != nil {
		return nil, err
	}

	for _, hunt_urn := range hunts {
		hunt_obj := &api_proto.Hunt{}
		err = db.GetSubject(config_obj, hunt_urn, hunt_obj)
		if err != nil {
			return nil, err
		}

		result.hunts = append(result.hunts, hunt_obj)
	}

	err = result.Update()
	if err != nil {
		return nil, err
	}

	return result, nil
}

func GetHuntDispatcher(config_obj *api_proto.Config) (*HuntDispatcher, error) {
	dispatch_container.mu.Lock()
	defer dispatch_container.mu.Unlock()

	if dispatch_container.dispatcher == nil {
		dispatcher, err := NewHuntDispatcher(config_obj)
		if err != nil {
			logging.GetLogger(config_obj, &logging.FrontendComponent).
				Error("", err)
			return nil, err
		}
		dispatch_container.dispatcher = dispatcher

		// Refresh the container every 10 seconds.
		go func() {
			for {
				time.Sleep(10 * time.Second)
				dispatch_container.Refresh(config_obj)
			}
		}()
	}
	return dispatch_container.dispatcher, nil
}

func GetNewHuntId() string {
	result := make([]byte, 8)
	buf := make([]byte, 4)

	rand.Read(buf)
	hex.Encode(result, buf)

	return urns.BuildURN("hunts", constants.HUNT_PREFIX+string(result))
}

func FindCollectedArtifacts(hunt *api_proto.Hunt) {
	switch hunt.StartRequest.FlowName {
	case "ArtifactCollector":
		flow_args := &flows_proto.ArtifactCollectorArgs{}
		err := ptypes.UnmarshalAny(hunt.StartRequest.Args, flow_args)
		if err == nil {
			hunt.Artifacts = flow_args.Artifacts.Names
		}
	case "FileFinder":
		hunt.Artifacts = []string{constants.FileFinderArtifactName}
	}
}

func CreateHunt(config_obj *api_proto.Config, hunt *api_proto.Hunt) (*string, error) {
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	hunt.HuntId = GetNewHuntId()
	hunt.CreateTime = uint64(time.Now().UTC().UnixNano() / 1000)
	hunt.LastUnpauseTime = hunt.CreateTime
	if hunt.Expires < hunt.CreateTime {
		hunt.Expires = uint64(time.Now().Add(7*24*time.Hour).
			UTC().UnixNano() / 1000)
	}
	if hunt.State == api_proto.Hunt_UNSET {
		hunt.State = api_proto.Hunt_PAUSED
	}

	err = db.SetSubject(config_obj, hunt.HuntId, hunt)
	if err != nil {
		return nil, err
	}

	// Trigger a refresh of the hunt dispatcher. This
	// guarantees that fresh data will be read in
	// subsequent ListHunt() calls.
	dispatch_container.Refresh(config_obj)

	// Notify all the clients about the new hunt. New hunts are
	// not that common so notifying all the clients at once is
	// probably ok.
	channel := grpc_client.GetChannel(config_obj)
	defer channel.Close()

	client := api_proto.NewAPIClient(channel)
	client.NotifyClients(
		context.Background(), &api_proto.NotificationRequest{
			NotifyAll: true,
		})

	return &hunt.HuntId, nil
}

func ListHunts(config_obj *api_proto.Config, in *api_proto.ListHuntsRequest) (
	*api_proto.ListHuntsResponse, error) {
	dispatcher, err := GetHuntDispatcher(config_obj)
	if err != nil {
		return nil, err
	}

	result := &api_proto.ListHuntsResponse{}
	for idx, hunt := range dispatcher.GetApplicableHunts(0) {
		if uint64(idx) < in.Offset {
			continue
		}

		if uint64(idx) >= in.Offset+in.Count {
			break
		}
		result.Items = append(result.Items, hunt)
	}

	return result, nil
}

func GetHunt(config_obj *api_proto.Config, in *api_proto.GetHuntRequest) (
	*api_proto.Hunt, error) {
	dispatcher, err := GetHuntDispatcher(config_obj)
	if err != nil {
		return nil, err
	}

	for _, hunt := range dispatcher.GetApplicableHunts(0) {
		if path.Base(hunt.HuntId) == in.HuntId {
			// HACK: Velociraptor only knows how to
			// collect artifacts now. Eventually the whole
			// concept of a flow will go away but for now
			// we need to figure out which artifacts we
			// are actually collecting - there are not
			// many possibilities since we have reduced
			// the number of possible flows significantly.
			FindCollectedArtifacts(hunt)
			return hunt, nil
		}
	}

	return nil, errors.New("Not found")
}

func GetHuntInfos(config_obj *api_proto.Config, in *api_proto.GetHuntResultsRequest) (
	*api_proto.HuntResults, error) {
	result := &api_proto.HuntResults{}
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}
	if in.Count == 0 {
		in.Count = 50
	}
	// Verify the hunt id
	if !strings.HasPrefix(in.HuntId, "aff4:/hunts/H.") {
		return nil, errors.New("Invalid hunt id")
	}

	// These HuntInfo are flows with at least one results.
	client_urns, err := db.ListChildren(
		config_obj, in.HuntId+"/results",
		in.Offset, in.Count)
	if err != nil {
		return nil, err
	}
	for _, urn := range client_urns {
		summary := &api_proto.HuntInfo{}
		err := db.GetSubject(config_obj, urn, summary)
		if err != nil {
			continue
		}

		result.Items = append(result.Items, summary)
	}

	return result, nil
}

func GetHuntResults(config_obj *api_proto.Config, in *api_proto.GetHuntResultsRequest) (
	*api_proto.ApiFlowResultDetails, error) {
	result := &api_proto.ApiFlowResultDetails{}
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}
	if in.Count == 0 {
		in.Count = 50
	}
	// Verify the hunt id
	if !strings.HasPrefix(in.HuntId, "aff4:/hunts/H.") {
		return nil, errors.New("Invalid hunt id")
	}

	offset := uint64(0)
	children_offset := uint64(0)
	count := uint64(0)
	for {
		// These HuntInfo are flows with at least one results.
		client_urns, err := db.ListChildren(
			config_obj, in.HuntId+"/results",
			children_offset, 50)
		if err != nil {
			return nil, err
		}

		if len(client_urns) == 0 {
			break
		}
		children_offset += uint64(len(client_urns))
		for _, urn := range client_urns {
			hunt_info := &api_proto.HuntInfo{}
			err := db.GetSubject(config_obj, urn, hunt_info)
			if err != nil {
				continue
			}

			// We need to skip until in.Offset. This flow's
			// results are all prior to in.Offset so we dont need
			// to read them.
			if offset+hunt_info.Result.TotalResults < in.Offset {
				offset += hunt_info.Result.TotalResults
				continue
			}

			first_result := in.Offset - offset
			if first_result < 0 {
				first_result = 0
			}
			count = in.Count - offset
			flow_results, err := GetFlowResults(
				config_obj, hunt_info.ClientId, hunt_info.FlowId,
				first_result, count)
			if err != nil {
				return nil, err
			}
			offset += uint64(len(flow_results.Items))
			result.Items = append(result.Items, flow_results.Items...)

			// We have enough items now, return them.
			if uint64(len(result.Items)) >= in.Count {
				return result, nil
			}
		}
	}
	return result, nil
}

func ModifyHunt(config_obj *api_proto.Config, hunt_modification *api_proto.Hunt) error {
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return err
	}

	// TODO: Check if the user has permission to start/stop the hunt.
	hunt_obj := &api_proto.Hunt{}
	err = db.GetSubject(config_obj, hunt_modification.HuntId, hunt_obj)
	if err != nil {
		return err
	}
	modified := false

	// Only some modifications are allowed. The modified fields
	// are set in the hunt arg.
	if hunt_modification.State != api_proto.Hunt_UNSET {
		hunt_obj.State = hunt_modification.State
		modified = true

		// Hunt is being unpaused. Adjust the hunt counters to
		// account for the unpause time. If we do not do this,
		// then hunt will schedule all the clients which were
		// not scheduled during the paused interval at once -
		// exceeding the specified client rate.
		if hunt_obj.State == api_proto.Hunt_PAUSED &&
			hunt_modification.State == api_proto.Hunt_RUNNING {
			hunt_obj.LastUnpauseTime = uint64(time.Now().UTC().UnixNano() / 1000)
			hunt_obj.TotalClientsWhenUnpaused = hunt_obj.TotalClientsScheduled
		}
	}

	if modified {
		err := db.SetSubject(config_obj, hunt_modification.HuntId, hunt_obj)
		if err != nil {
			return err
		}

		// Trigger a refresh of the hunt dispatcher. This
		// guarantees that fresh data will be read in
		// subsequent ListHunt() calls.
		dispatch_container.Refresh(config_obj)

		return nil
	}

	return errors.New("Modification not supported.")
}

func ListHuntClients(config_obj *api_proto.Config,
	req *api_proto.ListHuntClientsRequest) (*api_proto.HuntResults, error) {
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return nil, err
	}

	count := req.Count
	if count == 0 {
		count = 50
	}

	var queue string
	switch req.Type {
	case api_proto.ListHuntClientsRequest_PENDING:
		queue = "/pending"
	case api_proto.ListHuntClientsRequest_SCHEDULED:
		queue = "/running"
	case api_proto.ListHuntClientsRequest_COMPLETED:
		queue = "/completed"
	case api_proto.ListHuntClientsRequest_RESULTS:
		queue = "/results"
	default:
		queue = "/pending"
	}

	children, err := db.ListChildren(
		config_obj,
		req.HuntId+queue, req.Offset, count)
	if err != nil {
		return nil, err
	}

	result := &api_proto.HuntResults{}
	for _, child_urn := range children {
		hunt_info := &api_proto.HuntInfo{}
		err = db.GetSubject(config_obj, child_urn, hunt_info)
		if err != nil {
			continue
		}

		result.Items = append(result.Items, hunt_info)
	}

	return result, nil
}

// A Flow which runs a delegate flow and stores the result in the
// hunt.
type HuntRunnerFlow struct {
	delegate_flow_obj *AFF4FlowObject
}

func (self *HuntRunnerFlow) New() Flow {
	return &HuntRunnerFlow{}
}

func (self *HuntRunnerFlow) Start(
	config_obj *api_proto.Config,
	flow_obj *AFF4FlowObject,
	args proto.Message) error {
	hunt_summary_args, ok := args.(*api_proto.HuntInfo)
	if !ok {
		return errors.New("Expected args of type HuntInfo")
	}
	delegate_flow_obj_proto := &flows_proto.AFF4FlowObject{
		Urn:         flow_obj.Urn,
		RunnerArgs:  hunt_summary_args.StartRequest,
		FlowContext: flow_obj.FlowContext,
	}
	delegate_flow_obj_proto.RunnerArgs.ClientId = hunt_summary_args.ClientId
	delegate_flow_obj_proto.RunnerArgs.Creator = hunt_summary_args.HuntId

	delegate_args, err := GetFlowArgs(hunt_summary_args.StartRequest)
	if err != nil {
		return err
	}

	delegate_flow_obj, err := AFF4FlowObjectFromProto(delegate_flow_obj_proto)
	if err != nil {
		return err
	}
	self.delegate_flow_obj = delegate_flow_obj

	return self.delegate_flow_obj.impl.Start(
		config_obj, delegate_flow_obj, delegate_args)
}

func (self *HuntRunnerFlow) Load(
	config_obj *api_proto.Config,
	flow_obj *AFF4FlowObject) error {
	delegate_flow_obj_proto, ok := flow_obj.GetState().(*flows_proto.AFF4FlowObject)
	if ok {
		delegate_flow_obj, err := AFF4FlowObjectFromProto(delegate_flow_obj_proto)
		if err != nil {
			return err
		}
		self.delegate_flow_obj = delegate_flow_obj
		return self.delegate_flow_obj.impl.Load(config_obj, flow_obj)
	}
	return nil
}

func (self *HuntRunnerFlow) Save(
	config_obj *api_proto.Config,
	flow_obj *AFF4FlowObject) error {
	// Store the delegate in our state
	state, err := self.delegate_flow_obj.AsProto()
	if err != nil {
		return err
	}
	flow_obj.SetState(state)
	return nil
}

func (self *HuntRunnerFlow) ProcessMessage(
	config_obj *api_proto.Config,
	flow_obj *AFF4FlowObject,
	message *crypto_proto.GrrMessage) error {
	delegate_err := self.delegate_flow_obj.impl.ProcessMessage(
		config_obj, self.delegate_flow_obj, message)

	// If the delegate flow is no longer running then write its
	// result to the hunt complete queue.
	if delegate_err != nil ||
		self.delegate_flow_obj.FlowContext.State !=
			flows_proto.FlowContext_RUNNING {
		args, err := GetFlowArgs(flow_obj.RunnerArgs)
		if err != nil {
			return err
		}
		hunt_summary_args := args.(*api_proto.HuntInfo)
		hunt_summary_args.FlowId = flow_obj.Urn
		urn := hunt_summary_args.HuntId + "/completed/" + hunt_summary_args.ClientId
		hunt_summary_args.Result = self.delegate_flow_obj.FlowContext
		db, err := datastore.GetDB(config_obj)
		if err != nil {
			return err
		}
		err = db.SetSubject(config_obj, urn, hunt_summary_args)
		if err != nil {
			return err
		}
		flow_obj.SetContext(self.delegate_flow_obj.FlowContext)
	}

	return delegate_err
}

func init() {
	impl := HuntRunnerFlow{}
	default_args, _ := ptypes.MarshalAny(&actions_proto.VQLCollectorArgs{})
	desc := &flows_proto.FlowDescriptor{
		Name:         "HuntRunnerFlow",
		FriendlyName: "HuntRunnerFlow",
		Category:     "Internal",
		Doc:          "Runs a flow as part of a hunt.",
		ArgsType:     "HuntInfo",
		DefaultArgs:  default_args,
		Internal:     true,
	}

	RegisterImplementation(desc, &impl)
}
