package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func APIResourcesToDiscoveryAPIResources(rsrc []metav1.APIResource) []metav1.DiscoveryAPIResource {
	res := make([]metav1.DiscoveryAPIResource, len(rsrc), len(rsrc))
	for i, v := range rsrc {
		res[i] = APIResourceToDiscoveryAPIResource(v)
	}
	return res
}

func APIResourceToDiscoveryAPIResource(rsrc metav1.APIResource) metav1.DiscoveryAPIResource {
	// Should use json tags to autogenerate this?
	return metav1.DiscoveryAPIResource{
		Name:         rsrc.Name,
		SingularName: rsrc.SingularName,
		Namespaced:   rsrc.Namespaced,
		Group:        rsrc.Group,
		Version:      rsrc.Version,
		Kind:         rsrc.Kind,
		Verbs:        rsrc.Verbs,
		ShortNames:   rsrc.ShortNames,
		Categories:   rsrc.Categories,
	}
}
