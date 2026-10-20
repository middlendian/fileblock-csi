package kube

import "time"

type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller *bool  `json:"controller,omitempty"`
}

type Pod struct {
	Metadata struct {
		Name              string           `json:"name"`
		Namespace         string           `json:"namespace"`
		UID               string           `json:"uid"`
		DeletionTimestamp *time.Time       `json:"deletionTimestamp,omitempty"`
		OwnerReferences   []OwnerReference `json:"ownerReferences,omitempty"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		StartTime *time.Time `json:"startTime,omitempty"`
	} `json:"status"`
}

// ControllerOwner returns the owner marked controller, or nil if none is.
func (p Pod) ControllerOwner() *OwnerReference {
	for i := range p.Metadata.OwnerReferences {
		if c := p.Metadata.OwnerReferences[i].Controller; c != nil && *c {
			return &p.Metadata.OwnerReferences[i]
		}
	}
	return nil
}
