/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gke

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/gce"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	provider_gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gce_api "google.golang.org/api/compute/v1"
	gcfg "gopkg.in/gcfg.v1"
	"k8s.io/klog"
)

// GcpCloudProviderMode allows to pass information whether the cluster is in NAP mode.
type GcpCloudProviderMode string

const (
	// ModeGKE means that the cluster is running
	ModeGKE GcpCloudProviderMode = "gke"

	// ModeGKENAP means that the cluster is running on GKE with autoprovisioning enabled.
	// TODO(maciekpytel): remove this when NAP API is available in normal client
	ModeGKENAP GcpCloudProviderMode = "gke_nap"
)

const (
	gkeOperationWaitTimeout    = 120 * time.Second
	refreshInterval            = 1 * time.Minute
	machinesRefreshInterval    = 1 * time.Hour
	httpTimeout                = 30 * time.Second
	nodeAutoprovisioningPrefix = "nap"
	napMaxNodes                = 1000
	napMinNodes                = 0
	scaleToZeroSupported       = true
)

var (
	defaultOAuthScopes []string = []string{
		"https://www.googleapis.com/auth/compute",
		"https://www.googleapis.com/auth/devstorage.read_only",
		"https://www.googleapis.com/auth/service.management.readonly",
		"https://www.googleapis.com/auth/servicecontrol"}
	supportedResources = map[string]bool{}
)

func init() {
	supportedResources[cloudprovider.ResourceNameCores] = true
	supportedResources[cloudprovider.ResourceNameMemory] = true
	for _, gpuType := range supportedGpuTypes {
		supportedResources[gpuType] = true
	}
}

// GkeManager handles GCE and GKE communication and data caching.
type GkeManager interface {
	// Refresh triggers refresh of cached resources.
	Refresh() error
	// Cleanup cleans up open resources before the cloud provider is destroyed, i.e. go routines etc.
	Cleanup() error

	// GetLocation returns cluster's location.
	GetLocation() string
	// GetProjectId returns id of GCE project to which the cluster belongs.
	GetProjectId() string
	// GetClusterName returns the name of the GKE cluster.
	GetClusterName() string
	// GetMigs returns a list of registered MIGs.
	GetMigs() []*gce.MigInformation
	// GetMigNodes returns mig nodes.
	GetMigNodes(mig gce.Mig) ([]string, error)
	// GetMigForInstance returns MigConfig of the given Instance
	GetMigForInstance(instance *gce.GceRef) (gce.Mig, error)
	// GetMigTemplateNode returns a template node for MIG.
	GetMigTemplateNode(mig *GkeMig) (*apiv1.Node, error)
	// GetMigSize gets MIG size.
	GetMigSize(mig gce.Mig) (int64, error)
	// GetNodeLocations returns a list of locations with nodes.
	GetNodeLocations() []string
	// GetResourceLimiter returns resource limiter.
	GetResourceLimiter() (*cloudprovider.ResourceLimiter, error)

	// SetMigSize sets MIG size.
	SetMigSize(mig gce.Mig, size int64) error
	// DeleteInstances deletes the given instances. All instances must be controlled by the same MIG.
	DeleteInstances(instances []*gce.GceRef) error
	// CreateNodePool creates a MIG based on blueprint and returns the newly created MIG.
	CreateNodePool(mig *GkeMig) (*GkeMig, error)
	// DeleteNodePool deletes a MIG from cloud provider.
	DeleteNodePool(toBeRemoved *GkeMig) error
}

// gkeConfigurationCache is used for storing cached cluster configuration.
type gkeConfigurationCache struct {
	sync.Mutex
	nodeLocations []string
}

func (cache *gkeConfigurationCache) setNodeLocations(locations []string) {
	cache.Lock()
	defer cache.Unlock()

	cache.nodeLocations = make([]string, len(locations))
	copy(cache.nodeLocations, locations)
}

func (cache *gkeConfigurationCache) getNodeLocations() []string {
	cache.Lock()
	defer cache.Unlock()

	locations := make([]string, len(cache.nodeLocations))
	copy(locations, cache.nodeLocations)
	return locations
}

