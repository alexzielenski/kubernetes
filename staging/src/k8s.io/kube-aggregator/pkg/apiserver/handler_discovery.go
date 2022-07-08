package apiserver

import (
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/server/mux"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1"
)

type DiscoveryManager struct {
	codecs         serializer.CodecFactory
	lister         listers.APIServiceLister
	discoveryGroup metav1.APIGroup

	internalDelegate http.Handler
}

func (m *DiscoveryManager) InstallREST(container *mux.PathRecorderMux) error {
	// Install /discovery endpoint as a filter so that it never collides with
	// any explicitly registered endpoints
	// ...
	container.Handle("/discovery", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Respond something
		responsewriters.WriteRawJSON(200, "hello, world!", w)
	}))

	// Install /discovery/v1 handler
	container.Handle("/discovery/v1", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Temporarily forward to internal delegate for now
		m.internalDelegate.ServeHTTP(w, req)
	}))

	return nil
}
