// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gce

import (
	"fmt"
	"path"
	"time"

	log "github.com/golang/glog"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
)

type gceAPI struct {
	project        string
	computeService *compute.Service
	gceNetwork     *compute.Network
	gceInstance    *compute.Instance
}

func newAPI() (*gceAPI, error) {
	client, err := google.DefaultClient(oauth2.NoContext)
	if err != nil {
		return nil, fmt.Errorf("error creating client: %v", err)
	}

	cs, err := compute.New(client)
	if err != nil {
		return nil, fmt.Errorf("error creating compute service: %v", err)
	}

	networkName, err := networkFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting network metadata: %v", err)
	}

	prj, err := projectFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting project: %v", err)
	}

	instanceName, err := instanceNameFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting instance name: %v", err)
	}

	instanceZone, err := instanceZoneFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting instance zone: %v", err)
	}

	gn, err := cs.Networks.Get(prj, networkName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting network from compute service: %v", err)
	}

	gi, err := cs.Instances.Get(prj, instanceZone, instanceName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting instance from compute service: %v", err)
	}

	return &gceAPI{
		project:        prj,
		computeService: cs,
		gceNetwork:     gn,
		gceInstance:    gi,
	}, nil
}

func (api *gceAPI) getRoute(subnet string) (*compute.Route, error) {
	listCall := api.computeService.Routes.List(api.project)
	filter := "(name eq kubernetes-.*) "
	filter += "(description eq k8s-node-route)"
	listCall = listCall.Filter(filter)

	res, err := listCall.Do()
	if err != nil {
		return nil, err
	}

	for _, r := range res.Items {
		targetNodeName := path.Base(r.NextHopInstance)
		if targetNodeName != api.gceInstance.Name {
			log.Infof("Skipping non-matching route: %#v.", r)
		} else {
			log.Infof("Found matching route: %#v.", r)
			return r
		}
	}

	return nil, fmt.Errorf("could not find a route for %#v", api.gceInstance)

	// routeName := api.formatRouteName(subnet)
	// return api.computeService.Routes.Get(api.project, routeName).Do()
}

func (api *gceAPI) deleteRoute(subnet string) (*compute.Operation, error) {
	routeName := api.formatRouteName(subnet)
	return api.computeService.Routes.Delete(api.project, routeName).Do()
}

func (api *gceAPI) insertRoute(subnet string) (*compute.Operation, error) {
	log.Infof("Inserting route for subnet: %v", subnet)
	route := &compute.Route{
		Name:            api.formatRouteName(subnet),
		DestRange:       subnet,
		Network:         api.gceNetwork.SelfLink,
		NextHopInstance: api.gceInstance.SelfLink,
		Priority:        1000,
		Tags:            []string{},
		Description:     "k8s-node-route",
	}
	return api.computeService.Routes.Insert(api.project, route).Do()
}

func (api *gceAPI) pollOperationStatus(operationName string) error {
	for i := 0; i < 100; i++ {
		operation, err := api.computeService.GlobalOperations.Get(api.project, operationName).Do()
		if err != nil {
			return fmt.Errorf("error fetching operation status: %v", err)
		}

		if operation.Error != nil {
			return fmt.Errorf("error running operation: %v", operation.Error)
		}

		if i%5 == 0 {
			log.Infof("%v operation status: %v waiting for completion...", operation.OperationType, operation.Status)
		}

		if operation.Status == "DONE" {
			return nil
		}
		time.Sleep(time.Second)
	}

	return fmt.Errorf("timeout waiting for operation to finish")
}

func (api *gceAPI) formatRouteName(subnet string) string {
	return fmt.Sprintf("kubernetes-%v-%s", api.gceInstance.Id, replacer.Replace(subnet))
}
