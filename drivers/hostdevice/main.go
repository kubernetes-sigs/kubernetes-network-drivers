package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	kndnet "github.com/aojea/kubernetes-network-drivers/pkg/net"
)

// AllocatedDevice represents a network device that has been allocated to a pod.
type AllocatedDevice struct {
	Name       string
	Attributes map[string]string
	PoolName   string
	Request    string
}

// SharedState is the data that is shared between the DRA and NRI hooks.
// It is managed by the Plugin framework.
type SharedState struct {
	// PodDeviceConfig maps a pod's UID to the devices that have been allocated to it.
	PodDeviceConfig map[types.UID][]AllocatedDevice
	// PreparedData maps a pod's UID to the data that was returned by the PrepareDevice hook.
	PreparedData map[types.UID]interface{}
}

const (
	maxAttempts        = 10
	stabilityThreshold = 5 * time.Minute
)

// NetworkDriver manages the lifecycle of the DRA and NRI plugins.
type NetworkDriver struct {
	driverName string
	nodeName   string
	kubeClient kubernetes.Interface
	draPlugin  *kubeletplugin.Helper
	nriPlugin  stub.Stub

	mu          sync.Mutex
	sharedState *SharedState
}

// NewNetworkDriver creates a new NetworkDriver instance.
func NewNetworkDriver(driverName, nodeName string, kubeClient kubernetes.Interface) *NetworkDriver {
	return &NetworkDriver{
		driverName: driverName,
		nodeName:   nodeName,
		kubeClient: kubeClient,
		sharedState: &SharedState{
			PodDeviceConfig: make(map[types.UID][]AllocatedDevice),
			PreparedData:    make(map[types.UID]interface{}),
		},
	}
}

// Start initializes and runs the DRA and NRI plugins.
func (k *NetworkDriver) Start(ctx context.Context) error {
	driverPluginPath := filepath.Join(kubeletplugin.KubeletPluginsDir, k.driverName)
	if err := os.MkdirAll(driverPluginPath, 0750); err != nil {
		return fmt.Errorf("failed to create plugin path %s: %w", driverPluginPath, err)
	}

	kubeletOptions := []kubeletplugin.Option{
		kubeletplugin.DriverName(k.driverName),
		kubeletplugin.NodeName(k.nodeName),
		kubeletplugin.KubeClient(k.kubeClient),
	}
	draHelper, err := kubeletplugin.Start(ctx, k, kubeletOptions...)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	k.draPlugin = draHelper

	if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
		status := k.draPlugin.RegistrationStatus()
		return status != nil && status.PluginRegistered, nil
	}); err != nil {
		return err
	}

	nriOptions := []stub.Option{
		stub.WithPluginName(k.driverName),
		stub.WithPluginIdx("10"),
		stub.WithOnClose(func() { klog.Infof("%s NRI plugin closed", k.driverName) }),
	}
	nriStub, err := stub.New(k, nriOptions...)
	if err != nil {
		return fmt.Errorf("failed to create NRI plugin stub: %w", err)
	}
	k.nriPlugin = nriStub

	go k.runNRIPlugin(ctx)
	go k.publishResources(ctx)

	return nil
}

// Stop gracefully stops the DRA and NRI plugins.
func (k *NetworkDriver) Stop() {
	klog.Info("Stopping network driver plugin...")
	if k.nriPlugin != nil {
		k.nriPlugin.Stop()
	}
	if k.draPlugin != nil {
		k.draPlugin.Stop()
	}
	klog.Info("Network driver plugin stopped.")
}

