/*
Copyright 2023 The Kubernetes Authors.

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
	"sync/atomic"
	"time"

	"k8s.io/api/apiserverinternal/v1alpha1"
	v1 "k8s.io/api/coordination/v1"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/proxy"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/storageversion"
	kubeinformers "k8s.io/client-go/informers"
	apiserverinternallister "k8s.io/client-go/listers/apiserverinternal/v1alpha1"
	coordlisters "k8s.io/client-go/listers/coordination/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

const (
	aggregatedDiscoveryTimeout = 5 * time.Second
)

var (
	finishedSync atomic.Bool
)

// TestableConfig carries the parameters to an implementation that is testable
type TestableConfig struct {
	// Name of the controller
	Name string

	// InformerFactory to use in building the controller
	InformerFactory   kubeinformers.SharedInformerFactory
	CertProvider      dynamiccertificates.CertKeyContentProvider
	CaContentProvider dynamiccertificates.CAContentProvider

	StorageVersionManager storageversion.Manager
}

type uvipHandler struct {
	name        string // varies in tests of fighting controllers
	svLister    apiserverinternallister.StorageVersionLister
	leaseLister coordlisters.LeaseLister
	svi         cache.SharedIndexInformer
	leasei      cache.SharedIndexInformer
	svm         storageversion.Manager
	sai         serviceaccount.TokenGenerator
}

// New creates a new instance to implement API server proxy
func New(
	informerFactory kubeinformers.SharedInformerFactory,
	svm storageversion.Manager,
) Interface {
	return NewTestable(TestableConfig{
		Name:                  "Controller",
		InformerFactory:       informerFactory,
		StorageVersionManager: svm,
	})
}

// NewTestable is extra flexible to facilitate testing
func NewTestable(config TestableConfig) Interface {
	return newTestableController(config)
}
func newTestableController(config TestableConfig) *uvipHandler {
	cfgCtlr := &uvipHandler{
		name: config.Name,
		svm:  config.StorageVersionManager,
	}
	finishedSync.Store(false)
	svi := config.InformerFactory.Internal().V1alpha1().StorageVersions()
	leasei := config.InformerFactory.Coordination().V1().Leases()
	cfgCtlr.svi = svi.Informer()
	cfgCtlr.svLister = svi.Lister()
	cfgCtlr.leasei = leasei.Informer()
	cfgCtlr.leaseLister = leasei.Lister()
	return cfgCtlr
}

// Interface defines how the Unknown Version Proxy filter interacts with the underlying system.
type Interface interface {
	WaitForCacheSync(stopCh <-chan struct{}) error
	Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer) http.Handler
	HasFinishedSync() bool
	SetSAI(serviceaccount.TokenGenerator)
}

func (cfgCtlr *uvipHandler) SetSAI(sai serviceaccount.TokenGenerator) {
	cfgCtlr.sai = sai
}

func (cfgCtlr *uvipHandler) HasFinishedSync() bool {
	return finishedSync.Load()
}

func (cfgCtlr *uvipHandler) WaitForCacheSync(stopCh <-chan struct{}) error {

	klog.Info("uvip: Starting API Unknown Version Proxy poststarthook")
	ok := cache.WaitForNamedCacheSync("unknown-version-proxy", stopCh, cfgCtlr.svi.HasSynced, cfgCtlr.leasei.HasSynced, cfgCtlr.svm.Completed)
	if !ok {
		return fmt.Errorf("uvip: Error while waiting for initial cache sync")
	}
	klog.Infof("uvip: Setting finishedSync to true")
	finishedSync.Store(true)
	return nil
}

func (cfgHandler *uvipHandler) Handle(handler http.Handler, localAPIServerId string, s runtime.NegotiatedSerializer) http.Handler {
	if cfgHandler.svLister == nil || cfgHandler.leaseLister == nil {
		klog.Warningf("uvip: api server interoperability proxy support not found, skipping")
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {

		gv := schema.GroupVersion{Group: "unknown", Version: "unknown"}
		requestInfo, ok := apirequest.RequestInfoFrom(req.Context())

		if !ok {
			responsewriters.InternalError(w, req, errors.New("no RequestInfo found in the context"))
			return
		}

		// Allow non-resource requests
		if !requestInfo.IsResourceRequest {
			//klog.Warningf(fmt.Sprintf("Not a resource request skipping proxying"))
			handler.ServeHTTP(w, req)
			return
		}

		if !cfgHandler.HasFinishedSync() {
			klog.Warningf("uvip: informer caches not synced yet, skipping")
			handler.ServeHTTP(w, req)
			return
		}

		if (requestInfo.APIGroup == "coordination.k8s.io" && requestInfo.Resource == "leases") || (requestInfo.APIGroup == "internal.apiserver.k8s.io" && requestInfo.Resource == "storageversions") {
			handler.ServeHTTP(w, req)
			return
		}

		if requestInfo.APIGroup == "" {
			requestInfo.APIGroup = "core"
		}
		gv.Group = requestInfo.APIGroup
		gv.Version = requestInfo.APIVersion

		/*svList, err := cfgHandler.svLister.List(labels.Everything())
		if err != nil {
			klog.Warningf(fmt.Sprintf("Error retrieving All StorageVersions for the GV: %v skipping proxying: %v", gv, err))
			return
		}

		klog.Infof("uvip: All SVs : %v", svList)*/

		storageVersions, err := cfgHandler.svLister.Get(fmt.Sprintf("%s.%s", requestInfo.APIGroup, requestInfo.Resource))
		if err != nil {
			klog.Warningf(fmt.Sprintf("Error retrieving StorageVersions for the GV: %v skipping proxying: %v", gv, err))
			// TODO: confirm actions for this
			handler.ServeHTTP(w, req)
			return
		}

		if storageVersions == nil || storageVersions.Status.StorageVersions == nil || len(storageVersions.Status.StorageVersions) == 0 {
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
			klog.Infof("uvip: No relevant API server found for the requested GVR")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: No relevant API server found for the requested GVR: %v", gv))
			responsewriters.InternalError(w, req, errors.New(fmt.Sprintf("No relevant api server found for the requested gvr %v", gv)))
			return
		}

		//klog.Infof("uvip: Found %v serviceable by API servers!",len(serviceableByResp.serviceableBy))
		// randomly select an APIServer
		rand := rand.Intn(len(serviceableByResp.serviceableBy))
		apiserverId := serviceableByResp.serviceableBy[rand]

		// fetch APIServerIdentity Lease object for this apiserver
		lease, err := cfgHandler.leaseLister.Leases(metav1.NamespaceSystem).Get(apiserverId)

		if err != nil {
			klog.ErrorS(err, "uvip: Error getting apiserver lease")
			utilruntime.HandleError(fmt.Errorf("failed to serve request: Error retrieving lease for destination API server, err: %v", err))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("Error retrieving lease for destination API server for requested resource: %v,", gv)), s, gv, w, req)
			return
		}

		// check if lease is expired, which means that the apiserver that registered this resource has shutdown, serve 503
		if isLeaseExpired(lease) {
			utilruntime.HandleError(fmt.Errorf("failed to serve request: Expired lease for API server"))
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("Expired lease for API server for the requested GVR: %v", gv)), s, gv, w, req)
			return
		}

		klog.Infof("uvip: Handler started for request group: %v version: %v resource: %v", requestInfo.APIGroup, requestInfo.APIVersion, requestInfo.Resource)

		// finally proxy
		//hostname := lease.Labels[apiv1.LabelHostname]
		hostname := lease.Labels["listener-host"]
		port := lease.Labels[apiv1.PortHeader]

		// sc, pc := token.Claims(*svcacct, nil, nil, 5*time.Minute, 2*time.Minute, req.Spec.Audiences)
		// tok, err := cfgHandler.sai.GenerateToken(sc, pc)
		// if err != nil {
		// 	klog.ErrorS(err, "uvip: error generating bearer token")
		// 	responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("error generating bearer token: %v, err: %v", gv, err)), s, gv, w, req)
		// }
		err = proxyRequestToDestinationAPIServer(req, w, hostname, port, lease, "")
		if err != nil {
			klog.ErrorS(err, "uvip: Error proxying request for the requested GVR")
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable(fmt.Sprintf("Error proxying request for the requested GVR: %v, err: %v", gv, err)), s, gv, w, req)
		}

	})
}

