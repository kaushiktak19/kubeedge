package manager

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	beehiveContext "github.com/kubeedge/beehive/pkg/core/context"
	"github.com/kubeedge/beehive/pkg/core/model"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/client"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/modules"
	"github.com/kubeedge/kubeedge/cloud/pkg/taskmanager/nodeupgradecontroller"
	"github.com/kubeedge/kubeedge/cloud/pkg/taskmanager/util"
	"github.com/kubeedge/kubeedge/cloud/pkg/taskmanager/util/controller"
	"github.com/kubeedge/kubeedge/common/constants"
	commontypes "github.com/kubeedge/kubeedge/common/types"
	api "github.com/kubeedge/kubeedge/pkg/apis/fsm/v1alpha1"
	"github.com/kubeedge/kubeedge/pkg/apis/operations/v1alpha1"
	"github.com/kubeedge/kubeedge/pkg/util/fsm"
)

type Executor struct {
	task       util.TaskMessage
	statusChan chan *v1alpha1.TaskStatus
	nodes      []v1alpha1.TaskStatus
	controller controller.Controller
}

func NewExecutorMachine(messageChan chan util.TaskMessage, downStreamChan chan model.Message) (*ExecutorMachine, error) {
	executorMachine = &ExecutorMachine{
		kubeClient:     client.GetKubeClient(),
		executors:      map[string]*Executor{},
		messageChan:    messageChan,
		downStreamChan: downStreamChan,
	}
	return executorMachine, nil
}

func GetExecutorMachine() *ExecutorMachine {
	return executorMachine
}

// Start ExecutorMachine
func (em *ExecutorMachine) Start() error {
	klog.Info("Start ExecutorMachine")

	go em.syncTask()

	return nil
}

// syncTask is used to get events from informer
func (em *ExecutorMachine) syncTask() {
	for {
		select {
		case <-beehiveContext.Done():
			klog.Info("stop sync tasks")
			return
		case msg := <-em.messageChan:
			if msg.ShutDown {
				klog.Errorf("delete executor %s ", msg.Name)
				DeleteExecutor(msg)
				break
			}
			err := GetExecutor(msg).HandleMessage(msg.Status)
			if err != nil {
				klog.Errorf("Failed to handel upgrade message due to error %s", err.Error())
				break
			}
		}
	}
}

type ExecutorMachine struct {
	kubeClient     kubernetes.Interface
	executors      map[string]*Executor
	messageChan    chan util.TaskMessage
	downStreamChan chan model.Message
	sync.Mutex
}

var executorMachine *ExecutorMachine

func GetExecutor(msg util.TaskMessage) *Executor {
	executorMachine.Lock()
	e, ok := executorMachine.executors[fmt.Sprintf("%s::%s", msg.Type, msg.Name)]
	executorMachine.Unlock()
	if ok && e != nil {
		return e
	}
	e, err := initExecutor(msg)
	if err != nil {
		klog.Error("executor init failed, error: %s", err.Error())
		return nil
	}
	return e
}

func DeleteExecutor(msg util.TaskMessage) {
	executorMachine.Lock()
	defer executorMachine.Unlock()
	delete(executorMachine.executors, fmt.Sprintf("%s::%s", msg.Type, msg.Name))
}

func (e *Executor) HandleMessage(status v1alpha1.TaskStatus) error {
	if e == nil {
		return fmt.Errorf("executor is nil")
	}
	e.statusChan <- &status
	return nil
}

func (e *Executor) initMessage(node v1alpha1.TaskStatus) *model.Message {
	// delete it in 1.18
	if e.task.Type == util.TaskUpgrade {
		msg := e.initHistoryMessage(node)
		if msg != nil {
			klog.Warningf("send history message to node")
			return msg
		}
	}

	msg := model.NewMessage("")
	resource := buildTaskResource(e.task.Type, e.task.Name, node.NodeName)

	taskReq := commontypes.NodeTaskRequest{
		TaskID: e.task.Name,
		Type:   e.task.Type,
		State:  string(node.State),
	}
	taskReq.Item = e.task.Msg
	if node.State == api.TaskChecking {
		taskReq.Item = commontypes.NodePreCheckRequest{
			CheckItem: e.task.CheckItem,
		}
	}
	msg.BuildRouter(modules.TaskManagerModuleName, modules.TaskManagerModuleGroup, resource, util.TaskUpgrade).
		FillBody(taskReq)
	return msg
}

