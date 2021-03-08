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

// This work borrows from the https://github.com/kelseyhightower/flannel-route-manager
// project which has the following license agreement.

// Copyright (c) 2014 Kelsey Hightower

// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
// of the Software, and to permit persons to whom the Software is furnished to do
// so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
// +build !windows

package gce

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/api/googleapi"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/pkg/ip"
	"github.com/coreos/flannel/subnet"
)

func init() {
	backend.Register("gce", New)
}

// EnvGCENetworkUseAliasIP is an environment variable that determines whether to use static routes or alias IPs
// When "true", flannel adds alias IPs to instances
// When any other value or unset, flannel inserts static routes
const EnvGCENetworkUseAliasIP = "FLANNEL_GCE_NETWORK_USE_ALIAS_IP"

var metadataEndpoint = "http://169.254.169.254/computeMetadata/v1"

var replacer = strings.NewReplacer(".", "-", "/", "-")

type GCEBackend struct {
	sm       subnet.Manager
	extIface *backend.ExternalInterface
	apiInit  sync.Once
	api      *gceAPI
}

func New(sm subnet.Manager, extIface *backend.ExternalInterface) (backend.Backend, error) {
	gb := GCEBackend{
		sm:       sm,
		extIface: extIface,
	}
	return &gb, nil
}

func (g *GCEBackend) ensureAPI() error {
	var err error
	g.apiInit.Do(func() {
		g.api, err = newAPI()
	})
	return err
}

func (g *GCEBackend) RegisterNetwork(ctx context.Context, wg *sync.WaitGroup, config *subnet.Config) (backend.Network, error) {
	attrs := subnet.LeaseAttrs{
		PublicIP: ip.FromIP(g.extIface.ExtAddr),
	}

	l, err := g.sm.AcquireLease(ctx, &attrs)
	switch err {
	case nil:

	case context.Canceled, context.DeadlineExceeded:
		return nil, err

	default:
		return nil, fmt.Errorf("failed to acquire lease: %v", err)
	}

	if err = g.ensureAPI(); err != nil {
		return nil, err
	}

	useAliasIP, err := strconv.ParseBool(os.Getenv(EnvGCENetworkUseAliasIP))
	if err != nil {
		return nil, fmt.Errorf("error parsing environment variable '%s': %v", EnvGCENetworkUseAliasIP, err)
	}

	if useAliasIP {
		return g.registerAliasIP(l, config)
	}

	return g.registerStaticRoutes(l)
}

func (g *GCEBackend) registerStaticRoutes(l *subnet.Lease) (backend.Network, error) {
	found, err := g.handleMatchingRoute(l.Subnet.String())
	if err != nil {
		return nil, fmt.Errorf("error handling matching route: %v", err)
	}

	if !found {
		operation, err := g.api.insertRoute(l.Subnet.String())
		if err != nil {
			return nil, fmt.Errorf("error inserting route: %v", err)
		}

		err = g.api.pollOperationStatus(operation)
		if err != nil {
			return nil, fmt.Errorf("insert operation failed: %v", err)
		}
	}

	return &backend.SimpleNetwork{
		SubnetLease: l,
		ExtIface:    g.extIface,
	}, nil
}

func (g *GCEBackend) registerAliasIP(l *subnet.Lease, config *subnet.Config) (backend.Network, error) {
	clusterID := strings.TrimSpace(os.Getenv(EnvKubeClusterID))
	if clusterID == "" {
		return nil, fmt.Errorf("%s environment variable should not be blank", EnvKubeClusterID)
	}

	// If any conflicts routes already exist,
	err := g.checkConflictingLocalRoute(l)
	if err != nil {
		return nil, err
	}

	// Add secondary IP range to subnet if it doesn't already exist
	operation, err := g.api.addSubnetSecondaryRange(config.Network.String(), clusterID)
	if err != nil {
		return nil, fmt.Errorf("error adding secondary range to subnet: %v", err)
	}

	err = g.api.pollOperationStatus(operation)
	if err != nil {
		return nil, fmt.Errorf("error polling subnet operation: %v", err)
	}

	// Ensure the alias IP range is assigned to the instance
	operation, err = g.api.addAliasIPRange(l.Subnet.String(), os.Getenv(EnvKubeClusterID))
	if err != nil {
		return nil, fmt.Errorf("error adding alias IP: %v", err)
	}

	err = g.api.pollOperationStatus(operation)
	if err != nil {
		return nil, fmt.Errorf("error polling alias IP operation: %v", err)
	}

	// addAliasIPRange will cause the google-guest-agent to add a conflicting local route unless it is
	// configured with ip_forwarding=false. Since this check will race with the addition of the route
	// after the alias IP is added to the instance, we wait for 10 seconds to increase the likelihood
	// that we'll detect this problem.
	log.Info("Waiting for alias IP routes to propagate")
	time.Sleep(10 * time.Second)
	err = g.checkConflictingLocalRoute(l)
	if err != nil {
		return nil, err
	}

	return &backend.SimpleNetwork{
		SubnetLease: l,
		ExtIface:    g.extIface,
	}, nil
}

// Return an error if a conflicting alias IP routes exists in the local routing table
// ip route ls table local type local $SUBNET
// Example SUBNET: 10.245.55.0/24
func (g *GCEBackend) checkConflictingLocalRoute(l *subnet.Lease) error {
	cmd := exec.Command("ip", "route", "ls", "table", "local", "type", "local", l.Subnet.String())

	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("unable to run ip command: %v", err)
	}

	if len(bytes.TrimSpace(b)) != 0 {
		log.Errorf("Conflicting route found; set [NetworkInterfaces] ip_forwarding = false in `/etc/default/instance_configs.cfg`, run `systemctl restart google-guest-agent.service`, then run `ip route del local %s`", l.Subnet.String())
		log.Error("More information: https://cloud.google.com/vpc/docs/configure-alias-ip-ranges#enabling_ip_alias_on_images_disables_cbr0_bridge_on_self-managed_kubernetes_clusters")
		return fmt.Errorf("Found incompatible route: %v", string(b))
	}
	return nil
}

//returns true if an exact matching rule is found
func (g *GCEBackend) handleMatchingRoute(subnet string) (bool, error) {
	matchingRoute, err := g.api.getRoute(subnet)
	if err != nil {
		if apiError, ok := err.(*googleapi.Error); ok {
			if apiError.Code != 404 {
				return false, fmt.Errorf("error getting the route err: %v", err)
			}
			return false, nil
		}
		return false, fmt.Errorf("error getting googleapi: %v", err)
	}

	if matchingRoute.NextHopInstance == g.api.gceInstance.SelfLink {
		log.Info("Exact pre-existing route found")
		return true, nil
	}

	log.Info("Deleting conflicting route")
	operation, err := g.api.deleteRoute(subnet)
	if err != nil {
		return false, fmt.Errorf("error deleting conflicting route : %v", err)
	}

	err = g.api.pollOperationStatus(operation)
	if err != nil {
		return false, fmt.Errorf("delete operation failed: %v", err)
	}

	return false, nil
}
