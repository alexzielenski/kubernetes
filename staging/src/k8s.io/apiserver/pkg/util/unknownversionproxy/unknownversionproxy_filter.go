/*
Copyright 2019 The Kubernetes Authors.

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

package unknownversionproxy

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	v1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	apiserverinternallister "k8s.io/client-go/listers/apiserverinternal/v1alpha1"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const ConfigConsumerAsFieldManager = "api-server-proxy-v1"

// Interface defines how the API Priority and Fairness filter interacts with the underlying system.
type Interface interface {
	Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer )  http.Handler
}

// New creates a new instance to implement API server proxy
func New(
	informerFactory kubeinformers.SharedInformerFactory,
) Interface {
	return NewTestable(TestableConfig{
		Name:                   "Controller",
		InformerFactory:        informerFactory,
	})
}

// TestableConfig carries the parameters to an implementation that is testable
type TestableConfig struct {
	// Name of the controller
	Name string

	// InformerFactory to use in building the controller
	InformerFactory kubeinformers.SharedInformerFactory
}

// NewTestable is extra flexible to facilitate testing
func NewTestable(config TestableConfig) Interface {
	return newTestableController(config)
}

func (cfgCtlr *configController) Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer ) http.Handler {
	if cfgCtlr.svLister == nil {
		klog.Warningf("api server interoperability proxy support not found, skipping")
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gv := schema.GroupVersion{Group: "unknown", Version: "unknown"}
		requestInfo, ok := apirequest.RequestInfoFrom(req.Context())
		if ok {
			gv.Group = requestInfo.APIGroup
			gv.Version = requestInfo.APIVersion
		}

		storageVersions, err := cfgCtlr.svLister.Get(fmt.Sprintf("%s.%s", requestInfo.APIGroup, requestInfo.Resource))
		if err != nil {
			// this means that resource is an aggregated API or a CR, pass as it is
			klog.Warningf(fmt.Sprintf("No StorageVersion found for the GV: %v skipping proxying", gv))
			return
		}

		var serviceableBy []string

		for _, sv := range storageVersions.Status.StorageVersions {
			for _, version := range sv.DecodableVersions {
				if version == requestInfo.APIVersion {
					// found the gvr locally, pass handler as it is
					if sv.APIServerID == localAPIServerId {
						klog.Infof(fmt.Sprintf("Resource can be served locally, skipping proxying"))
						handler.ServeHTTP(w, req)
						return
					}
					serviceableBy = append(serviceableBy, sv.APIServerID)
				}
			}
		}

		// TODO : is this right?
		if len(serviceableBy) == 0 {
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.InternalError(w, req, errors.New("no relevant api server found for the requested gvr"))
			return
		}

		// randomly select an APIServer
		rand := rand.Intn(len(serviceableBy))
		apiserverId := serviceableBy[rand]

		// fetch APIServerIdentity Lease object for this apiserver
		lease, err := cfgCtlr.kubeclientset.CoordinationV1().Leases(metav1.NamespaceSystem).Get(req.Context(), apiserverId, metav1.GetOptions{})

		if err != nil {
			klog.ErrorS(err, "Error getting apiserver lease")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("No relevant API server found for the requested GVR: %v", gv)), s, gv, w, req)

		}

		// check if lease is expired, which means that the apiserver that registered this resource has shutdown, serve 503
		if !isLeaseExpired(lease) {
			klog.ErrorS(err, "Error getting apiserver lease")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("No relevant API server found for the requested GVR: %v", gv)), s, gv, w, req)

		}

		// finally proxy
		hostname := lease.Annotations["hostname"]
		// TODO: how to get the correct port?
		port := lease.Annotations["port"]


	})
}

func isLeaseExpired(lease *v1.Lease) bool {
	currentTime := time.Now()
	// Leases created by the apiserver lease controller should have non-nil renew time
	// and lease duration set. Leases without these fields set are invalid and should
	// be GC'ed.
	return lease.Spec.RenewTime == nil ||
			lease.Spec.LeaseDurationSeconds == nil ||
			lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second).Before(currentTime)
}

type configController struct {
	name              string // varies in tests of fighting controllers
	svInformerSynced cache.InformerSynced
	svLister         apiserverinternallister.StorageVersionLister
	leaseLister  coordlisters.LeaseLister
	kubeclientset kubernetes.Interface
}

func newTestableController(config TestableConfig) *configController {
	cfgCtlr := &configController{
		name:                   config.Name,
	}
	cfgCtlr.svLister = config.InformerFactory.Internal().V1alpha1().StorageVersions().Lister()
	return cfgCtlr
}