type gkeManagerImpl struct {
	cache                    gce.GceCache
	gkeConfigurationCache    gkeConfigurationCache
	lastRefresh              time.Time
	machinesCacheLastRefresh time.Time

	GkeService AutoscalingGkeClient
	GceService gce.AutoscalingGceClient

	location    string
	projectId   string
	clusterName string
	mode        GcpCloudProviderMode
	templates   *GkeTemplateBuilder
	interrupt   chan struct{}
	regional    bool
}

// CreateGkeManager constructs GkeManager object.
func CreateGkeManager(configReader io.Reader, mode GcpCloudProviderMode, clusterName string, regional bool) (GkeManager, error) {
	// Create Google Compute Engine token.
	var err error
	tokenSource := google.ComputeTokenSource("")
	if len(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")) > 0 {
		tokenSource, err = google.DefaultTokenSource(oauth2.NoContext, gce_api.ComputeScope)
		if err != nil {
			return nil, err
		}
	}
	var projectId, location string
	if configReader != nil {
		var cfg provider_gce.ConfigFile
		if err := gcfg.ReadInto(&cfg, configReader); err != nil {
			klog.Errorf("Couldn't read config: %v", err)
			return nil, err
		}
		if cfg.Global.TokenURL == "" {
			klog.Warning("Empty tokenUrl in cloud config")
		} else {
			tokenSource = provider_gce.NewAltTokenSource(cfg.Global.TokenURL, cfg.Global.TokenBody)
			klog.V(1).Infof("Using TokenSource from config %#v", tokenSource)
		}
		projectId = cfg.Global.ProjectID
		location = cfg.Global.LocalZone
	} else {
		klog.V(1).Infof("Using default TokenSource %#v", tokenSource)
	}
	if len(projectId) == 0 || len(location) == 0 {
		// XXX: On GKE discoveredProjectId is hosted master project and
		// not the project we want to use, however, zone seems to not
		// be specified in config. For now we can just assume that hosted
		// master project is in the same zone as cluster and only use
		// discoveredZone.
		discoveredProjectId, discoveredLocation, err := getProjectAndLocation(regional)
		if err != nil {
			return nil, err
		}
		if len(projectId) == 0 {
			projectId = discoveredProjectId
		}
		if len(location) == 0 {
			location = discoveredLocation
		}
	}
	klog.V(1).Infof("GCE projectId=%s location=%s", projectId, location)

	// Create Google Compute Engine service.
	client := oauth2.NewClient(oauth2.NoContext, tokenSource)
	client.Timeout = httpTimeout
	gceService, err := gce.NewAutoscalingGceClientV1(client, projectId)
	if err != nil {
		return nil, err
	}
	manager := &gkeManagerImpl{
		cache:       gce.NewGceCache(gceService),
		GceService:  gceService,
		location:    location,
		regional:    regional,
		projectId:   projectId,
		clusterName: clusterName,
		mode:        mode,
		templates:   &GkeTemplateBuilder{},
		interrupt:   make(chan struct{}),
	}

	switch mode {
	case ModeGKE:
		gkeService, err := NewAutoscalingGkeClientV1(client, projectId, location, clusterName)
		if err != nil {
			return nil, err
		}
		manager.GkeService = gkeService
	case ModeGKENAP:
		gkeBetaService, err := NewAutoscalingGkeClientV1beta1(client, projectId, location, clusterName)
		if err != nil {
			return nil, err
		}
		manager.GkeService = gkeBetaService
		klog.V(1).Info("Using GKE-NAP mode")
	}

	if err := manager.forceRefresh(); err != nil {
		return nil, err
	}

	go wait.Until(func() {
		if err := manager.cache.RegenerateInstancesCache(); err != nil {
			klog.Errorf("Error while regenerating Mig cache: %v", err)
		}
	}, time.Hour, manager.interrupt)

	return manager, nil
}

// Cleanup closes the channel to stop the goroutine refreshing cache.
func (m *gkeManagerImpl) Cleanup() error {
	close(m.interrupt)
	return nil
}

func (m *gkeManagerImpl) assertGKENAP() {
	if m.mode != ModeGKENAP {
		klog.Fatalf("This should run only in GKE mode with autoprovisioning enabled")
	}
}