// DRA plugin implementation
func (k *NetworkDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(2).Infof("PrepareResourceClaims called for %d claims", len(claims))
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		preparedData, err := k.prepareDevice(ctx, claim)
		if err != nil {
			results[claim.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		k.mu.Lock()
		k.sharedState.PreparedData[claim.UID] = preparedData
		k.mu.Unlock()
		results[claim.UID] = kubeletplugin.PrepareResult{}
	}
	return results, nil
}

func (k *NetworkDriver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(2).Infof("UnprepareResourceClaims called for %d claims", len(claims))
	errors := make(map[types.UID]error)
	for _, claim := range claims {
		if err := k.unprepareDevice(ctx, claim); err != nil {
			errors[claim.UID] = err
		}
		k.mu.Lock()
		delete(k.sharedState.PreparedData, claim.UID)
		k.mu.Unlock()
	}
	return errors, nil
}

// HandleError is called for errors encountered in the background.
func (k *NetworkDriver) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleError(fmt.Errorf("%s: %w", msg, err))
}

// NRI handler implementation
func (k *NetworkDriver) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	klog.V(2).Info("Synchronize called")
	return nil, nil
}

// RunPodSandbox is called when a pod is created by the Container Runtime.
func (k *NetworkDriver) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RunPodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)
	if networkNamespace == "" {
		return fmt.Errorf("pod %s/%s has no network namespace", pod.Namespace, pod.Name)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	devices := k.sharedState.PodDeviceConfig[podUID]
	preparedData := k.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := k.configureDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			return err
		}
	}
	return nil
}

// StopPodSandbox is called when a pod is stopped by the Container Runtime.
func (k *NetworkDriver) StopPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("StopPodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)

	k.mu.Lock()
	defer k.mu.Unlock()

	devices := k.sharedState.PodDeviceConfig[podUID]
	preparedData := k.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := k.cleanupDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			klog.Errorf("failed to cleanup device %s for pod %s: %v", device.Name, pod.Name, err)
		}
	}
	return nil
}

// RemovePodSandbox is called when a pod is removed by the Container Runtime.
func (k *NetworkDriver) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RemovePodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.sharedState.PodDeviceConfig, podUID)
	delete(k.sharedState.PreparedData, podUID)
	return nil
}

// Helper functions
// runNRIPlugin starts the NRI plugin and keeps it running, it also
// deals with the restart logic in case of failure.
func (k *NetworkDriver) runNRIPlugin(ctx context.Context) {
	attempt := 0
	for attempt < maxAttempts {
		startTime := time.Now()
		if err := k.nriPlugin.Run(ctx); err != nil {
			klog.Errorf("NRI plugin failed: %v", err)
		}

		// if the plugin was stable for a while, reset the backoff counter
		if time.Since(startTime) > stabilityThreshold {
			klog.Infof("nri plugin was stable for more than %v, resetting retry counter", stabilityThreshold)
			attempt = 0
		} else {
			attempt++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			klog.Infof("Restarting NRI plugin (attempt %d/%d)", attempt, maxAttempts)
		}
	}
	klog.Fatalf("NRI plugin failed to restart after %d attempts", maxAttempts)
}

// publishResources publishes the available devices to the DRA plugin.
func (k *NetworkDriver) publishResources(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices, err := k.getDevices()
			if err != nil {
				klog.Errorf("failed to get devices: %v", err)
				continue
			}
			resources := resourceslice.DriverResources{
				Pools: map[string]resourceslice.Pool{
					k.nodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
				},
			}
			if err := k.draPlugin.PublishResources(ctx, resources); err != nil {
				klog.Errorf("failed to publish resources: %v", err)
			}
		}
	}
}

// getNetworkNamespace returns the network namespace path for a pod from the NRI PodSandbox.
func getNetworkNamespace(pod *api.PodSandbox) string {
	for _, ns := range pod.Linux.GetNamespaces() {
		if ns.Type == "network" {
			return ns.Path
		}
	}
	return ""
}

// getDevices discovers all physical network interfaces on the host.
func (k *NetworkDriver) getDevices() ([]resourceapi.Device, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	var devices []resourceapi.Device
	for _, link := range links {
		attrs := link.Attrs()

		// Skip loopback, virtual, and down interfaces
		if attrs.Flags&net.FlagLoopback != 0 || attrs.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(attrs.Name, "veth") || strings.HasPrefix(attrs.Name, "docker") || strings.HasPrefix(attrs.Name, "cni") {
			continue
		}

		device := resourceapi.Device{
			Name: attrs.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"interface-name": {StringValue: &attrs.Name},
				"mac-address":    {StringValue: func() *string { s := attrs.HardwareAddr.String(); return &s }()},
			},
		}
		devices = append(devices, device)
		klog.V(2).Infof("Discovered device: %s", attrs.Name)
	}
	return devices, nil
}

