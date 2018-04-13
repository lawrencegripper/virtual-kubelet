package azurebatch

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Azure/go-autorest/autorest/azure"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/Azure/go-autorest/autorest/to"

	"github.com/Azure/azure-sdk-for-go/services/batch/2017-09-01.6.0/batch"
	"github.com/lawrencegripper/pod2docker"
	"github.com/virtual-kubelet/virtual-kubelet/manager"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	stdoutFile string = "stdout.txt"
	stderrFile string = "stderr.txt"
	podJsonKey string = "virtualkubelet_pod"
)

// Provider the base struct for the Azure Batch provider
type Provider struct {
	batchConfig        *Config
	ctx                context.Context
	cancelOps          context.CancelFunc
	poolClient         *batch.PoolClient
	jobClient          *batch.JobClient
	taskClient         *batch.TaskClient
	fileClient         *batch.FileClient
	resourceManager    *manager.ResourceManager
	listTasks          func() (*[]batch.CloudTask, error)
	resourceGroup      string
	region             string
	nodeName           string
	operatingSystem    string
	cpu                string
	memory             string
	pods               string
	internalIP         string
	daemonEndpointPort int32
}

// Config - Basic azure config used to interact with ARM resources.
type Config struct {
	ClientID        string
	ClientSecret    string
	SubscriptionID  string
	TenantID        string
	ResourceGroup   string
	PoolID          string
	JobID           string
	AccountName     string
	AccountLocation string
}

// NewBatchProvider Creates a batch provider
func NewBatchProvider(config string, rm *manager.ResourceManager, nodeName, operatingSystem string, internalIP string, daemonEndpointPort int32) (*Provider, error) {
	fmt.Println("Starting create provider")

	batchConfig, err := getAzureConfigFromEnv()
	if err != nil {
		log.Println("Failed to get auth information")
	}

	p := Provider{}
	p.batchConfig = &batchConfig
	// Set sane defaults for Capacity in case config is not supplied
	p.cpu = "20"
	p.memory = "100Gi"
	p.pods = "20"
	p.resourceManager = rm
	p.operatingSystem = operatingSystem
	p.nodeName = nodeName
	p.internalIP = internalIP
	p.daemonEndpointPort = daemonEndpointPort
	p.ctx = context.Background()

	auth := getAzureADAuthorizer(p.batchConfig, azure.PublicCloud.BatchManagementEndpoint)

	createOrGetPool(&p, auth)
	createOrGetJob(&p, auth)

	taskclient := batch.NewTaskClientWithBaseURI(getBatchBaseURL(p.batchConfig))
	taskclient.Authorizer = auth
	p.taskClient = &taskclient

	p.listTasks = func() (*[]batch.CloudTask, error) {
		res, err := p.taskClient.List(p.ctx, p.batchConfig.JobID, "", "", "", nil, nil, nil, nil, nil)
		if err != nil {
			return &[]batch.CloudTask{}, err
		}
		currentTasks := res.Values()
		for res.NotDone() {
			err = res.Next()
			if err != nil {
				return &[]batch.CloudTask{}, err
			}
			pageTasks := res.Values()
			if pageTasks != nil || len(pageTasks) != 0 {
				currentTasks = append(currentTasks, pageTasks...)
			}
		}

		return &currentTasks, nil
	}

	fileClient := batch.NewFileClientWithBaseURI(getBatchBaseURL(p.batchConfig))
	fileClient.Authorizer = auth
	p.fileClient = &fileClient

	return &p, nil
}

// CreatePod accepts a Pod definition
func (p *Provider) CreatePod(pod *v1.Pod) error {
	log.Println("Creating pod...")
	podCommand, err := pod2docker.GetBashCommand(pod2docker.PodComponents{
		Containers: pod.Spec.Containers,
		PodName:    pod.Name,
		Volumes:    pod.Spec.Volumes,
	})
	if err != nil {
		return err
	}

	bytes, err := json.Marshal(pod)
	if err != nil {
		panic(err)
	}

	task := batch.TaskAddParameter{
		DisplayName: to.StringPtr(string(pod.UID)),
		ID:          to.StringPtr(getTaskIDForPod(pod.Namespace, pod.Name)),
		CommandLine: to.StringPtr(fmt.Sprintf(`/bin/bash -c "%s"`, podCommand)),
		UserIdentity: &batch.UserIdentity{
			AutoUser: &batch.AutoUserSpecification{
				ElevationLevel: batch.Admin,
				Scope:          batch.Pool,
			},
		},
		EnvironmentSettings: &[]batch.EnvironmentSetting{
			{
				Name:  to.StringPtr(podJsonKey),
				Value: to.StringPtr(string(bytes)),
			},
		},
	}
	p.taskClient.Add(p.ctx, p.batchConfig.JobID, task, nil, nil, nil, nil)

	return nil
}

