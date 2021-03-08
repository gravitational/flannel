// Copyright 2021 Splunk Inc.
//
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
// +build !windows

package gce

import (
	"context"
	"fmt"
	"os"
	"path"
	"time"

	log "github.com/golang/glog"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
)

// EnvGCENetworkProjectID is an environment variable to set the network project
// When set, network routes will be created within a network project instead of the project running the instances
const EnvGCENetworkProjectID = "GCE_NETWORK_PROJECT_ID"

// EnvKubeClusterID is an environment variable that contains the cluster name
// This variable is used as the subnetwork secondary range name
const EnvKubeClusterID = "KUBE_CLUSTER_ID"

type gceAPI struct {
	project        string
	useIPNextHop   bool
	instanceName   string
	instanceZone   string
	instanceRegion string

	computeService *compute.Service
	gceNetwork     *compute.Network
	gceInstance    *compute.Instance
}

// limit auth scope to just the required GCP API's
// https://developers.google.com/identity/protocols/oauth2/scopes
func gceScopes() []string {
	return []string{"https://www.googleapis.com/auth/compute"}
}

func newAPI() (*gceAPI, error) {
	client, err := google.DefaultClient(context.TODO(), gceScopes()...)
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

	instanceRegion, err := instanceRegionFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting instance region: %v", err)
	}

	// netPrj refers to the project which owns the network being used
	// defaults to what is read by the metadata
	netPrj := prj
	// has the network project been provided?
	if v := os.Getenv(EnvGCENetworkProjectID); v != "" {
		netPrj = v
	}

	gn, err := cs.Networks.Get(netPrj, networkName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting network from compute service: %v", err)
	}

	gi, err := cs.Instances.Get(prj, instanceZone, instanceName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting instance from compute service: %v", err)
	}

	if len(gi.NetworkInterfaces) != 1 {
		return nil, fmt.Errorf("expected 1 network interface, got %d", len(gi.NetworkInterfaces))
	}

	// if the instance project is different from the network project
	// we need to use the ip as the next hop when creating routes
	// cross project referencing is not allowed for instances
	useIPNextHop := prj != netPrj

	return &gceAPI{
		project:        netPrj,
		instanceZone:   instanceZone,
		instanceRegion: instanceRegion,
		instanceName:   instanceName,
		useIPNextHop:   useIPNextHop,
		computeService: cs,
		gceNetwork:     gn,
		gceInstance:    gi,
	}, nil
}

func (api *gceAPI) getRoute(subnet string) (*compute.Route, error) {
	routeName := formatRouteName(subnet)
	return api.computeService.Routes.Get(api.project, routeName).Do()
}

func (api *gceAPI) deleteRoute(subnet string) (*compute.Operation, error) {
	routeName := formatRouteName(subnet)
	return api.computeService.Routes.Delete(api.project, routeName).Do()
}

func (api *gceAPI) insertRoute(subnet string) (*compute.Operation, error) {
	log.Infof("Inserting route for subnet: %v", subnet)
	route := &compute.Route{
		Name:      formatRouteName(subnet),
		DestRange: subnet,
		Network:   api.gceNetwork.SelfLink,
		Priority:  1000,
		Tags:      []string{},
	}

	if api.useIPNextHop {
		if len(api.gceInstance.NetworkInterfaces) == 0 {
			return nil, fmt.Errorf("error expected instance=%v to have network interfaces",
				api.gceInstance.SelfLink)
		}

		route.NextHopIp = api.gceInstance.NetworkInterfaces[0].NetworkIP
	} else {
		route.NextHopInstance = api.gceInstance.SelfLink
	}

	return api.computeService.Routes.Insert(api.project, route).Do()
}

// refresh the held instance with the most recent information
func (api *gceAPI) refreshInstance() error {
	instance, err := api.computeService.Instances.Get(api.project, api.instanceZone, api.instanceName).Do()
	if err != nil {
		return err
	}
	api.gceInstance = instance
	return nil
}

