package v1

import (
	"net/http"
	"sync"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

const DiscoveryEndpointRoot = "/discovery"

// This handler serves the /discovery/v1 endpoint for a given list of
// api resources indexed by their group version.
type ResourceManager interface {
	// Adds knowledge of the given groupversion to the discovery document
	// If it was already being tracked, updates the stored DiscoveryGroupVersion
	// Thread-safe
	AddGroupVersion(groupName string, value metav1.DiscoveryGroupVersion)

	// Removes all group versions for a given group
	// Thread-safe
	RemoveGroup(groupName string)

	// Removes a specfic groupversion. If all versions of a group have been
	// removed, then the entire group is unlisted.
	// Thread-safe
	RemoveGroupVersion(gv metav1.GroupVersion)

	// Returns a restful webservice which responds to discovery requests
	// Thread-safe
	WebService() *restful.WebService
}

type resourceDiscoveryManager struct {
	apiGroupsLock sync.RWMutex
	apiGroups     map[string]metav1.DiscoveryAPIGroup
	apiGroupNames []string // apiGroupNames preserves insertion order

	serializer runtime.NegotiatedSerializer
}

func NewResourceManager(serializer runtime.NegotiatedSerializer) ResourceManager {
	result := &resourceDiscoveryManager{serializer: serializer}
	return result
}

func (self *resourceDiscoveryManager) AddGroupVersion(groupName string, value metav1.DiscoveryGroupVersion) {
	self.apiGroupsLock.Lock()
	defer self.apiGroupsLock.Unlock()

	if self.apiGroups == nil {
		self.apiGroups = make(map[string]metav1.DiscoveryAPIGroup)
	}

	if existing, groupExists := self.apiGroups[groupName]; groupExists {
		// If this version already exists, replace it
		versionExists := false

		// Not very efficient, but in practice there are generally not many versions
		for i := range existing.Versions {
			if existing.Versions[i].Version == value.Version {
				existing.Versions[i] = value
				versionExists = true
				break
			}
		}

		if !versionExists {
			existing.Versions = append(existing.Versions, value)
		}

	} else {
		self.apiGroups[groupName] = metav1.DiscoveryAPIGroup{
			Name:     groupName,
			Versions: []metav1.DiscoveryGroupVersion{value},
		}
		self.apiGroupNames = append(self.apiGroupNames, groupName)
	}

}

func (self *resourceDiscoveryManager) RemoveGroupVersion(apiGroup metav1.GroupVersion) {
	self.apiGroupsLock.Lock()
	defer self.apiGroupsLock.Unlock()

	group, exists := self.apiGroups[apiGroup.Group]
	if !exists {
		return
	}

	for i := range group.Versions {
		if group.Versions[i].Version == apiGroup.Version {
			group.Versions = append(group.Versions[:i], group.Versions[i+1:]...)
			break
		}
	}

	if len(group.Versions) == 0 {
		delete(self.apiGroups, group.Name)
		for i := range self.apiGroupNames {
			if self.apiGroupNames[i] == group.Name {
				self.apiGroupNames = append(self.apiGroupNames[:i], self.apiGroupNames[i+1:]...)
				break
			}
		}
	}
}

func (self *resourceDiscoveryManager) RemoveGroup(groupName string) {
	self.apiGroupsLock.Lock()
	defer self.apiGroupsLock.Unlock()

	delete(self.apiGroups, groupName)
	for i := range self.apiGroupNames {
		if self.apiGroupNames[i] == groupName {
			self.apiGroupNames = append(self.apiGroupNames[:i], self.apiGroupNames[i+1:]...)
			break
		}
	}
}

func (self *resourceDiscoveryManager) WebService() *restful.WebService {
	mediaTypes, _ := negotiation.MediaTypesForSerializer(self.serializer)
	ws := new(restful.WebService)
	ws.Path(DiscoveryEndpointRoot)
	ws.Doc("get available API groupversions and resources")

	ws.Route(ws.GET("/v1").To(func(req *restful.Request, resp *restful.Response) {
		self.ServeHTTP(resp.ResponseWriter, req.Request)
	}).
		Doc("get available API groupversions and their resources").
		Operation("getDiscoveryResources").
		Produces(mediaTypes...).
		Consumes(mediaTypes...).
		Writes(metav1.DiscoveryAPIGroupList{}))
	return ws
}

func (self *resourceDiscoveryManager) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	self.apiGroupsLock.RLock()
	defer self.apiGroupsLock.RUnlock()

	orderedGroups := []metav1.DiscoveryAPIGroup{}
	for _, groupName := range self.apiGroupNames {
		orderedGroups = append(orderedGroups, self.apiGroups[groupName])
	}

	responsewriters.WriteObjectNegotiated(
		self.serializer,
		negotiation.DefaultEndpointRestrictions,
		schema.GroupVersion{},
		resp,
		req,
		http.StatusOK,
		&metav1.DiscoveryAPIGroupList{Groups: orderedGroups},
	)
}