// GetPodStatus retrieves the status of a given pod by name.
func (p *Provider) GetPodStatus(namespace, name string) (*v1.PodStatus, error) {
	log.Println("Getting pod status ....")
	pod, err := p.GetPod(namespace, name)

	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, nil
	}
	return &pod.Status, nil
}

// UpdatePod accepts a Pod definition
func (p *Provider) UpdatePod(pod *v1.Pod) error {
	err := p.DeletePod(pod)
	if err != nil {
		return err
	}
	err = p.CreatePod(pod)
	if err != nil {
		return err
	}
	return nil
}

// DeletePod accepts a Pod definition
func (p *Provider) DeletePod(pod *v1.Pod) error {
	taskID := getTaskIDForPod(pod.Namespace, pod.Name)
	task, err := p.taskClient.Delete(p.ctx, p.batchConfig.JobID, taskID, nil, nil, nil, nil, "", "", nil, nil)
	if err != nil {
		log.Println(task)
		log.Println(err)
		return err
	}

	log.Printf(fmt.Sprintf("Deleting task: %v", taskID))
	return nil
}

// GetPod returns a pod by name
func (p *Provider) GetPod(namespace, name string) (*v1.Pod, error) {
	log.Println("Getting Pod ...")
	task, err := p.taskClient.Get(p.ctx, p.batchConfig.JobID, getTaskIDForPod(namespace, name), "", "", nil, nil, nil, nil, "", "", nil, nil)
	if err != nil {
		if task.Response.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		log.Println(err)
		return nil, err
	}

	pod, err := getPodFromTask(&task)
	if err != nil {
		panic(err)
	}

	// jsonBytpes, _ := json.Marshal(task)
	// if pod.Labels == nil {
	// 	pod.Labels = make(map[string]string)
	// }
	// pod.Labels["batchStatus"] = string(jsonBytpes)
	status, _ := convertTaskToPodStatus(&task)
	pod.Status = *status

	return pod, nil
}

// GetContainerLogs returns the logs of a container running in a pod by name.
func (p *Provider) GetContainerLogs(namespace, podName, containerName string, tail int) (string, error) {
	log.Println("Getting pod logs ....")

	logFileLocation := fmt.Sprintf("wd/%s", containerName)
	// todo: Log file is the json log from docker - deserialise and form at it before returning it.
	reader, err := p.fileClient.GetFromTask(p.ctx, p.batchConfig.JobID, getTaskIDForPod(namespace, podName), logFileLocation, nil, nil, nil, nil, "", nil, nil)

	if err != nil {
		return "", err
	}

	bytes, err := ioutil.ReadAll(*reader.Value)

	if err != nil {
		return "", err
	}

	return string(bytes), nil
}

// GetPods retrieves a list of all pods scheduled to run.
func (p *Provider) GetPods() ([]*v1.Pod, error) {
	log.Println("Getting pods...")
	tasksPtr, err := p.listTasks()
	if err != nil {
		panic(err)
	}
	if tasksPtr == nil {
		return []*v1.Pod{}, nil
	}

	tasks := *tasksPtr

	pods := make([]*v1.Pod, len(tasks), len(tasks))
	for i, t := range tasks {
		pod, err := getPodFromTask(&t)
		if err != nil {
			panic(err)
		}
		pods[i] = pod
	}

	// for _, pod := range pods {
	// 	// status, _ := p.GetPodStatus(pod.Namespace, pod.Name)
	// 	if status != nil {
	// 		pod.Status = *status
	// 	}
	// }
	return pods, nil
}

// Capacity returns a resource list containing the capacity limits
func (p *Provider) Capacity() v1.ResourceList {
	return v1.ResourceList{
		"cpu":    resource.MustParse(p.cpu),
		"memory": resource.MustParse(p.memory),
		"pods":   resource.MustParse(p.pods),
	}
}

// NodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status
// within Kubernetes.
func (p *Provider) NodeConditions() []v1.NodeCondition {
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

// NodeAddresses returns a list of addresses for the node status
// within Kubernetes.
func (p *Provider) NodeAddresses() []v1.NodeAddress {
	// TODO: Make these dynamic and augment with custom ACI specific conditions of interest
	return []v1.NodeAddress{
		{
			Type:    "InternalIP",
			Address: p.internalIP,
		},
	}
}

// NodeDaemonEndpoints returns NodeDaemonEndpoints for the node status
// within Kubernetes.
func (p *Provider) NodeDaemonEndpoints() *v1.NodeDaemonEndpoints {
	return &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}

// OperatingSystem returns the operating system for this provider.
func (p *Provider) OperatingSystem() string {
	return p.operatingSystem
}