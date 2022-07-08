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
	AddGroupVersion(groupName string, value metav1.DiscoveryGroupVersion)
	RemoveGroup(groupName string)
	RemoveGroupVersion(gv metav1.GroupVersion)

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

	if existing, alreadyExists := self.apiGroups[groupName]; alreadyExists {
		existing.Versions = append(existing.Versions, value)
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
	ws.Doc("get available API versions")

	ws.Route(ws.GET("/").To(func(req *restful.Request, resp *restful.Response) {
		// 404?
		self.ServeHTTP(resp.ResponseWriter, req.Request)
	}).
		Doc("get available API versions").
		Operation("getDiscoveryResourcesFake").
		Produces(mediaTypes...).
		Consumes(mediaTypes...).
		Writes(metav1.DiscoveryAPIGroupList{}))

	ws.Route(ws.GET("/v1").To(func(req *restful.Request, resp *restful.Response) {
		self.ServeHTTP(resp.ResponseWriter, req.Request)
	}).
		Doc("get available API groups and their resources").
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