// TODO: why do you need to find out ALL serviceableBy's? Why not just stop at the first one?
// TODO: What about retries?
func findServiceableByServers(storageVersions *v1alpha1.StorageVersion, requestInfo *apirequest.RequestInfo, localAPIServerId string) serviceableByResponse {

	var serviceableBy []string

	for _, sv := range storageVersions.Status.StorageVersions {
		for _, version := range sv.DecodableVersions {
			if len(strings.Split(version, "/")) == 1 {
				version = fmt.Sprintf("%s/%s", "core", version)
			}
			if version == fmt.Sprintf("%s/%s", requestInfo.APIGroup, requestInfo.APIVersion) {
				// found the gvr locally, pass handler as it is
				if sv.APIServerID == localAPIServerId {
					return serviceableByResponse{locallyServiceable: true}
				}
				serviceableBy = append(serviceableBy, sv.APIServerID)
			}
		}
	}
	klog.Infof("uvip: Found these api server lease objects: %v", serviceableBy)
	return serviceableByResponse{serviceableBy: serviceableBy}
}

func proxyRequestToDestinationAPIServer(req *http.Request, w http.ResponseWriter, hostname string, port string, lease *v1.Lease, tok string) error {
	user, ok := genericapirequest.UserFrom(req.Context())
	if !ok {
		return fmt.Errorf("RICHA error finding user info")
	}

	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	location.Host = fmt.Sprintf("%s:%s", hostname, port)
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	newReq, cancelFn := newRequestForProxy(location, req)
	defer cancelFn()

	if len(tok) > 0 {
		newReq.Header.Add("Authorization", "Bearer "+tok)
	}

	// create transport
	clientConfig := &restclient.Config{
		TLSClientConfig: restclient.TLSClientConfig{
			Insecure: true,
		},
	}

	proxyRoundTripper, transportBuildingError := restclient.TransportFor(clientConfig)
	if transportBuildingError != nil {
		klog.Warning(transportBuildingError.Error())
		return transportBuildingError
	}

	klog.Infof("RICHHAAA user info %v", user)
	proxyRoundTripper = transport.NewAuthProxyRoundTripper("system:anomymous", []string{"system:unauthenticated"}, nil, proxyRoundTripper)

	resp := &responder{w: w}
	handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, false, resp)

	klog.Infof("uvip: Proxying old request:  \n %v to  new request: \n %v", req, newReq)

	handler.ServeHTTP(w, newReq)
	return resp.e
}

func normalizeLocation(location *url.URL) *url.URL {
	normalized, _ := url.Parse(location.String())
	if len(normalized.Scheme) == 0 {
		normalized.Scheme = "http"
	}
	return normalized
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
	if auditID, found := audit.AuditIDFrom(req.Context()); found {
		newReq.Header.Set(auditinternal.HeaderAuditID, string(auditID))
	}
	return newReq, cancelFn
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
	e error
}

// TODO this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(w http.ResponseWriter, req *http.Request, err error) {
	klog.Errorf("Error while proxying request: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
	r.e = err
}

type serviceableByResponse struct {
	locallyServiceable bool
	serviceableBy      []string
}
