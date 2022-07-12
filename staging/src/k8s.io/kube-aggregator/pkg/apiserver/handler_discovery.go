package apiserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/authentication/user"
	discoveryv1 "k8s.io/apiserver/pkg/endpoints/discovery/v1"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"
	v1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

// Given a list of APIServices and proxyHandlers for contacting them,
// DiscoveryManager caches a list of discovery documents for each server

type DiscoveryManager interface {
	APIHandlerManager

	// Spwans a worker which waits for added/updated apiservices and updates
	// the unified discovery document by contacting the aggregated api services
	Run(ctx <-chan struct{})

	// Returns a restful webservice which responds to discovery requests
	// Thread-safe
	WebService() *restful.WebService

	RefreshDocument() error
}

type discoveryManager struct {
	serializer      runtime.NegotiatedSerializer
	getProxyHandler func(apiServiceName string) http.Handler

	// Channel used to indicate that the document needs to be refreshed
	// The Run() function starts a worker thread which waits for signals on this
	// channel to refetch new discovery documents.
	dirtyChannel chan struct{}

	// Map from v1.APIService.Name to our stored discovery information about
	// that APIService
	servicesLock sync.RWMutex

	//TODO: Index by APIService.Spec.Service rather than its name. Each APIService
	// object represents a particular groupversion as opposed to a single server
	// to reach.
	services map[string]apiServiceInfo

	// Merged handler which stores all known groupversions
	mergedDiscoveryHandler discoveryv1.ResourceManager
}

type apiServiceInfo struct {
	fresh bool

	// Currently cached discovery document for this apiservice
	// a nil discovery document indicates this service needs to be re-fetched
	discovery *metav1.DiscoveryAPIGroupList

	// ETag hash of the cached discoveryDocument
	etag string
}

var _ DiscoveryManager = &discoveryManager{}

func NewDiscoveryManager(
	codecs serializer.CodecFactory,
	serializer runtime.NegotiatedSerializer,
	getProxyHandler func(string) http.Handler,
) DiscoveryManager {
	return &discoveryManager{
		serializer:             serializer,
		mergedDiscoveryHandler: discoveryv1.NewResourceManager(serializer),
		getProxyHandler:        getProxyHandler,
		services:               make(map[string]apiServiceInfo),
		dirtyChannel:           make(chan struct{}),
	}
}

func handlerWithUser(handler http.Handler, info user.Info) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req = req.WithContext(request.WithUser(req.Context(), info))
		handler.ServeHTTP(w, req)
	})
}

// Synchronously refreshes the discovery document by contacting all known
// APIServices. Waits until all respond or timeout.
func (self *discoveryManager) RefreshDocument() error {
	klog.Info("Refreshing discovery information from apiservices")

	// information needed in the below loop to update a service
	type serviceUpdateInfo struct {
		// Handler For the service which repsonse to /discovery/v1 requests
		handler http.Handler

		// ETag of the existing discovery information known about the service
		etag string
	}

	servicesToUpdate := map[string]serviceUpdateInfo{}

	// Collect all services which have no discovery document and then update them
	func() {
		self.servicesLock.RLock()
		defer self.servicesLock.RUnlock()

		for name, service := range self.services {
			if !service.fresh {
				handler := self.getProxyHandler(name)
				if handler == nil {
					klog.Error(fmt.Errorf("nil discovery handler returned for APIService %v", name))
					continue
				}
				servicesToUpdate[name] = serviceUpdateInfo{
					handler: handler,
					etag:    service.etag,
				}
			}
		}
	}()

	if servicesToUpdate == nil {
		return nil
	}

	// Download update discovery documents in parallel
	type resultItem struct {
		name      string
		discovery *metav1.DiscoveryAPIGroupList
		etag      string
		error     error
	}

	waitGroup := sync.WaitGroup{}
	results := make(chan resultItem, len(servicesToUpdate))

	for name, updateInfo := range servicesToUpdate {
		// Send a GET request to /discovery/v1 for each service that needs to
		// be updated
		waitGroup.Add(1)

		// Silence loop varaible capture warning?
		name := name
		updateInfo := updateInfo

		go func() {
			defer waitGroup.Done()

			handler := updateInfo.handler
			handler = handlerWithUser(handler, &user.DefaultInfo{Name: "system:aggregator"})
			handler = http.TimeoutHandler(handler, 5*time.Second, "request timed out")

			req, err := http.NewRequest("GET", "/discovery/v1", nil)
			if err != nil {
				// NewRequest should not fail, but if it does for some reason,
				// log it and continue
				klog.Errorf("failed to create http.Request for /discovery/v1: %v", err)
				return
			}
			req.Header.Add("Accept", "application/json")

			if updateInfo.etag != "" {
				req.Header.Add("If-None-Match", updateInfo.etag)
			}

			writer := newInMemoryResponseWriter()
			handler.ServeHTTP(writer, req)

			switch writer.respCode {
			case http.StatusNotModified:
				// Do nothing. Just keep the old entry
			case http.StatusNotFound:
				// Wipe out any data for this service
				results <- resultItem{
					name:      name,
					discovery: &metav1.DiscoveryAPIGroupList{},
					error:     errors.New("not found"),
				}
			case http.StatusOK:
				parsed := &metav1.DiscoveryAPIGroupList{}
				if err := utiljson.Unmarshal(writer.data, parsed); err != nil {
					results <- resultItem{
						name:  name,
						error: err,
					}
					return
				}

				results <- resultItem{
					name:      name,
					discovery: parsed,
					etag:      writer.Header().Get("Etag"),
				}
			default:
				results <- resultItem{
					name:  name,
					error: fmt.Errorf("unknown response code: %v", writer.respCode),
				}
			}
		}()
	}

	// For for all transfers to either finish or fail
	waitGroup.Wait()
	close(results)

	// Merge information back into services list and inform the endpoint handler
	//  of updated information
	self.servicesLock.Lock()
	defer self.servicesLock.Unlock()

	for info := range results {
		if _, exists := self.services[info.name]; exists {
			self.services[info.name] = apiServiceInfo{
				fresh:     true,
				discovery: info.discovery,
				etag:      info.etag,
			}
		} else {
			// If a service was in servicesToUpdate at the beginning of this
			// function call but not anymore, then it was removed in the meantime
			// so we just throw away this result.
		}

		// If there was an issue with fetching either apiextensions or legacy
		// types then throw an error
		//!TODO: This condition is never hit due to the addition of internal names
		if info.error != nil && (info.name == "" || info.name == "apiextensions.k8s.io") {
			return info.error
		}
	}

	// After merging all the data back together, give it to the endpoint handler
	// to respond to HTTP requests
	self.mergedDiscoveryHandler.Reset()
	for _, info := range self.services {
		self.mergedDiscoveryHandler.AddGroups(info.discovery.Groups)
	}

	return nil
}

