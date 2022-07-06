package apiserver

import (
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/server/mux"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1"
)

type DiscoveryManager struct {
	codecs         serializer.CodecFactory
	lister         listers.APIServiceLister
	discoveryGroup metav1.APIGroup
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
		// Example hardcoded response
		result := &metav1.DiscoveryAPIGroupList{
			Groups: []metav1.DiscoveryAPIGroup{
				{
					// Group: stable.example.com
					Name: "stable.example.com",
					Versions: []metav1.DiscoveryGroupVersion{
						{
							// Version: stable.example.com/v1
							Version: "v1",
							APIResources: []metav1.DiscoveryAPIResource{
								{
									// Resource: stable.example.com/v1.TestCRD
									Name:         "testcrds",
									SingularName: "testcrd",
									Namespaced:   false,
									Kind:         "TestCRD",
									Verbs:        []string{"patch", "update", "delete", "proxy"},
									ShortNames:   []string{"testcrd"},
									Categories:   []string{"all"},
								},
							},
						},
					},
				},
			},
		}

		// Respond something
		responsewriters.WriteObjectNegotiated(
			m.codecs,
			negotiation.DefaultEndpointRestrictions,
			schema.GroupVersion{},
			w,
			req,
			http.StatusOK,
			result,
		)
	}))

	return nil
}