func (m *gkeManagerImpl) refreshNodePools(nodePools []NodePool) error {
	existingMigs := map[gce.GceRef]struct{}{}
	changed := false

	for _, nodePool := range nodePools {
		for _, igurl := range nodePool.InstanceGroupUrls {
			project, zone, name, err := gce.ParseIgmUrl(igurl)
			if err != nil {
				return err
			}
			mig := &GkeMig{
				gceRef: gce.GceRef{
					Name:    name,
					Zone:    zone,
					Project: project,
				},
				gkeManager:      m,
				exist:           true,
				autoprovisioned: nodePool.Autoprovisioned,
				nodePoolName:    nodePool.Name,
				minSize:         int(nodePool.MinNodeCount),
				maxSize:         int(nodePool.MaxNodeCount),
			}
			existingMigs[mig.GceRef()] = struct{}{}

			if m.registerMig(mig) {
				changed = true
			}
		}
	}
	for _, mig := range m.cache.GetMigs() {
		if _, found := existingMigs[mig.Config.GceRef()]; !found {
			m.cache.UnregisterMig(mig.Config)
			changed = true
		}
	}
	if changed {
		return m.cache.RegenerateInstancesCache()
	}
	return nil
}

// GetNodeLocation returns a list of zones in which cluster has nodes.
func (m *gkeManagerImpl) GetNodeLocations() []string {
	return m.gkeConfigurationCache.getNodeLocations()
}

func (m *gkeManagerImpl) registerMig(mig *GkeMig) bool {
	changed := m.cache.RegisterMig(mig)
	if changed {
		// Try to build a node from template to validate that this group
		// can be scaled up from 0 nodes.
		// We may never need to do it, so just log error if it fails.
		if _, err := m.GetMigTemplateNode(mig); err != nil {
			klog.Errorf("Can't build node from template for %s, won't be able to scale from 0: %v", mig.GceRef().String(), err)
		}
	}
	return changed
}

// DeleteNodePool deletes a node pool corresponding to the given MIG.
func (m *gkeManagerImpl) DeleteNodePool(toBeRemoved *GkeMig) error {
	m.assertGKENAP()

	if !toBeRemoved.Autoprovisioned() {
		return fmt.Errorf("only autoprovisioned node pools can be deleted")
	}
	err := m.GkeService.DeleteNodePool(toBeRemoved.NodePoolName())
	if err != nil {
		return err
	}
	return m.refreshClusterResources()
}

// CreateNodePool creates a node pool based on provided spec and returns newly created MIG.
func (m *gkeManagerImpl) CreateNodePool(mig *GkeMig) (*GkeMig, error) {
	m.assertGKENAP()

	err := m.GkeService.CreateNodePool(mig)
	if err != nil {
		return nil, err
	}
	err = m.refreshClusterResources()
	if err != nil {
		return nil, err
	}
	for _, existingMig := range m.cache.GetMigs() {
		gkeMig, ok := existingMig.Config.(*GkeMig)
		if !ok {
			// This is "should never happen" branch.
			// Report error as InternalError since it would signify a
			// serious bug in autoscaler code.
			errMsg := fmt.Sprintf("Mig %s is not GkeMig: got %v, want GkeMig", existingMig.Config.GceRef().String(), reflect.TypeOf(existingMig.Config))
			klog.Error(errMsg)
			return nil, errors.NewAutoscalerError(errors.InternalError, errMsg)
		}
		if gkeMig.NodePoolName() == mig.NodePoolName() {
			return gkeMig, nil
		}
	}
	return nil, fmt.Errorf("node pool %s not found", mig.NodePoolName())
}

func (m *gkeManagerImpl) refreshMachinesCache() error {
	if m.machinesCacheLastRefresh.Add(machinesRefreshInterval).After(time.Now()) {
		return nil
	}
	// Machine types cache is only updated directly after refreshing cluster resources, so value from cache should be good enough.
	locations := m.gkeConfigurationCache.getNodeLocations()
	machinesCache := make(map[gce.MachineTypeKey]*gce_api.MachineType)
	for _, location := range locations {
		machineTypes, err := m.GceService.FetchMachineTypes(location)
		if err != nil {
			return err
		}
		for _, machineType := range machineTypes {
			machinesCache[gce.MachineTypeKey{location, machineType.Name}] = machineType
		}

	}
	m.cache.SetMachinesCache(machinesCache)
	nextRefresh := time.Now()
	m.machinesCacheLastRefresh = nextRefresh
	klog.V(2).Infof("Refreshed machine types, next refresh after %v", nextRefresh)
	return nil
}

