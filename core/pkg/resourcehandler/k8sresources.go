package resourcehandler

import (
	"context"
	"fmt"
	"strings"

	"github.com/armosec/kubescape/core/cautils"
	"github.com/armosec/kubescape/core/cautils/logger"
	"github.com/armosec/kubescape/core/cautils/logger/helpers"
	"github.com/armosec/kubescape/core/pkg/hostsensorutils"
	"github.com/armosec/opa-utils/objectsenvelopes"
	"github.com/armosec/opa-utils/reporthandling/apis"

	"github.com/armosec/k8s-interface/cloudsupport"
	"github.com/armosec/k8s-interface/k8sinterface"
	"github.com/armosec/k8s-interface/workloadinterface"

	"github.com/armosec/armoapi-go/armotypes"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
)

type K8sResourceHandler struct {
	k8s               *k8sinterface.KubernetesApi
	hostSensorHandler hostsensorutils.IHostSensor
	fieldSelector     IFieldSelector
	rbacObjectsAPI    *cautils.RBACObjects
	registryAdaptors  *RegistryAdaptors
}

func NewK8sResourceHandler(k8s *k8sinterface.KubernetesApi, fieldSelector IFieldSelector, hostSensorHandler hostsensorutils.IHostSensor, rbacObjects *cautils.RBACObjects, registryAdaptors *RegistryAdaptors) *K8sResourceHandler {
	return &K8sResourceHandler{
		k8s:               k8s,
		fieldSelector:     fieldSelector,
		hostSensorHandler: hostSensorHandler,
		rbacObjectsAPI:    rbacObjects,
		registryAdaptors:  registryAdaptors,
	}
}

func (k8sHandler *K8sResourceHandler) GetResources(sessionObj *cautils.OPASessionObj, designator *armotypes.PortalDesignator) (*cautils.K8SResources, map[string]workloadinterface.IMetadata, *cautils.ArmoResources, error) {
	allResources := map[string]workloadinterface.IMetadata{}

	// get k8s resources
	logger.L().Info("Accessing Kubernetes objects")

	cautils.StartSpinner()
	resourceToControl := make(map[string][]string)
	// build resources map
	// map resources based on framework required resources: map["/group/version/kind"][]<k8s workloads ids>
	k8sResourcesMap := setK8sResourceMap(sessionObj.Policies)

	// get namespace and labels from designator (ignore cluster labels)
	_, namespace, labels := armotypes.DigestPortalDesignator(designator)

	// pull k8s recourses
	armoResourceMap := setArmoResourceMap(sessionObj.Policies, resourceToControl)

	// map of armo resources to control_ids
	sessionObj.ResourceToControlsMap = resourceToControl

	if err := k8sHandler.pullResources(k8sResourcesMap, allResources, namespace, labels); err != nil {
		cautils.StopSpinner()
		return k8sResourcesMap, allResources, armoResourceMap, err
	}

	numberOfWorkerNodes, err := k8sHandler.pullWorkerNodesNumber()

	if err != nil {
		logger.L().Debug("failed to collect worker nodes number", helpers.Error(err))
	} else {
		sessionObj.Metadata.ClusterMetadata.NumberOfWorkerNodes = numberOfWorkerNodes
	}

	imgVulnResources := cautils.MapImageVulnResources(armoResourceMap)
	// check that controls use image vulnerability resources
	if len(imgVulnResources) > 0 {
		if err := k8sHandler.registryAdaptors.collectImagesVulnerabilities(k8sResourcesMap, allResources, armoResourceMap); err != nil {
			logger.L().Warning("failed to collect image vulnerabilities", helpers.Error(err))
		}
	}

	hostResources := cautils.MapHostResources(armoResourceMap)
	// check that controls use host sensor resources
	if len(hostResources) > 0 {
		if sessionObj.Metadata.ScanMetadata.HostScanner {
			infoMap, err := k8sHandler.collectHostResources(allResources, armoResourceMap)
			if err != nil {
				logger.L().Warning("failed to collect host scanner resources", helpers.Error(err))
				cautils.SetInfoMapForResources(err.Error(), hostResources, sessionObj.InfoMap)
			} else if k8sHandler.hostSensorHandler == nil {
				// using hostSensor mock
				cautils.SetInfoMapForResources("failed to init host scanner", hostResources, sessionObj.InfoMap)
			} else {
				sessionObj.InfoMap = infoMap
			}
		} else {
			cautils.SetInfoMapForResources("enable-host-scan flag not used", hostResources, sessionObj.InfoMap)
		}
	}

	if err := k8sHandler.collectRbacResources(allResources); err != nil {
		logger.L().Warning("failed to collect rbac resources", helpers.Error(err))
	}

	cloudResources := cautils.MapCloudResources(armoResourceMap)
	// check that controls use cloud resources
	if len(cloudResources) > 0 {
		provider, err := getCloudProviderDescription(allResources, armoResourceMap)
		if err != nil {
			cautils.SetInfoMapForResources(err.Error(), cloudResources, sessionObj.InfoMap)
			logger.L().Warning("failed to collect cloud data", helpers.Error(err))
		}
		if provider != "" {
			sessionObj.Metadata.ClusterMetadata.CloudProvider = provider
		}
	}

	cautils.StopSpinner()
	logger.L().Success("Accessed to Kubernetes objects")

	return k8sResourcesMap, allResources, armoResourceMap, nil
}