// Spwans a goroutune which waits for added/updated apiservices and updates
// the discovery document accordingly
func (self *discoveryManager) Run(ctx <-chan struct{}) {
	klog.Info("Starting ResourceDiscoveryManager")

	// Every time the dirty channel is signalled, refresh the document
	// debounce in 1s intervals so that successive updates don't keep causing
	// a refresh
	go debounce(time.Second, self.dirtyChannel, func() {
		_ = self.RefreshDocument()
	})
}

// Adds an APIService to be tracked by the discovery manager. If the APIService
// is already known
func (self *discoveryManager) AddAPIService(apiService *v1.APIService) error {
	if apiService.Spec.Service == nil && !strings.HasPrefix(apiService.Name, "internal_handler_") {
		// Local and non-functional aggregated APIservices will have a nil service
		return nil
	}

	self.servicesLock.Lock()
	defer self.servicesLock.Unlock()

	if service, exists := self.services[apiService.Name]; exists {
		// Set the fresh flag to false
		self.services[apiService.Name] = apiServiceInfo{
			fresh:     false,
			discovery: service.discovery,
		}
	} else {
		// APIService is new to us, so start tracking it
		self.services[apiService.Name] = apiServiceInfo{}
	}

	// Kick worker thread to notice the change and update the discovery document
	select {
	case self.dirtyChannel <- struct{}{}:
		// Flagged to the channel that the object is dirty
	default:
		// Don't wait/Do nothing if the channel is already flagged
	}
	return nil
}

func (self *discoveryManager) RemoveAPIService(apiServiceName string) {
	self.servicesLock.Lock()
	defer self.servicesLock.Unlock()

	// Delete service from our database. When the worker thread runs again,
	// it will notice the service is missing and remove it from its response.
	delete(self.services, apiServiceName)

	// Kick the worker so that it remakes the discovery document. Otherwise
	// we would wait until the next added/updated apiservice to see changes
	// reflected.
	select {
	case self.dirtyChannel <- struct{}{}:
		// Flagged to the channel that the object is dirty
	default:
		// Don't wait/Do nothing if the channel is already flagged
	}
}

func (self *discoveryManager) WebService() *restful.WebService {
	return self.mergedDiscoveryHandler.WebService()
}

// Takes an input structP{} channel and quantizes the channel sends to the given
// interval
// Should be moved into util library somewhere?
func debounce(interval time.Duration, input chan struct{}, cb func()) {
	var timer *time.Timer = time.NewTimer(interval)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-input:
			timer.Reset(interval)
		case <-timer.C:
			cb()
		}
	}
}

//!TODO: This was copied from staging/src/k8s.io/kube-aggregator/pkg/controllers/openapi/aggregator/downloader.go
// which was copied from staging/src/k8s.io/kube-aggregator/pkg/controllers/openapiv3/aggregator/downloader.go
// so we should find a home for this
// inMemoryResponseWriter is a http.Writer that keep the response in memory.
type inMemoryResponseWriter struct {
	writeHeaderCalled bool
	header            http.Header
	respCode          int
	data              []byte
}

func newInMemoryResponseWriter() *inMemoryResponseWriter {
	return &inMemoryResponseWriter{header: http.Header{}}
}

func (r *inMemoryResponseWriter) Header() http.Header {
	return r.header
}

func (r *inMemoryResponseWriter) WriteHeader(code int) {
	r.writeHeaderCalled = true
	r.respCode = code
}

func (r *inMemoryResponseWriter) Write(in []byte) (int, error) {
	if !r.writeHeaderCalled {
		r.WriteHeader(http.StatusOK)
	}
	r.data = append(r.data, in...)
	return len(in), nil
}

func (r *inMemoryResponseWriter) String() string {
	s := fmt.Sprintf("ResponseCode: %d", r.respCode)
	if r.data != nil {
		s += fmt.Sprintf(", Body: %s", string(r.data))
	}
	if r.header != nil {
		s += fmt.Sprintf(", Header: %s", r.header)
	}
	return s
}