// GetMigSize gets MIG size.
func (m *gkeManagerImpl) GetMigSize(mig gce.Mig) (int64, error) {
	targetSize, err := m.GceService.FetchMigTargetSize(mig.GceRef())
	if err != nil {
		return -1, err
	}
	return targetSize, nil
}

// SetMigSize sets MIG size.
func (m *gkeManagerImpl) SetMigSize(mig gce.Mig, size int64) error {
	klog.V(0).Infof("Setting mig size %s to %d", mig.Id(), size)
	return m.GceService.ResizeMig(mig.GceRef(), size)
}

// DeleteInstances deletes the given instances. All instances must be controlled by the same MIG.
func (m *gkeManagerImpl) DeleteInstances(instances []*gce.GceRef) error {
	if len(instances) == 0 {
		return nil
	}
	commonMig, err := m.GetMigForInstance(instances[0])
	if err != nil {
		return err
	}
	for _, instance := range instances {
		mig, err := m.GetMigForInstance(instance)
		if err != nil {
			return err
		}
		if mig != commonMig {
			return fmt.Errorf("Cannot delete instances which don't belong to the same MIG.")
		}
	}

	return m.GceService.DeleteInstances(commonMig.GceRef(), instances)
}

func (m *gkeManagerImpl) GetMigs() []*gce.MigInformation {
	return m.cache.GetMigs()
}

// GetMigForInstance returns MIG to which the given instance belongs.
func (m *gkeManagerImpl) GetMigForInstance(instance *gce.GceRef) (gce.Mig, error) {
	return m.cache.GetMigForInstance(instance)
}

// GetMigNodes returns instances that belong to a MIG.
func (m *gkeManagerImpl) GetMigNodes(mig gce.Mig) ([]string, error) {
	instances, err := m.GceService.FetchMigInstances(mig.GceRef())
	if err != nil {
		return []string{}, err
	}
	result := make([]string, 0)
	for _, ref := range instances {
		result = append(result, fmt.Sprintf("gce://%s/%s/%s", ref.Project, ref.Zone, ref.Name))
	}
	return result, nil
}

// GetLocation returns cluster's location.
func (m *gkeManagerImpl) GetLocation() string {
	return m.location
}

// GetProjectId returns id of GCE project to which the cluster belongs.
func (m *gkeManagerImpl) GetProjectId() string {
	return m.projectId
}

// GetClusterName returns the name of GKE cluster.
func (m *gkeManagerImpl) GetClusterName() string {
	return m.clusterName
}

// Refresh triggers refresh of cached resources.
func (m *gkeManagerImpl) Refresh() error {
	if m.lastRefresh.Add(refreshInterval).After(time.Now()) {
		return nil
	}
	return m.forceRefresh()
}

func (m *gkeManagerImpl) forceRefresh() error {
	if err := m.refreshClusterResources(); err != nil {
		klog.Errorf("Failed to refresh GKE cluster resources: %v", err)
		return err
	}
	if err := m.refreshMachinesCache(); err != nil {
		klog.Errorf("Failed to fetch machine types: %v", err)
		return err
	}
	m.lastRefresh = time.Now()
	klog.V(2).Infof("Refreshed GCE resources, next refresh after %v", m.lastRefresh.Add(refreshInterval))
	return nil
}

func (m *gkeManagerImpl) refreshClusterResources() error {
	cluster, err := m.GkeService.GetCluster()
	if err != nil {
		return err
	}
	m.refreshNodePools(cluster.NodePools)
	m.refreshResourceLimiter(cluster.ResourceLimiter)
	m.gkeConfigurationCache.setNodeLocations(cluster.Locations)
	return nil
}

func (m *gkeManagerImpl) buildMigFromFlag(flag string) (gce.Mig, error) {
	s, err := dynamic.SpecFromString(flag, scaleToZeroSupported)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node group spec: %v", err)
	}
	return m.buildMigFromSpec(s)
}

