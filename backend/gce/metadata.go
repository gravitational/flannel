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
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
)

func networkFromMetadata() (string, error) {
	network, err := metadataGet("/instance/network-interfaces/0/network")
	if err != nil {
		return "", err
	}
	return path.Base(network), nil
}

func projectFromMetadata() (string, error) {
	projectName, err := metadataGet("/project/project-id")
	if err != nil {
		return "", err
	}
	return path.Base(projectName), nil
}

func instanceRegionFromMetadata() (string, error) {
	zone, err := instanceZoneFromMetadata()
	if err != nil {
		return "", err
	}

	// From https://cloud.google.com/compute/docs/regions-zones:
	// >  The fully-qualified name for a zone is made up of <region>-<zone>
	//
	// Example zone: us-central1-a
	// Example region: us-central1
	zoneSlice := strings.Split(zone, "-")
	if len(zoneSlice) == 0 {
		return "", fmt.Errorf("Zone must have at least one '-': %s", zone)
	}
	return strings.Join(zoneSlice[:len(zoneSlice)-1], "-"), nil
}

func instanceZoneFromMetadata() (string, error) {
	zone, err := metadataGet("/instance/zone")

	if err != nil {
		return "", err
	}
	return path.Base(zone), nil
}

func instanceNameFromMetadata() (string, error) {
	hostname, err := metadataGet("/instance/hostname")
	if err != nil {
		return "", err
	}
	//works because we can't have . in the instance name
	return strings.SplitN(hostname, ".", 2)[0], nil
}

func metadataGet(path string) (string, error) {
	req, err := http.NewRequest("GET", metadataEndpoint+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Metadata-Flavor", "Google")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