func (k8sHandler *K8sResourceHandler) GetClusterAPIServerInfo() *version.Info {
	clusterAPIServerInfo, err := k8sHandler.k8s.DiscoveryClient.ServerVersion()
	if err != nil {
		logger.L().Error("failed to discover API server information", helpers.Error(err))
		return nil
	}
	return clusterAPIServerInfo
}

func (k8sHandler *K8sResourceHandler) pullResources(k8sResources *cautils.K8SResources, allResources map[string]workloadinterface.IMetadata, namespace string, labels map[string]string) error {

	var errs error
	for groupResource := range *k8sResources {
		apiGroup, apiVersion, resource := k8sinterface.StringToResourceGroup(groupResource)
		gvr := schema.GroupVersionResource{Group: apiGroup, Version: apiVersion, Resource: resource}
		result, err := k8sHandler.pullSingleResource(&gvr, namespace, labels)
		if err != nil {
			if !strings.Contains(err.Error(), "the server could not find the requested resource") {
				// handle error
				if errs == nil {
					errs = err
				} else {
					errs = fmt.Errorf("%s; %s", errs, err.Error())
				}
			}
			continue
		}
		// store result as []map[string]interface{}
		metaObjs := ConvertMapListToMeta(k8sinterface.ConvertUnstructuredSliceToMap(result))
		for i := range metaObjs {
			allResources[metaObjs[i].GetID()] = metaObjs[i]
		}
		(*k8sResources)[groupResource] = workloadinterface.ListMetaIDs(metaObjs)
	}
	return errs
}

func (k8sHandler *K8sResourceHandler) pullSingleResource(resource *schema.GroupVersionResource, namespace string, labels map[string]string) ([]unstructured.Unstructured, error) {
	resourceList := []unstructured.Unstructured{}
	// set labels
	listOptions := metav1.ListOptions{}
	fieldSelectors := k8sHandler.fieldSelector.GetNamespacesSelectors(resource)
	for i := range fieldSelectors {

		listOptions.FieldSelector = fieldSelectors[i]

		if len(labels) > 0 {
			set := k8slabels.Set(labels)
			listOptions.LabelSelector = set.AsSelector().String()
		}

		// set dynamic object
		var clientResource dynamic.ResourceInterface
		if namespace != "" && k8sinterface.IsNamespaceScope(resource) {
			clientResource = k8sHandler.k8s.DynamicClient.Resource(*resource).Namespace(namespace)
		} else {
			clientResource = k8sHandler.k8s.DynamicClient.Resource(*resource)
		}

		// list resources
		result, err := clientResource.List(context.Background(), listOptions)
		if err != nil || result == nil {
			return nil, fmt.Errorf("failed to get resource: %v, namespace: %s, labelSelector: %v, reason: %v", resource, namespace, listOptions.LabelSelector, err)
		}

		resourceList = append(resourceList, result.Items...)

	}

	return resourceList, nil

}
func ConvertMapListToMeta(resourceMap []map[string]interface{}) []workloadinterface.IMetadata {
	workloads := []workloadinterface.IMetadata{}
	for i := range resourceMap {
		if w := objectsenvelopes.NewObject(resourceMap[i]); w != nil {
			workloads = append(workloads, w)
		}
	}
	return workloads
}

