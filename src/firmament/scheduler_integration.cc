#include "base/resource_status.h"
#include "base/resource_topology_node_desc.pb.h"
#include "misc/utils.h"
#include "misc/trace_generator.h"
#include "misc/wall_time.h"
#include "platforms/sim/simulated_messaging_adapter.h"
#include "scheduling/flow/flow_scheduler.h"
#include "storage/stub_object_store.h"

#include "apiclient/k8s_api_client.h"

#include <glog/logging.h>
#include <gflags/gflags.h>

// XXX(malte): hack to make things compile
DEFINE_string(listen_uri, "", "");

using firmament::BaseMessage;
using firmament::JobMap_t;
using firmament::ResourceDescriptor;
using firmament::ResourceMap_t;
using firmament::ResourceID_t;
using firmament::ResourceStatus;
using firmament::TaskMap_t;
using firmament::KnowledgeBase;
using firmament::ResourceTopologyNodeDescriptor;
using firmament::TraceGenerator;
using firmament::WallTime;
using firmament::platform::sim::SimulatedMessagingAdapter;
using firmament::scheduler::FlowScheduler;
using firmament::scheduler::ObjectStoreInterface;
using firmament::scheduler::TopologyManager;

using poseidon::apiclient::K8sApiClient;

boost::shared_ptr<JobMap_t> job_map_;
boost::shared_ptr<KnowledgeBase> knowledge_base_;
boost::shared_ptr<ObjectStoreInterface> obj_store_;
boost::shared_ptr<ResourceMap_t> resource_map_;
boost::shared_ptr<TaskMap_t> task_map_;
boost::shared_ptr<TopologyManager> topology_manager_;

ResourceStatus* CreateTopLevelResource(void) {
  ResourceID_t res_id = firmament::GenerateResourceID();
  ResourceTopologyNodeDescriptor* rtnd = new ResourceTopologyNodeDescriptor();
  // Set up the RD
  ResourceDescriptor* rd = rtnd->mutable_resource_desc();
  rd->set_uuid(firmament::to_string(res_id));
  rd->set_type(ResourceDescriptor::RESOURCE_COORDINATOR);
  // Need to maintain a ResourceStatus for the resource map
  // TODO(malte): don't pass localhost here
  ResourceStatus* rs = new ResourceStatus(rd, rtnd, "localhost", 0);
  // Insert into resource map
  CHECK(InsertIfNotPresent(resource_map_.get(), res_id, rs));
  return rs;
}

ResourceStatus* CreateResourceForNode(ResourceID_t node_id,
                                      ResourceID_t parent_id) {
  ResourceTopologyNodeDescriptor* r =
    new ResourceTopologyNodeDescriptor();
  // Create and initialize RD
  ResourceDescriptor* rd = r->mutable_resource_desc();
  rd->set_uuid(firmament::to_string(node_id));
  rd->set_type(ResourceDescriptor::RESOURCE_MACHINE);
  rd->set_state(ResourceDescriptor::RESOURCE_IDLE);
  r->set_parent_id(firmament::to_string(parent_id));
  // Need to maintain a ResourceStatus for the resource map
  // TODO(malte): set hostname correctly
  ResourceStatus* rs = new ResourceStatus(rd, r, "", 0);
  // Insert into resource map
  CHECK(InsertIfNotPresent(resource_map_.get(), node_id, rs));
  return rs;
}

int main(int argc, char** argv) {
  google::ParseCommandLineFlags(&argc, &argv, false);
  google::InitGoogleLogging(argv[0]);

  // Kubernetes API client
  K8sApiClient api_client;

  job_map_.reset(new JobMap_t);
  resource_map_.reset(new ResourceMap_t);

  ResourceStatus* toplevel_res_status = CreateTopLevelResource();
  ResourceID_t toplevel_res_id =
    firmament::ResourceIDFromString(toplevel_res_status->descriptor().uuid());

  SimulatedMessagingAdapter<BaseMessage> ma;
  WallTime wall_time;
  TraceGenerator tg(&wall_time);

  FlowScheduler fs(job_map_, resource_map_,
                   toplevel_res_status->mutable_topology_node(), obj_store_,
                   task_map_, knowledge_base_, topology_manager_,
                   &ma, NULL, toplevel_res_id, "", &wall_time, &tg);
  LOG(INFO) << "Firmament scheduler instantiated: " << fs;

  // main loop -- keep looking for nodes and pods
  while (true) {
    // Poll nodes
    vector<pair<string, string>> nodes = api_client.AllNodes();
    if (!nodes.empty()) {
      for (auto& n : nodes) {
        ResourceID_t rid = firmament::ResourceIDFromString(n.first);
        // Check if we know about this node already
        if (!ContainsKey(*resource_map_, rid)) {
          LOG(INFO) << "Adding new node's resource with RID " << rid;
          // Create a new Firmament resource
          ResourceStatus* rs = CreateResourceForNode(rid, toplevel_res_id);
          // Register with the scheudler
          fs.RegisterResource(rs->mutable_topology_node(), false, false);
        }
      }
    }

    // Poll pods
    vector<string> pods = api_client.AllPods();
    if (!pods.empty()) {
      for (auto& p : pods) {
        LOG(INFO) << "Pod: " << p;
        // XXX(malte): test hack -- always bind to first node
        // Note that this will try to re-bind even already bound pods at the
        // moment.
        if (!nodes.empty()) {
          api_client.BindPodToNode(p, nodes[0].second);
        }
      }
    }

    sleep(10);
  }
}
