/*
Copyright 2022 The Kruise Authors.

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

package alibabacloud

import (
	"context"
	gamekruiseiov1alpha1 "github.com/openkruise/kruise-game/apis/v1alpha1"
	"github.com/openkruise/kruise-game/cloudprovider"
	cperrors "github.com/openkruise/kruise-game/cloudprovider/errors"
	provideroptions "github.com/openkruise/kruise-game/cloudprovider/options"
	"github.com/openkruise/kruise-game/cloudprovider/utils"
	"github.com/openkruise/kruise-game/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
	"strings"
	"sync"
)

const (
	SlbNetwork              = "AlibabaCloud-SLB"
	AliasSLB                = "LB-Network"
	SlbIdsConfigName        = "SlbIds"
	PortProtocolsConfigName = "PortProtocols"
	SlbListenerOverrideKey  = "service.beta.kubernetes.io/alibaba-cloud-loadbalancer-force-override-listeners"
	SlbIdAnnotationKey      = "service.beta.kubernetes.io/alibaba-cloud-loadbalancer-id"
	SlbIdLabelKey           = "service.k8s.alibaba/loadbalancer-id"
	SvcSelectorKey          = "statefulset.kubernetes.io/pod-name"
	allocatedPortsKey       = "game.kruise.io/AlibabaCloud-SLB-ports-allocated"
)

type portAllocated map[int32]bool

type SlbPlugin struct {
	maxPort int32
	minPort int32
	cache   map[string]portAllocated
	mutex   sync.RWMutex
}

func (s *SlbPlugin) Name() string {
	return SlbNetwork
}

func (s *SlbPlugin) Alias() string {
	return AliasSLB
}

func (s *SlbPlugin) Init(c client.Client, options cloudprovider.CloudProviderOptions, ctx context.Context) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	slbOptions := options.(provideroptions.AlibabaCloudOptions).SLBOptions
	s.minPort = slbOptions.MinPort
	s.maxPort = slbOptions.MaxPort

	svcList := &corev1.ServiceList{}
	err := c.List(ctx, svcList)
	if err != nil {
		return err
	}

	s.cache = initLbCache(svcList.Items, s.minPort, s.maxPort)
	return nil
}

func initLbCache(svcList []corev1.Service, minPort, maxPort int32) map[string]portAllocated {
	newCache := make(map[string]portAllocated)
	for _, svc := range svcList {
		lbId := svc.Labels[SlbIdLabelKey]
		if lbId != "" && svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			if newCache[lbId] == nil {
				newCache[lbId] = make(portAllocated, maxPort-minPort)
				for i := minPort; i < maxPort; i++ {
					newCache[lbId][i] = false
				}
			}
			for _, port := range getPorts(svc.Spec.Ports) {
				if port <= maxPort && port >= minPort {
					newCache[lbId][port] = true
				}
			}
		}
	}
	return newCache
}

func (s *SlbPlugin) OnPodAdded(c client.Client, pod *corev1.Pod, ctx context.Context) (*corev1.Pod, cperrors.PluginError) {
	networkManager := utils.NewNetworkManager(pod, c)
	err := c.Create(ctx, s.createSvc(networkManager.GetNetworkConfig(), pod, c, ctx))
	return pod, cperrors.ToPluginError(err, cperrors.ApiCallError)
}

func (s *SlbPlugin) OnPodUpdated(c client.Client, pod *corev1.Pod, ctx context.Context) (*corev1.Pod, cperrors.PluginError) {
	networkManager := utils.NewNetworkManager(pod, c)

	networkStatus, _ := networkManager.GetNetworkStatus()
	if networkStatus == nil {
		pod, err := networkManager.UpdateNetworkStatus(gamekruiseiov1alpha1.NetworkStatus{
			CurrentNetworkState: gamekruiseiov1alpha1.NetworkNotReady,
		}, pod)
		return pod, cperrors.ToPluginError(err, cperrors.InternalError)
	}

	// get svc
	svc := &corev1.Service{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      pod.GetName(),
		Namespace: pod.GetNamespace(),
	}, svc)
	if err != nil {
		if errors.IsNotFound(err) {
			return pod, cperrors.ToPluginError(c.Create(ctx, s.createSvc(networkManager.GetNetworkConfig(), pod, c, ctx)), cperrors.ApiCallError)
		}
		return pod, cperrors.NewPluginError(cperrors.ApiCallError, err.Error())
	}

	// disable network
	if networkManager.GetNetworkDisabled() && svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		return pod, cperrors.ToPluginError(c.Update(ctx, svc), cperrors.ApiCallError)
	}

	// enable network
	if !networkManager.GetNetworkDisabled() && svc.Spec.Type == corev1.ServiceTypeClusterIP {
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		return pod, cperrors.ToPluginError(c.Update(ctx, svc), cperrors.ApiCallError)
	}

	// network not ready
	if svc.Status.LoadBalancer.Ingress == nil {
		networkStatus.CurrentNetworkState = gamekruiseiov1alpha1.NetworkNotReady
		pod, err = networkManager.UpdateNetworkStatus(*networkStatus, pod)
		return pod, cperrors.ToPluginError(err, cperrors.InternalError)
	}

	// network ready
	internalAddresses := make([]gamekruiseiov1alpha1.NetworkAddress, 0)
	externalAddresses := make([]gamekruiseiov1alpha1.NetworkAddress, 0)
	for _, port := range svc.Spec.Ports {
		instrIPort := port.TargetPort
		instrEPort := intstr.FromInt(int(port.Port))
		internalAddress := gamekruiseiov1alpha1.NetworkAddress{
			IP: pod.Status.PodIP,
			Ports: []gamekruiseiov1alpha1.NetworkPort{
				{
					Name:     instrIPort.String(),
					Port:     &instrIPort,
					Protocol: port.Protocol,
				},
			},
		}
		externalAddress := gamekruiseiov1alpha1.NetworkAddress{
			IP: svc.Status.LoadBalancer.Ingress[0].IP,
			Ports: []gamekruiseiov1alpha1.NetworkPort{
				{
					Name:     instrIPort.String(),
					Port:     &instrEPort,
					Protocol: port.Protocol,
				},
			},
		}
		internalAddresses = append(internalAddresses, internalAddress)
		externalAddresses = append(externalAddresses, externalAddress)
	}
	networkStatus.InternalAddresses = internalAddresses
	networkStatus.ExternalAddresses = externalAddresses
	networkStatus.CurrentNetworkState = gamekruiseiov1alpha1.NetworkReady
	pod, err = networkManager.UpdateNetworkStatus(*networkStatus, pod)
	return pod, cperrors.ToPluginError(err, cperrors.InternalError)
}

func (s *SlbPlugin) OnPodDeleted(c client.Client, pod *corev1.Pod, ctx context.Context) cperrors.PluginError {
	svc := &corev1.Service{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      pod.GetName(),
		Namespace: pod.GetNamespace(),
	}, svc)
	if err != nil {
		return cperrors.NewPluginError(cperrors.ApiCallError, err.Error())
	}

	for _, port := range getPorts(svc.Spec.Ports) {
		s.deAllocate(svc.Annotations[SlbIdAnnotationKey], port)
	}

	return nil
}

func (s *SlbPlugin) allocate(lbId string, num int) []int32 {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	var ports []int32
	for i := 0; i < num; i++ {
		var port int32
		if s.cache[lbId] == nil {
			s.cache[lbId] = make(portAllocated, s.maxPort-s.minPort)
			for i := s.minPort; i < s.maxPort; i++ {
				s.cache[lbId][i] = false
			}
		}

		for p, allocated := range s.cache[lbId] {
			if !allocated {
				port = p
				break
			}
		}
		s.cache[lbId][port] = true
		ports = append(ports, port)
	}
	return ports
}

func (s *SlbPlugin) deAllocate(lbId string, port int32) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.cache[lbId][port] = false
}

func init() {
	slbPlugin := SlbPlugin{
		mutex: sync.RWMutex{},
	}
	alibabaCloudProvider.registerPlugin(&slbPlugin)
}

func parseLbConfig(conf []gamekruiseiov1alpha1.NetworkConfParams) (string, []int, []corev1.Protocol, bool) {
	var lbId string
	ports := make([]int, 0)
	protocols := make([]corev1.Protocol, 0)
	isFixed := false
	for _, c := range conf {
		switch c.Name {
		case SlbIdsConfigName:
			lbId = c.Value
		case PortProtocolsConfigName:
			for _, pp := range strings.Split(c.Value, ",") {
				ppSlice := strings.Split(pp, "/")
				port, err := strconv.Atoi(ppSlice[0])
				if err != nil {
					continue
				}
				ports = append(ports, port)
				if len(ppSlice) != 2 {
					protocols = append(protocols, corev1.ProtocolTCP)
				} else {
					protocols = append(protocols, corev1.Protocol(ppSlice[1]))
				}
			}
		case FixedConfigName:
			v, err := strconv.ParseBool(c.Value)
			if err != nil {
				continue
			}
			isFixed = v
		}
	}
	return lbId, ports, protocols, isFixed
}

func getPorts(ports []corev1.ServicePort) []int32 {
	var ret []int32
	for _, port := range ports {
		ret = append(ret, port.Port)
	}
	return ret
}

func (s *SlbPlugin) createSvc(conf []gamekruiseiov1alpha1.NetworkConfParams, pod *corev1.Pod, c client.Client, ctx context.Context) *corev1.Service {
	lbId, targetPorts, protocol, isFixed := parseLbConfig(conf)

	var ports []int32
	allocatedPorts := pod.Annotations[allocatedPortsKey]
	if allocatedPorts != "" {
		ports = util.StringToInt32Slice(allocatedPorts, ",")
	} else {
		ports = s.allocate(lbId, len(targetPorts))
		pod.Annotations[allocatedPortsKey] = util.Int32SliceToString(ports, ",")
	}

	svcPorts := make([]corev1.ServicePort, 0)
	for i := 0; i < len(targetPorts); i++ {
		svcPorts = append(svcPorts, corev1.ServicePort{
			Name:       strconv.Itoa(targetPorts[i]),
			Port:       ports[i],
			Protocol:   protocol[i],
			TargetPort: intstr.FromInt(targetPorts[i]),
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.GetName(),
			Namespace: pod.GetNamespace(),
			Annotations: map[string]string{
				SlbListenerOverrideKey: "true",
				SlbIdAnnotationKey:     lbId,
			},
			OwnerReferences: getSvcOwnerReference(c, ctx, pod, isFixed),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				SvcSelectorKey: pod.GetName(),
			},
			Ports: svcPorts,
		},
	}
	return svc
}

func getSvcOwnerReference(c client.Client, ctx context.Context, pod *corev1.Pod, isFixed bool) []metav1.OwnerReference {
	ownerReferences := []metav1.OwnerReference{
		{
			APIVersion:         pod.APIVersion,
			Kind:               pod.Kind,
			Name:               pod.GetName(),
			UID:                pod.GetUID(),
			Controller:         pointer.BoolPtr(true),
			BlockOwnerDeletion: pointer.BoolPtr(true),
		},
	}
	if isFixed {
		gss, err := util.GetGameServerSetOfPod(pod, c, ctx)
		if err == nil {
			ownerReferences = []metav1.OwnerReference{
				{
					APIVersion:         gss.APIVersion,
					Kind:               gss.Kind,
					Name:               gss.GetName(),
					UID:                gss.GetUID(),
					Controller:         pointer.BoolPtr(true),
					BlockOwnerDeletion: pointer.BoolPtr(true),
				},
			}
		}
	}
	return ownerReferences
}