// prepareDevice extracts the target interface name from the claim.
func (k *NetworkDriver) prepareDevice(ctx context.Context, claim *resourceapi.ResourceClaim) (interface{}, error) {
	if claim.Status.Allocation == nil || len(claim.Status.Allocation.Devices.Results) == 0 {
		return nil, fmt.Errorf("claim %s has no allocated devices", claim.Name)
	}

	// For this simple driver, we just need the name of the device to move.
	// The device name is the primary information we need for ConfigureDeviceForPod.
	deviceName := claim.Status.Allocation.Devices.Results[0].Device
	klog.Infof("Preparing device %q for claim %s", deviceName, claim.Name)

	return deviceName, nil
}

// unprepareDevice is a no-op for this simple driver.
func (k *NetworkDriver) unprepareDevice(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	klog.Infof("Unpreparing resources for claim %s", claim.Name)
	return nil
}

// configureDeviceForPod moves the allocated network device into the pod's namespace.
func (k *NetworkDriver) configureDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	// The device name inside the pod will be the same as on the host.
	podInterfaceName := hostDeviceName

	klog.Infof("Moving device %q to pod %s/%s network namespace %s as %q",
		hostDeviceName, podSandbox.Namespace, podSandbox.Name, networkNamespace, podInterfaceName)

	// Here we use the plumbing library to do the actual work.
	_, err := kndnet.NsAttachNetdev(hostDeviceName, networkNamespace, netlink.LinkAttrs{Name: podInterfaceName}, nil)
	return err
}

// cleanupDeviceForPod moves the network device back to the host namespace.
func (k *NetworkDriver) cleanupDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	podInterfaceName := hostDeviceName

	klog.Infof("Moving device %q from pod %s/%s back to host namespace",
		podInterfaceName, podSandbox.Namespace, podSandbox.Name)

	// Use the plumbing library to move the device back.
	return kndnet.NsDetachNetdev(networkNamespace, podInterfaceName, hostDeviceName)
}

//================================================================
// Main Entrypoint
//================================================================

const (
	driverName = "hostdevice.k8s.io"
)

var (
	hostnameOverride string
	kubeconfig       string
	bindAddress      string
	ready            atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&bindAddress, "bind-address", ":9177", "The IP address and port for the metrics and healthz server to serve on")
	flag.StringVar(&hostnameOverride, "hostname-override", "", "If non-empty, will be used as the name of the Node is running on.")
	klog.InitFlags(nil)
	flag.Parse()
}

func main() {
	printVersion()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer cancel()

	// Set up healthz and metrics endpoints
	setupHTTPServer()

	// Set up Kubernetes client
	clientset, err := newClientset()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	nodeName, err := nodeutil.GetHostname(hostnameOverride)
	if err != nil {
		klog.Fatalf("Cannot get node name: %v", err)
	}

	// 1. Create the plugin
	plugin := NewNetworkDriver(driverName, nodeName, clientset)

	// 2. Start the plugin
	if err := plugin.Start(ctx); err != nil {
		klog.Fatalf("Driver failed to start: %v", err)
	}
	defer plugin.Stop()

	ready.Store(true)
	klog.Infof("Driver started successfully on node %s", nodeName)

	// Wait for termination signal
	<-ctx.Done()
	klog.Info("Driver shutting down")
}

func setupHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: bindAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to listen and serve: %v", err)
		}
	}()
}

func newClientset() (kubernetes.Interface, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot create client-go config: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

func printVersion() {
	info, _ := debug.ReadBuildInfo()
	klog.Infof("Version: %s, Go version: %s", info.Main.Version, info.GoVersion)
}