func (e *Executor) initHistoryMessage(node v1alpha1.TaskStatus) *model.Message {
	resource := buildUpgradeResource(e.task.Name, node.NodeName)
	req := e.task.Msg.(commontypes.NodeUpgradeJobRequest)
	upgradeController := e.controller.(*nodeupgradecontroller.NodeUpgradeController)
	edgeVersion, err := upgradeController.GetNodeVersion(node.NodeName)
	if err != nil {
		klog.Errorf("get node version failed: %s", err.Error())
		return nil
	}
	less, err := util.VersionLess(edgeVersion, "v1.16.0")
	if err != nil {
		klog.Errorf("version less failed: %s", err.Error())
		return nil
	}
	if !less {
		return nil
	}
	klog.Warningf("edge version is %s, is less than version %s", edgeVersion, "v1.16.0")
	upgradeReq := commontypes.NodeUpgradeJobRequest{
		UpgradeID:   e.task.Name,
		HistoryID:   uuid.New().String(),
		UpgradeTool: "keadm",
		Version:     req.Version,
		Image:       req.Image,
	}
	msg := model.NewMessage("")
	msg.BuildRouter(modules.NodeUpgradeJobControllerModuleName, modules.NodeUpgradeJobControllerModuleGroup, resource, util.TaskUpgrade).
		FillBody(upgradeReq)
	return msg
}

func initExecutor(message util.TaskMessage) (*Executor, error) {
	controller, err := controller.GetController(message.Type)
	if err != nil {
		return nil, err
	}
	nodeStatus, err := controller.GetNodeStatus(message.Name)
	if err != nil {
		return nil, err
	}
	if len(nodeStatus) == 0 {
		nodeList := controller.ValidateNode(message)
		if len(nodeList) == 0 {
			return nil, fmt.Errorf("no node need to be upgrade")
		}
		nodeStatus = make([]v1alpha1.TaskStatus, len(nodeList))
		for i, node := range nodeList {
			nodeStatus[i] = v1alpha1.TaskStatus{NodeName: node.Name}
		}
		err = controller.UpdateNodeStatus(message.Name, nodeStatus)
		if err != nil {
			return nil, err
		}
	}
	e := &Executor{
		task:       message,
		statusChan: make(chan *v1alpha1.TaskStatus, 10),
		controller: controller,
		nodes:      nodeStatus,
	}
	go e.start()
	executorMachine.executors[fmt.Sprintf("%s::%s", message.Type, message.Name)] = e
	return e, nil
}

func (e *Executor) start() {
	maxFailedNodes := float64(len(e.nodes)) * (e.task.FailureTolerate)
	failedNodes := map[string]bool{}
	worker := workers{
		number:       int(e.task.Concurrency),
		jobs:         make(map[string]int),
		shuttingDown: false,
		Mutex:        sync.Mutex{},
	}
	index := 0
	dealCompletedNode := func(node v1alpha1.TaskStatus) error {
		if node.State == api.TaskFailed {
			failedNodes[node.NodeName] = true
		}
		if float64(len(failedNodes)) < maxFailedNodes {
			return nil
		}
		worker.shuttingDown = true
		if len(worker.jobs) > 0 {
			klog.Warningf("wait for all workers(%d/%d) for task %s to finish running ", len(worker.jobs), worker.number, e.task.Name)
			return nil
		}

		errMsg := fmt.Sprintf("the number of failed nodes is %d/%d, which exceeds the failure tolerance threshold.", len(failedNodes), len(e.nodes))
		_, err := e.controller.ReportTaskStatus(e.task.Name, fsm.Event{
			Type:     node.Event,
			Action:   node.Action,
			ErrorMsg: errMsg,
		})
		if err != nil {
			return fmt.Errorf("%s, report status failed, %s", errMsg, err.Error())
		}
		return fmt.Errorf(errMsg)
	}

	index, err := e.initWorker(dealCompletedNode, &worker)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	for {
		select {
		case <-beehiveContext.Done():
			klog.Info("stop sync tasks")
			return
		case status := <-e.statusChan:
			if status == nil || reflect.DeepEqual(*status, v1alpha1.TaskStatus{}) {
				break
			}
			if !e.controller.StageCompleted(e.task.Name, status.State) {
				break
			}
			var endNode int
			endNode, err = worker.endJob(status.NodeName)
			if err != nil {
				klog.Errorf(err.Error())
				break
			}

			e.nodes[endNode] = *status
			err = dealCompletedNode(*status)
			if err != nil {
				klog.Warning(err.Error())
				break
			}

			if index >= len(e.nodes) {
				if len(worker.jobs) != 0 {
					break
				}
				var state api.State
				state, err = e.completedTaskStage(*status)
				if err != nil {
					klog.Errorf(err.Error())
					break
				}
				if fsm.TaskFinish(state) {
					DeleteExecutor(e.task)
					klog.Infof("task %s is finish", e.task.Name)
					return
				}
				// next stage
				index, err = e.initWorker(dealCompletedNode, &worker)
				if err != nil {
					klog.Errorf(err.Error())
				}
				break
			}

			nextNode := e.nodes[index]
			err = worker.addJob(nextNode, index, e)
			if err != nil {
				klog.Errorf(err.Error())
				break
			}
			index++
		}
	}
}