// func (k8sHandler *K8sResourceHandler) collectHostResourcesAPI(allResources map[string]workloadinterface.IMetadata, resourcesMap *cautils.K8SResources) error {

// 	HostSensorAPI := map[string]string{
// 		"bla/v1": "",
// 	}
// 	for apiVersion := range allResources {
// 		if HostSensorAPI == apiVersion {
// 			k8sHandler.collectHostResources()
// 		}
// 	}
// 	return nil
// }
func (k8sHandler *K8sResourceHandler) collectHostResources(allResources map[string]workloadinterface.IMetadata, armoResourceMap *cautils.ArmoResources) (map[string]apis.StatusInfo, error) {
	logger.L().Debug("Collecting host scanner resources")
	hostResources, infoMap, err := k8sHandler.hostSensorHandler.CollectResources()
	if err != nil {
		return nil, err
	}

	for rscIdx := range hostResources {
		group, version := getGroupNVersion(hostResources[rscIdx].GetApiVersion())
		groupResource := k8sinterface.JoinResourceTriplets(group, version, hostResources[rscIdx].GetKind())
		allResources[hostResources[rscIdx].GetID()] = &hostResources[rscIdx]

		grpResourceList, ok := (*armoResourceMap)[groupResource]
		if !ok {
			grpResourceList = make([]string, 0)
		}
		(*armoResourceMap)[groupResource] = append(grpResourceList, hostResources[rscIdx].GetID())
	}
	return infoMap, nil
}

func (k8sHandler *K8sResourceHandler) collectRbacResources(allResources map[string]workloadinterface.IMetadata) error {
	logger.L().Debug("Collecting rbac resources")

	if k8sHandler.rbacObjectsAPI == nil {
		return nil
	}
	allRbacResources, err := k8sHandler.rbacObjectsAPI.ListAllResources()
	if err != nil {
		return err
	}
	for k, v := range allRbacResources {
		allResources[k] = v
	}
	return nil
}

func getCloudProviderDescription(allResources map[string]workloadinterface.IMetadata, armoResourceMap *cautils.ArmoResources) (string, error) {
	logger.L().Debug("Collecting cloud data")
	cloudProvider := initCloudProvider()
	cluster := cloudProvider.getKubeCluster()
	clusterName := cloudProvider.getKubeClusterName()
	provider := getCloudProvider()
	region, err := cloudProvider.getRegion(cluster, provider)
	if err != nil {
		return provider, err
	}
	project, err := cloudProvider.getProject(cluster, provider)
	if err != nil {
		return provider, err
	}

	if provider != "" {
		logger.L().Debug("cloud", helpers.String("cluster", cluster), helpers.String("clusterName", clusterName), helpers.String("provider", provider), helpers.String("region", region), helpers.String("project", project))

		wl, err := cloudsupport.GetDescriptiveInfoFromCloudProvider(clusterName, provider, region, project)
		if err != nil {
			// Return error with useful info on how to configure credentials for getting cloud provider info
			switch provider {
			case "gke":
				return provider, fmt.Errorf("could not get descriptive information about gke cluster: %s using sdk client. See https://developers.google.com/accounts/docs/application-default-credentials for more information", cluster)
			case "eks":
				return provider, fmt.Errorf("could not get descriptive information about eks cluster: %s using sdk client. Check out how to configure credentials in https://docs.aws.amazon.com/sdk-for-go/api/", cluster)
			case "aks":
				return provider, fmt.Errorf("could not get descriptive information about aks cluster: %s. %v", cluster, err.Error())
			}
			return provider, err
		}
		allResources[wl.GetID()] = wl
		(*armoResourceMap)[fmt.Sprintf("%s/%s", wl.GetApiVersion(), wl.GetKind())] = []string{wl.GetID()}
	}
	return provider, nil

}

func (k8sHandler *K8sResourceHandler) pullWorkerNodesNumber() (int, error) {
	// labels used for control plane
	listOptions := metav1.ListOptions{
		LabelSelector: "!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master",
	}
	nodesList, err := k8sHandler.k8s.KubernetesClient.CoreV1().Nodes().List(context.TODO(), listOptions)
	if err != nil {
		return 0, err
	}
	nodesNumber := 0
	if nodesList != nil {
		nodesNumber = len(nodesList.Items)
	}
	return nodesNumber, nil
}