// combine ranges by name, updating any existing entries
func combineSecondaryRanges(ranges []*compute.SubnetworkSecondaryRange, newRange *compute.SubnetworkSecondaryRange) []*compute.SubnetworkSecondaryRange {
	m := make(map[string]*compute.SubnetworkSecondaryRange)

	for i, secondaryRange := range ranges {
		m[secondaryRange.RangeName] = ranges[i]
	}

	m[newRange.RangeName] = newRange

	combined := make([]*compute.SubnetworkSecondaryRange, 0)
	for key := range m {
		combined = append(combined, m[key])
	}
	return combined
}

func (api *gceAPI) addSubnetSecondaryRange(networkCidr string, rangeName string) (*compute.Operation, error) {
	subnetworkName := path.Base(api.gceInstance.NetworkInterfaces[0].Subnetwork)
	subnetwork, err := api.computeService.Subnetworks.Get(api.project, api.instanceRegion, subnetworkName).Do()
	if err != nil {
		return nil, err
	}

	for _, secondaryIPRange := range subnetwork.SecondaryIpRanges {
		if secondaryIPRange.RangeName == rangeName && secondaryIPRange.IpCidrRange == networkCidr {
			log.Infof("Found existing secondary IP range '%s' with cidr '%s'", secondaryIPRange.RangeName, secondaryIPRange.IpCidrRange)
			return nil, nil
		}
	}

	newRange := &compute.SubnetworkSecondaryRange{
		IpCidrRange: networkCidr,
		RangeName:   rangeName,
	}

	subnetworkUpdate := &compute.Subnetwork{
		Fingerprint:       subnetwork.Fingerprint,
		SecondaryIpRanges: combineSecondaryRanges(subnetwork.SecondaryIpRanges, newRange),
	}

	log.Infof("Adding secondary range '%s' with network '%s' to subnet '%s'", rangeName, networkCidr, subnetwork.Name)
	return api.computeService.Subnetworks.Patch(api.project, api.instanceRegion, subnetwork.Name, subnetworkUpdate).Do()
}

// combine ranges by name, updating any existing entries
func combineAliasRanges(ranges []*compute.AliasIpRange, newRange *compute.AliasIpRange) []*compute.AliasIpRange {
	m := make(map[string]*compute.AliasIpRange)

	for i, aliasRange := range ranges {
		m[aliasRange.SubnetworkRangeName] = ranges[i]
	}

	m[newRange.SubnetworkRangeName] = newRange

	combined := make([]*compute.AliasIpRange, 0)
	for key := range m {
		combined = append(combined, m[key])
	}
	return combined
}

func (api *gceAPI) addAliasIPRange(subnetCidr string, rangeName string) (*compute.Operation, error) {
	err := api.refreshInstance()
	if err != nil {
		return nil, err
	}

	newRange := &compute.AliasIpRange{
		IpCidrRange:         subnetCidr,
		SubnetworkRangeName: rangeName,
	}

	networkInterface := &compute.NetworkInterface{
		Fingerprint:   api.gceInstance.NetworkInterfaces[0].Fingerprint,
		AliasIpRanges: combineAliasRanges(api.gceInstance.NetworkInterfaces[0].AliasIpRanges, newRange),
	}

	log.Infof("Adding alias cidr '%s' as part of range '%s' to instance '%s'", subnetCidr, rangeName, api.instanceName)
	operation, err := api.computeService.Instances.UpdateNetworkInterface(api.project,
		api.instanceZone,
		api.instanceName,
		api.gceInstance.NetworkInterfaces[0].Name,
		networkInterface).Do()

	if err != nil {
		return nil, err
	}
	return operation, nil
}

func (api *gceAPI) pollOperationStatus(o *compute.Operation) error {
	if o == nil || o.Status == "DONE" {
		return nil
	}

	operationName := o.Name
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

func formatRouteName(subnet string) string {
	return fmt.Sprintf("flannel-%s", replacer.Replace(subnet))
}