func (m *gkeManagerImpl) buildMigFromSpec(s *dynamic.NodeGroupSpec) (gce.Mig, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid node group spec: %v", err)
	}
	project, zone, name, err := gce.ParseMigUrl(s.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mig url: %s got error: %v", s.Name, err)
	}
	mig := &GkeMig{
		gceRef: gce.GceRef{
			Project: project,
			Name:    name,
			Zone:    zone,
		},
		gkeManager: m,
		minSize:    s.MinSize,
		maxSize:    s.MaxSize,
		exist:      true,
	}
	return mig, nil
}

func (m *gkeManagerImpl) refreshResourceLimiter(resourceLimiter *cloudprovider.ResourceLimiter) {
	if m.mode == ModeGKENAP {
		if resourceLimiter != nil {
			klog.V(2).Infof("Refreshed resource limits: %s", resourceLimiter.String())
			m.cache.SetResourceLimiter(resourceLimiter)
		} else {
			oldLimits, _ := m.cache.GetResourceLimiter()
			klog.Errorf("Resource limits should always be defined in NAP mode, but they appear to be empty. Using possibly outdated limits: %v", oldLimits.String())
		}
	}
}

// GetResourceLimiter returns resource limiter from cache.
func (m *gkeManagerImpl) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return m.cache.GetResourceLimiter()
}

func (m *gkeManagerImpl) clearMachinesCache() {
	if m.machinesCacheLastRefresh.Add(machinesRefreshInterval).After(time.Now()) {
		return
	}

	machinesCache := make(map[gce.MachineTypeKey]*gce_api.MachineType)
	m.cache.SetMachinesCache(machinesCache)
	nextRefresh := time.Now()
	m.machinesCacheLastRefresh = nextRefresh
	klog.V(2).Infof("Cleared machine types cache, next clear after %v", nextRefresh)
}

// Code borrowed from gce cloud provider. Reuse the original as soon as it becomes public.
func getProjectAndLocation(regional bool) (string, string, error) {
	result, err := metadata.Get("instance/zone")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(result, "/")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("unexpected response: %s", result)
	}
	location := parts[3]
	if regional {
		location, err = provider_gce.GetGCERegion(location)
		if err != nil {
			return "", "", err
		}
	}
	projectID, err := metadata.ProjectID()
	if err != nil {
		return "", "", err
	}
	return projectID, location, nil
}

// GetMigTemplateNode constructs a node:
// - from GCE instance template of the given MIG, if the MIG already exists,
// - from MIG spec, if it doesn't exist, but may be autoprovisioned.
func (m *gkeManagerImpl) GetMigTemplateNode(mig *GkeMig) (*apiv1.Node, error) {
	if mig.Exist() {
		template, err := m.GceService.FetchMigTemplate(mig.GceRef())
		if err != nil {
			return nil, err
		}
		cpu, mem, err := m.getCpuAndMemoryForMachineType(template.Properties.MachineType, mig.GceRef().Zone)
		if err != nil {
			return nil, err
		}
		return m.templates.BuildNodeFromTemplate(mig, template, cpu, mem)
	} else if mig.Autoprovisioned() {
		cpu, mem, err := m.getCpuAndMemoryForMachineType(mig.Spec().MachineType, mig.GceRef().Zone)
		if err != nil {
			return nil, err
		}
		return m.templates.BuildNodeFromMigSpec(mig, cpu, mem)
	}
	return nil, fmt.Errorf("unable to get node info for %s", mig.GceRef().String())
}

func (m *gkeManagerImpl) getCpuAndMemoryForMachineType(machineType string, zone string) (cpu int64, mem int64, err error) {
	if strings.HasPrefix(machineType, "custom-") {
		return parseCustomMachineType(machineType)
	}
	machine := m.cache.GetMachineFromCache(machineType, zone)
	if machine == nil {
		machine, err = m.GceService.FetchMachineType(zone, machineType)
		if err != nil {
			return 0, 0, err
		}
		m.cache.AddMachineToCache(machineType, zone, machine)
	}
	return machine.GuestCpus, machine.MemoryMb * bytesPerMB, nil
}

func parseCustomMachineType(machineType string) (cpu, mem int64, err error) {
	// example custom-2-2816
	var count int
	count, err = fmt.Sscanf(machineType, "custom-%d-%d", &cpu, &mem)
	if err != nil {
		return
	}
	if count != 2 {
		return 0, 0, fmt.Errorf("failed to parse all params in %s", machineType)
	}
	// Mb to bytes
	mem = mem * bytesPerMB
	return
}
