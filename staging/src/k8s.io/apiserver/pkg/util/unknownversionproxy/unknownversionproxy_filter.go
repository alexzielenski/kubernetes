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
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/api/apiserverinternal/v1alpha1"
	v1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/proxy"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kubeinformers "k8s.io/client-go/informers"
	apiserverinternallister "k8s.io/client-go/listers/apiserverinternal/v1alpha1"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)
const (
aggregatedDiscoveryTimeout = 5 * time.Second)

// TestableConfig carries the parameters to an implementation that is testable
type TestableConfig struct {
	// Name of the controller
	Name string

	// InformerFactory to use in building the controller
	InformerFactory kubeinformers.SharedInformerFactory
}

type configController struct {
	name              string // varies in tests of fighting controllers
	svLister         apiserverinternallister.StorageVersionLister
	leaseLister  coordlisters.LeaseLister
}

// New creates a new instance to implement API server proxy
func New(
	informerFactory kubeinformers.SharedInformerFactory,
) Interface {
	return newController(TestableConfig{
		Name:                   "Controller",
		InformerFactory:        informerFactory,
	})
}

func newController(config TestableConfig) *configController {
	cfgCtlr := &configController{
		name:                   config.Name,
	}
	cfgCtlr.svLister = config.InformerFactory.Internal().V1alpha1().StorageVersions().Lister()
	cfgCtlr.leaseLister = config.InformerFactory.Coordination().V1().Leases().Lister()
	return cfgCtlr
}

// Interface defines how the Unknown Version Proxy filter interacts with the underlying system.
type Interface interface {
	Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer)  http.Handler
}

func (cfgCtlr *configController) Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer) http.Handler {
	if cfgCtlr.svLister == nil || cfgCtlr.leaseLister == nil {
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
			klog.Errorf(fmt.Sprintf("Error retrieving StorageVersions for the GV: %v skipping proxying", gv))
			// TODO: confirm actions for this
			handler.ServeHTTP(w, req)
			return
		}

		if storageVersions == nil || storageVersions.Status.StorageVersions == nil || len(storageVersions.Status.StorageVersions) == 0{
			// this means that resource is an aggregated API or a CR, pass as it is
			klog.Warningf(fmt.Sprintf("No StorageVersion found for the GV: %v skipping proxying", gv))
			handler.ServeHTTP(w, req)
			return
		}

		serviceableByResp := findServiceableByServers(storageVersions, requestInfo, localAPIServerId)
		// found the gvr locally, pass handler as it is
		if serviceableByResp.locallyServiceable {
			klog.Infof(fmt.Sprintf("Resource can be served locally, skipping proxying"))
			handler.ServeHTTP(w, req)
			return
		}

		// TODO : is the response right?
		if serviceableByResp.serviceableBy == nil || len(serviceableByResp.serviceableBy) == 0 {
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.InternalError(w, req, errors.New("no relevant api server found for the requested gvr"))
			return
		}

		// randomly select an APIServer
		rand := rand.Intn(len(serviceableByResp.serviceableBy))
		apiserverId := serviceableByResp.serviceableBy[rand]

		// fetch APIServerIdentity Lease object for this apiserver
		lease, err := cfgCtlr.leaseLister.Leases(metav1.NamespaceSystem).Get(apiserverId)

		if err != nil {
			klog.ErrorS(err, "Error getting apiserver lease")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("No relevant API server found for the requested GVR: %v", gv)), s, gv, w, req)
			return
		}

		// check if lease is expired, which means that the apiserver that registered this resource has shutdown, serve 503
		if !isLeaseExpired(lease) {
			klog.ErrorS(err, "Error getting apiserver lease")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("No relevant API server found for the requested GVR: %v", gv)), s, gv, w, req)
			return
		}

		// finally proxy
		hostname := lease.Labels[apiv1.LabelHostname]
		// TODO: where to store/get port number details?
		port := lease.Annotations["port"]

		err = proxyRequestToDestinationAPIServer(req, w, hostname, port)
		if err != nil {
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("No relevant API server found for the requested GVR: %v", gv)), s, gv, w, req)
		}

	})
}

// TODO: why do you need to find out ALL serviceableBy's? Why not just stop at the first one?
// TODO: What about retries?
func findServiceableByServers(storageVersions *v1alpha1.StorageVersion, requestInfo *apirequest.RequestInfo, localAPIServerId string) serviceableByResponse {

	var serviceableBy []string

	for _, sv := range storageVersions.Status.StorageVersions {
		for _, version := range sv.DecodableVersions {
			if version == requestInfo.APIVersion {
				// found the gvr locally, pass handler as it is
				if sv.APIServerID == localAPIServerId {
					return serviceableByResponse{locallyServiceable: true}
				}
				serviceableBy = append(serviceableBy, sv.APIServerID)
			}
		}
	}
	return serviceableByResponse{serviceableBy: serviceableBy}
}

func proxyRequestToDestinationAPIServer(req *http.Request, w http.ResponseWriter, hostname string, port string) error {
	// define location
	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}

	// TODO: change to https once cert stuff is resolved
	location.Scheme = "http"

	location.Host = fmt.Sprintf("%s:%s",hostname,port)
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	newReq, cancelFn := newRequestForProxy(location, req)
	defer cancelFn()

	clientConfig := &restclient.Config{
		TLSClientConfig: restclient.TLSClientConfig{
			Insecure:   true,
			ServerName: hostname,
		},
	}

	proxyRoundTripper, transportBuildingError := restclient.TransportFor(clientConfig)
	// TODO: is it ok to assume that the request is not upgrade request?
	upgrade := false
	if transportBuildingError != nil {
		klog.Warning(transportBuildingError.Error())
	}

	// TODO: check passing responder vs upstream responsewriter
	proxyHandler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, upgrade, &responder{w: w})
	proxyHandler.ServeHTTP(w, newReq)
	return nil
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

// newRequestForProxy returns a shallow copy of the original request with a context that may include a timeout for discovery requests
func newRequestForProxy(location *url.URL, req *http.Request) (*http.Request, context.CancelFunc) {
	newCtx := req.Context()
	cancelFn := func() {}

	if requestInfo, ok := genericapirequest.RequestInfoFrom(req.Context()); ok {
		// trim leading and trailing slashes. Then "/apis/group/version" requests are for discovery, so if we have exactly three
		// segments that we are going to proxy, we have a discovery request.
		if !requestInfo.IsResourceRequest && len(strings.Split(strings.Trim(requestInfo.Path, "/"), "/")) == 3 {
			// discovery requests are used by kubectl and others to determine which resources a server has.  This is a cheap call that
			// should be fast for every aggregated apiserver.  Latency for aggregation is expected to be low (as for all extensions)
			// so forcing a short timeout here helps responsiveness of all clients.
			newCtx, cancelFn = context.WithTimeout(newCtx, aggregatedDiscoveryTimeout)
		}
	}

	// WithContext creates a shallow clone of the request with the same context.
	newReq := req.WithContext(newCtx)
	newReq.Header = utilnet.CloneHeader(req.Header)
	newReq.URL = location
	newReq.Host = location.Host

	return newReq, cancelFn
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
}

// TODO this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(_ http.ResponseWriter, _ *http.Request, err error) {
	http.Error(r.w, err.Error(), http.StatusServiceUnavailable)
}

type serviceableByResponse struct {
	locallyServiceable bool
	serviceableBy []string
}