func (e *Executor) completedTaskStage(node v1alpha1.TaskStatus) (api.State, error) {
	state, err := e.controller.ReportTaskStatus(e.task.Name, fsm.Event{
		Type:   node.Event,
		Action: api.ActionSuccess,
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

func (e *Executor) initWorker(dealCompletedNode func(node v1alpha1.TaskStatus) error, worker *workers) (int, error) {
	var index int
	var node v1alpha1.TaskStatus
	isEndNode := true
	for index, node = range e.nodes {
		if e.controller.StageCompleted(e.task.Name, node.State) {
			err := dealCompletedNode(node)
			if err != nil {
				return 0, err
			}
			continue
		}
		err := worker.addJob(node, index, e)
		if err != nil {
			klog.Info(err.Error())
			isEndNode = false
			break
		}
	}
	if isEndNode {
		index++
	}
	return index, nil
}

type workers struct {
	number int
	jobs   map[string]int
	sync.Mutex
	shuttingDown bool
}

func (w *workers) addJob(node v1alpha1.TaskStatus, index int, e *Executor) error {
	if w.shuttingDown {
		return fmt.Errorf("workers is stopped")
	}
	w.Lock()
	if len(w.jobs) >= w.number {
		w.Unlock()
		return fmt.Errorf("workers are all running, %v/%v", len(w.jobs), w.number)
	}
	w.jobs[node.NodeName] = index
	w.Unlock()
	msg := e.initMessage(node)
	go e.handelTimeOutJob(index)
	executorMachine.downStreamChan <- *msg
	return nil
}

func (e *Executor) handelTimeOutJob(index int) {
	lastState := e.nodes[index].State
	err := wait.Poll(1*time.Second, time.Duration(*e.task.TimeOutSeconds)*time.Second, func() (bool, error) {
		if lastState != e.nodes[index].State || fsm.TaskFinish(e.nodes[index].State) {
			return true, nil
		}
		klog.V(4).Infof("node %s stage is not completed", e.nodes[index].NodeName)
		return false, nil
	})
	if err != nil {
		_, err = e.controller.ReportNodeStatus(e.task.Name, e.nodes[index].NodeName, fsm.Event{
			Type:     "TimeOut",
			Action:   api.ActionFailure,
			ErrorMsg: fmt.Sprintf("node task execution timeout, %s", err.Error()),
		})
		if err != nil {
			klog.Warningf(err.Error())
		}
	}
}

func (w *workers) endJob(job string) (int, error) {
	index, ok := w.jobs[job]
	if !ok {
		return index, fmt.Errorf("end job %s error, job not exist", job)
	}
	w.Lock()
	delete(w.jobs, job)
	w.Unlock()
	return index, nil
}

func buildTaskResource(task, taskID, nodeID string) string {
	resource := strings.Join([]string{task, taskID, "node", nodeID}, constants.ResourceSep)
	return resource
}

func buildUpgradeResource(upgradeID, nodeID string) string {
	resource := strings.Join([]string{util.TaskUpgrade, upgradeID, "node", nodeID}, constants.ResourceSep)
	return resource
}
